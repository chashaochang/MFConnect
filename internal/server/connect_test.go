package server

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

func connectFixture(t *testing.T) *fixture {
	f := setup(t, "")
	f.app.Config.ConnectHost = "mfconnect.example.test"
	if err := f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.PortalSupported = true
		d.LastSeen = time.Now().Unix()
		st.Devices[d.ID] = d
		st.Devices["dev_other"] = store.Device{ID: "dev_other", Owner: "alice", PortalSupported: true}
		st.Services["svc_other"] = store.Service{ID: "svc_other", DeviceID: "dev_other", Published: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f
}
func deviceConnectRequest(f *fixture, input any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(input)
	r := httptest.NewRequest("POST", "https://api.example.test/v1/device/connect", bytes.NewReader(b))
	core.Sign(r, "dev_one", f.key, b)
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	return w
}
func TestConnectIDAtomicUniquenessAndRedirect(t *testing.T) {
	f := connectFixture(t)
	for _, id := range []string{"api", "bad.name", "-bad", "bad-", "a/b", "a_1", ""} {
		w := invoke(f.app, "PUT", "api.example.test", "/v1/devices/dev_one/connect", "alice-token", map[string]any{"id": id, "enabled": true})
		if w.Code != 400 {
			t.Fatalf("accepted %q: %d", id, w.Code)
		}
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, device := range []string{"dev_one", "dev_other"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			w := invoke(f.app, "PUT", "api.example.test", "/v1/devices/"+id+"/connect", "alice-token", map[string]any{"id": " Home ", "enabled": true})
			if w.Code == 200 {
				successes.Add(1)
			} else if w.Code != 409 {
				t.Errorf("unexpected status %d", w.Code)
			}
		}(device)
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("duplicate connection ID claimed")
	}
	w := invoke(f.app, "GET", "mfconnect.example.test", "/home", "", nil)
	if w.Code != 302 || w.Header().Get("Location") != "https://home.mfconnect.example.test/" {
		t.Fatalf("redirect: %d %v", w.Code, w.Header())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("redirect cached")
	}
	for _, path := range []string{"//evil.test", "/home/extra", "/%2Fhome"} {
		if invoke(f.app, "GET", "mfconnect.example.test", path, "", nil).Code != 404 {
			t.Fatal("invalid path redirected")
		}
	}
	w = invoke(f.app, "GET", "api.example.test", "/v1/connect/resolve?id=home", "", nil)
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("interface_ips")) || bytes.Contains(w.Body.Bytes(), []byte("private_key")) {
		t.Fatal("bad public lookup")
	}
	if invoke(f.app, "PUT", "api.example.test", "/v1/devices/dev_one/connect", "bob-token", map[string]any{"id": "other", "enabled": true}).Code != 404 {
		t.Fatal("cross-owner rename")
	}
}
func TestDeviceSessionScopeAndRefresh(t *testing.T) {
	f := connectFixture(t)
	if invoke(f.app, "POST", "api.example.test", "/v1/device/connect", "", map[string]string{"action": "session"}).Code != 401 {
		t.Fatal("unsigned session issuance")
	}
	w := deviceConnectRequest(f, map[string]string{"action": "session"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var out struct {
		Token   string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	w = invoke(f.app, "GET", "api.example.test", "/v1/devices", out.Token, nil)
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("dev_other")) {
		t.Fatal("session escaped device scope")
	}
	if invoke(f.app, "GET", "api.example.test", "/v1/devices/dev_other/connect", out.Token, nil).Code != 404 {
		t.Fatal("other device accessible")
	}
	if invoke(f.app, "POST", "api.example.test", "/v1/updates/server", out.Token, map[string]string{}).Code != 403 {
		t.Fatal("NAS session gained server admin")
	}
	if invoke(f.app, "POST", "api.example.test", "/v1/enrollments", out.Token, map[string]string{}).Code != 403 {
		t.Fatal("NAS session gained enrollment admin")
	}
	w = invoke(f.app, "POST", "api.example.test", "/v1/connect/refresh", "", map[string]string{"refresh_token": out.Refresh})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if invoke(f.app, "GET", "api.example.test", "/v1/devices", out.Token, nil).Code != 401 {
		t.Fatal("rotated access token still active")
	}
	if invoke(f.app, "POST", "api.example.test", "/v1/connect/refresh", "", map[string]string{"refresh_token": out.Refresh}).Code != 401 {
		t.Fatal("refresh replay accepted")
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.Revoked = true
		st.Devices[d.ID] = d
		return nil
	})
	if invoke(f.app, "POST", "api.example.test", "/v1/connect/refresh", "", map[string]string{"refresh_token": out.Refresh}).Code != 401 {
		t.Fatal("revoked device refreshed")
	}
}
func TestConnectDisableRevokesAppsPreservesConfiguration(t *testing.T) {
	f := connectFixture(t)
	if _, code := f.app.setConnect("dev_one", "home", true); code != "" {
		t.Fatal(code)
	}
	cookie := session(t, f)
	if _, code := f.app.setConnect("dev_one", "home", false); code != "" {
		t.Fatal(code)
	}
	st := f.st.View()
	if st.Devices["dev_one"].ConnectID != "home" || !st.Services["svc_one"].Published {
		t.Fatal("configuration lost")
	}
	if _, exists := st.Sessions[core.Hash(cookie.Value)]; exists {
		t.Fatal("old browser session retained")
	}
	if invoke(f.app, "GET", "api.example.test", "/v1/connect/resolve?id=home", "", nil).Code != 404 {
		t.Fatal("disabled ID still resolves")
	}
	if invoke(f.app, "POST", "api.example.test", "/v1/services/svc_one/launches", "alice-token", map[string]string{"nonce_sha256": core.Hash(core.Random())}).Code != 403 {
		t.Fatal("disabled remote access still launched")
	}
	if _, code := f.app.setConnect("dev_one", "", false); code != "" {
		t.Fatal(code)
	}
	if f.st.View().Devices["dev_one"].ConnectID != "home" {
		t.Fatal("disable erased ID")
	}
	if _, code := f.app.setConnect("dev_one", "house", true); code != "" {
		t.Fatal(code)
	}
	if invoke(f.app, "GET", "mfconnect.example.test", "/home", "", nil).Code != 404 {
		t.Fatal("old ID still active")
	}
	if invoke(f.app, "GET", "mfconnect.example.test", "/house", "", nil).Code != http.StatusFound {
		t.Fatal("new ID unavailable")
	}
}

func TestPrivateInstallerAuthorizationAndExpiry(t *testing.T) {
	f := connectFixture(t)
	f.app.Config.PluginManifest = filepath.Join(t.TempDir(), "mfconnect.plg")
	os.WriteFile(f.app.Config.PluginManifest, []byte(`<PLUGIN name="mfconnect" version="0.5.0"><FILE Run="/bin/bash"><INLINE>echo install</INLINE></FILE></PLUGIN>`), 0600)
	f.st.Update(func(st *store.State) error {
		st.Principals["admin"] = store.Principal{ID: "admin", Admin: true, TokenHash: core.Hash("admin-token")}
		return nil
	})
	if invoke(f.app, "POST", "api.example.test", "/v1/installations", "alice-token", map[string]string{}).Code != 403 {
		t.Fatal("non-admin generated installation")
	}
	w := invoke(f.app, "POST", "api.example.test", "/v1/installations", "admin-token", map[string]string{"owner": "alice"})
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out struct {
		URL string `json:"install_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	path := strings.TrimPrefix(out.URL, "https://api.example.test")
	w = invoke(f.app, "GET", "api.example.test", path, "", nil)
	var doc struct{ XMLName xml.Name }
	if w.Code != 200 || xml.Unmarshal(w.Body.Bytes(), &doc) != nil || doc.XMLName.Local != "PLUGIN" || !strings.Contains(w.Body.String(), "mfconnectd provision") {
		t.Fatal("invalid private installer")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("invitation cacheable")
	}
	f.st.Update(func(st *store.State) error {
		for key, i := range st.Enrollments {
			i.Expires = 1
			st.Enrollments[key] = i
		}
		return nil
	})
	if invoke(f.app, "GET", "api.example.test", path, "", nil).Code != 404 {
		t.Fatal("expired installation available")
	}
}
