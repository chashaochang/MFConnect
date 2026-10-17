package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

// Session creation and refresh must agree with the endpoint selected by the
// client. In particular, an expired v1 session must not silently become v2.
func TestConnectSessionContractNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name, entry, host, prefix, header, browser string
		legacyRoot                                 bool
		want                                       int
	}{
		{"legacy-only", "", "api.example.test", "", "", "mfconnect.example.test", false, 1},
		{"legacy-only-v2", "", "api.example.test", "", "2", "mfconnect.example.test", false, 2},
		{"migrated-v1-default", "example.test", "api.example.test", "", "", "legacy.example.test", false, 1},
		{"migrated-v1-explicit", "example.test", "api.example.test", "", "1", "legacy.example.test", false, 1},
		{"migrated-v2-explicit", "example.test", "api.example.test", "", "2", "example.test", false, 2},
		{"apex-api", "example.test", "example.test", "/api", "", "example.test", false, 2},
		{"subdomain-api", "connect.example.test", "connect.example.test", "/api", "", "connect.example.test", false, 2},
		{"unified-api-cannot-downgrade", "example.test", "example.test", "/api", "1", "example.test", false, 2},
		{"same-host-legacy-root", "api.example.test", "api.example.test", "", "", "legacy.example.test", true, 1},
		{"same-host-legacy-v2", "api.example.test", "api.example.test", "", "2", "api.example.test", true, 2},
		{"same-host-unified-api", "api.example.test", "api.example.test", "/api", "", "api.example.test", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *fixture
			if tc.entry == "" {
				f = connectFixture(t)
				if _, code := f.app.setConnect("dev_one", "home", true); code != "" {
					t.Fatal(code)
				}
			} else {
				f = unified(t, "connect.example.test", "")
				config := f.app.Config
				config.EntryOrigin = "https://" + tc.entry
				config.LegacyAPIRoot = tc.legacyRoot
				var err error
				f.app, err = New(config, f.st)
				if err != nil {
					t.Fatal(err)
				}
			}
			request := func(path, header string, body any, signed bool) *httptest.ResponseRecorder {
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", "https://"+tc.host+tc.prefix+path, bytes.NewReader(data))
				if header != "" {
					r.Header.Set("X-MF-Endpoint-Contract", header)
				}
				if signed {
					core.Sign(r, "dev_one", f.key, data)
				}
				w := httptest.NewRecorder()
				f.app.ServeHTTP(w, r)
				return w
			}
			check := func(w *httptest.ResponseRecorder) map[string]any {
				t.Helper()
				if w.Code != 200 {
					t.Fatalf("session HTTP %d", w.Code)
				}
				var out map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if out["schema_version"] != float64(tc.want) {
					t.Errorf("schema_version = %v, want %d", out["schema_version"], tc.want)
				}
				if out["api_origin"] != "https://"+tc.host {
					t.Error("session API origin changed")
				}
				connect, ok := out["connect"].(map[string]any)
				if !ok || connect["origin"] != "https://home."+tc.browser {
					t.Error("session NAS origin changed without negotiation")
				}
				if tc.want == 1 {
					for _, key := range []string{"endpoint_contract", "entry_origin", "api_base_url", "canonical_api_base_url", "browser_domain", "apps_domain", "capabilities"} {
						if _, present := out[key]; present {
							t.Errorf("legacy session contains unnegotiated %s", key)
						}
					}
					if w.Header().Get("X-MF-Endpoint-Contract") != "1" {
						t.Error("legacy response header advertises the wrong selected contract")
					}
				} else {
					if out["endpoint_contract"] != float64(2) || out["api_base_url"] != "https://"+tc.host+tc.prefix || w.Header().Get("X-MF-Endpoint-Contract") != "2" {
						t.Error("negotiated v2 descriptor missing or downgraded")
					}
					caps, ok := out["capabilities"].(map[string]any)
					if !ok || caps["launch_navigation"] != float64(1) {
						t.Error("v2 launch navigation capability lost")
					}
				}
				return out
			}
			initial := check(request("/v1/device/connect", tc.header, map[string]string{"action": "session"}, true))
			access, refresh := initial["access_token"].(string), initial["refresh_token"].(string)
			if err := f.st.Update(func(st *store.State) error {
				for id, p := range st.Principals {
					if p.TokenHash == core.Hash(access) {
						p.Expires = time.Now().Unix() - 1
						st.Principals[id] = p
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if invoke(f.app, "GET", tc.host, tc.prefix+"/v1/devices", access, nil).Code != 401 {
				t.Fatal("expired access token accepted")
			}
			if request("/v1/connect/refresh", "3", map[string]string{"refresh_token": refresh}, false).Code != 400 {
				t.Fatal("unknown contract accepted")
			}
			// The rejected negotiation must not consume the refresh token.
			rotated := check(request("/v1/connect/refresh", tc.header, map[string]string{"refresh_token": refresh}, false))
			if rotated["access_token"] == access || rotated["refresh_token"] == refresh {
				t.Fatal("tokens did not rotate")
			}
			if request("/v1/connect/refresh", tc.header, map[string]string{"refresh_token": refresh}, false).Code != 401 {
				t.Fatal("consumed refresh token replayed")
			}
			for _, path := range []string{"/v1/devices", "/v1/updates/server", "/v1/devices/dev_one/updates"} {
				if w := invoke(f.app, "GET", tc.host, tc.prefix+path, rotated["access_token"].(string), nil); w.Code != 200 {
					t.Errorf("refreshed session rejected for %s: %d", path, w.Code)
				}
			}
		})
	}
}

func TestConnectResolveKeepsLegacyContractAndDiscoveryAdvertisesV2(t *testing.T) {
	f := unified(t, "example.test", "")
	for _, tc := range []struct {
		name, host, path, header string
		want                     int
	}{
		{"legacy-resolve", "api.example.test", "/v1/connect/resolve?id=home", "", 1},
		{"negotiated-resolve", "api.example.test", "/v1/connect/resolve?id=home", "2", 2},
		{"unified-resolve", "example.test", "/api/v1/connect/resolve?id=home", "", 2},
		{"legacy-discovery", "api.example.test", "/.well-known/mfconnect", "", 2},
		{"legacy-endpoints", "api.example.test", "/v1/endpoints", "", 2},
		{"unified-discovery", "example.test", "/.well-known/mfconnect", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://"+tc.host+tc.path, nil)
			if tc.header != "" {
				r.Header.Set("X-MF-Endpoint-Contract", tc.header)
			}
			w := httptest.NewRecorder()
			f.app.ServeHTTP(w, r)
			var out map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
				t.Fatalf("unexpected resolve/discovery response: HTTP %d", w.Code)
			}
			if out["schema_version"] != float64(tc.want) {
				t.Fatalf("schema_version = %v, want %d", out["schema_version"], tc.want)
			}
			if tc.want == 1 {
				if _, has := out["endpoint_contract"]; has || w.Header().Get("X-MF-Endpoint-Contract") != "1" {
					t.Fatal("legacy lookup unexpectedly selected v2")
				}
			} else if out["endpoint_contract"] != float64(2) || w.Header().Get("X-MF-Endpoint-Contract") != "2" {
				t.Fatal("v2 endpoint discovery lost its contract marker")
			}
		})
	}
}
