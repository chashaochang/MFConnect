package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

func TestWebAccessSettingsReadbackLegacyAndDisable(t *testing.T) {
	f := connectFixture(t)
	for _, domain := range []string{"example.test", "connect.example.test"} {
		f.app.Config.EntryOrigin = "https://" + domain
		path := "/api/v1/devices/dev_one/connect"
		initial := invoke(f.app, "GET", domain, path, "alice-token", nil)
		var info core.ConnectInfo
		json.Unmarshal(initial.Body.Bytes(), &info)
		if info.BrowserPublicDirect || info.WebAccess.Mode != "relay_only" {
			t.Fatal(initial.Body.String())
		}
		for _, enabled := range []bool{true, false} {
			w := invoke(f.app, "PUT", domain, path, "alice-token", map[string]any{"id": "home", "enabled": true, "browser_public_direct": enabled})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			json.Unmarshal(w.Body.Bytes(), &info)
			if info.BrowserPublicDirect != enabled {
				t.Fatal("save mismatch")
			}
			// Old clients omit the new field; their ID edits preserve the preference.
			w = invoke(f.app, "PUT", "api.example.test", "/v1/devices/dev_one/connect", "alice-token", map[string]any{"id": "home", "enabled": true})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			w = invoke(f.app, "GET", domain, path, "alice-token", nil)
			json.Unmarshal(w.Body.Bytes(), &info)
			if info.BrowserPublicDirect != enabled {
				t.Fatal("legacy client reset preference")
			}
		}
	}
	// An explicit off before choosing an ID also closes legacy manual services.
	f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.ConnectID = ""
		st.Devices[d.ID] = d
		return nil
	})
	w := deviceConnectRequest(f, map[string]any{"action": "configure", "enabled": false})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = invoke(f.app, "POST", "api.example.test", "/v1/services/svc_one/launches", "alice-token", map[string]any{"nonce_sha256": core.Hash(core.Random())})
	if w.Code != 403 {
		t.Fatal("disabled manual service reachable")
	}
}
func TestPublicProbeIdentityTLSRedirectAndPrivateDNS(t *testing.T) {
	f := setup(t, "")
	d := f.st.View().Devices["dev_one"]
	host := "app.direct.example.test"
	bad := false
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := r.URL.Query().Get("nonce")
		message := core.PublicProbeMessage(d.ID, "https://"+host, nonce)
		if bad {
			message = []byte("wrong device")
		}
		json.NewEncoder(w).Encode(core.PublicProbe{Schema: 1, DeviceID: d.ID, Origin: "https://" + host, Nonce: nonce, Signature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(f.key, message))})
	}))
	defer endpoint.Close()
	// Controlled transport redirects the socket, retaining verified TLS to the
	// test CA. Production publicTransport has no such injection/configuration.
	client := endpoint.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, endpoint.Listener.Addr().String())
	}
	client.Transport = transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if !probePublic(context.Background(), client, d, host) {
		t.Fatal("valid identity probe rejected")
	}
	bad = true
	if probePublic(context.Background(), client, d, host) {
		t.Fatal("wrong device accepted")
	}
	if probePublic(context.Background(), &http.Client{}, d, "127.0.0.1") {
		t.Fatal("bad TLS accepted")
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "100.64.1.1", "169.254.169.254", "0.1.2.3", "240.1.1.1", "::1", "fc00::1", "fe80::1", "2001:db8::1"} {
		if publicAddress(netip.MustParseAddr(raw)) {
			t.Fatal("private probe destination", raw)
		}
	}
	if _, e := publicTransport(context.Background(), "localhost"); e == nil {
		t.Fatal("private DNS accepted")
	}
	for _, raw := range []string{"https://example.test:8443", "https://api.example.test", "https://example.test", "https://apps.example.test/path", "https://localhost", "http://direct.example.net"} {
		if f.app.validPublicIngress(raw) {
			t.Fatal("bad ingress", raw)
		}
	}
	if !f.app.validPublicIngress("https://direct.example.net") {
		t.Fatal("valid ingress rejected")
	}
}
func TestPublicHandoffIsHostDeviceOriginAndSwitchBound(t *testing.T) {
	f := connectFixture(t)
	now := time.Now().Unix()
	f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.ConnectID = "home"
		d.ConnectEnabled = true
		d.BrowserPublicDirect = true
		d.PublicIngress = "https://direct.example.net"
		d.GatewayPin = strings.Repeat("a", 64)
		st.Devices[d.ID] = d
		return nil
	})
	host := "one.direct.example.net"
	nonce := core.Random()
	token := core.Random()
	f.st.Update(func(st *store.State) error {
		st.Tickets[core.Hash(token)] = store.Ticket{Host: host, SourceOrigin: "https://one.apps.example.test", ServiceID: "svc_one", Revision: 1, UserID: "alice", Path: "/web/?a=1", NonceHash: core.Hash(nonce), Expires: now + 30}
		return nil
	})
	input := map[string]string{"action": "public_bootstrap", "host": host, "ticket": token, "nonce": nonce, "source_origin": "https://evil.example.net"}
	if deviceConnectRequest(f, input).Code != 401 {
		t.Fatal("wrong origin consumed ticket")
	}
	input["source_origin"] = "https://one.apps.example.test"
	w := deviceConnectRequest(f, input)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var out struct {
		Session string `json:"session"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if deviceConnectRequest(f, input).Code != 401 {
		t.Fatal("replayed handoff")
	}
	access := func(host string) int {
		return signedCall(f, "dev_one", "/v1/device/access", core.AccessRequest{Host: host, Session: out.Session}).Code
	}
	if access(host) != 200 || access("two.direct.example.net") != 401 || access("one.apps.example.test") != 401 {
		t.Fatal("direct session crossed host")
	}
	off := false
	if _, e := f.app.setConnect("dev_one", "home", true, &off); e != "" {
		t.Fatal(e)
	}
	if access(host) != 401 {
		t.Fatal("public switch had no effect")
	}
	on := true
	f.app.setConnect("dev_one", "home", true, &on)
	if access(host) != 401 {
		t.Fatal("revoked direct session revived")
	}
}

func TestLaunchNavigationContractVerifiedExactOriginsAndSingleUse(t *testing.T) {
	f := connectFixture(t)
	f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.ConnectEnabled = true
		d.ConnectID = "home"
		d.BrowserPublicDirect = true
		d.PublicIngress = "https://direct.example.net"
		st.Devices[d.ID] = d
		return nil
	})
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := r.URL.Query().Get("nonce")
		origin := "https://" + r.Host
		json.NewEncoder(w).Encode(core.PublicProbe{Schema: 1, DeviceID: "dev_one", Origin: origin, Nonce: nonce, Signature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(f.key, core.PublicProbeMessage("dev_one", origin, nonce)))})
	}))
	defer endpoint.Close()
	f.app.publicTransportFactory = func(ctx context.Context, host string) (*http.Transport, error) {
		tr := endpoint.Client().Transport.(*http.Transport).Clone()
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		tr.TLSClientConfig.ServerName = "example.com"
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, endpoint.Listener.Addr().String())
		}
		return tr, nil
	}
	nonce := core.Random()
	for _, contract := range []int{0, 1} {
		w := invoke(f.app, "POST", "api.example.test", "/v1/services/svc_one/launches", "alice-token", map[string]any{"path": "/web/", "nonce_sha256": core.Hash(nonce), "navigation_contract": contract})
		if w.Code != 201 {
			t.Fatal(w.Body.String())
		}
		var out struct {
			Ticket   string   `json:"ticket"`
			Origins  []string `json:"navigation_origins"`
			Public   *string  `json:"public_gateway_origin"`
			Deadline int64    `json:"navigation_expires_at"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		if contract == 0 && (len(out.Origins) != 0 || out.Public != nil) {
			t.Fatal("legacy launch acquired navigation")
		}
		if contract == 1 && (len(out.Origins) != 2 || out.Origins[0] != "https://one.apps.example.test" || out.Public == nil || *out.Public != "https://one.direct.example.net" || out.Deadline < time.Now().Unix()) {
			t.Fatal(w.Body.String())
		}
		request := httptest.NewRequest("POST", "https://one.apps.example.test/_mf/bootstrap", strings.NewReader("ticket="+out.Ticket+"&nonce="+nonce))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		result := httptest.NewRecorder()
		f.app.ServeHTTP(result, request)
		if result.Code != 303 {
			t.Fatal(result.Body.String())
		}
		cookie := result.Result().Cookies()[0]
		sessionKey := core.Hash(cookie.Value)
		session := f.st.View().Sessions[sessionKey]
		request = httptest.NewRequest("GET", "https://one.apps.example.test/web/", nil)
		request.AddCookie(cookie)
		result = httptest.NewRecorder()
		state := f.st.View()
		selected := f.app.maybePublicApp(result, request, state.Devices["dev_one"], state.Services["svc_one"], session, sessionKey)
		if selected != (contract == 1) {
			t.Fatal("navigation permission not enforced")
		}
		if contract == 1 {
			if result.Header().Get("Referrer-Policy") != "strict-origin" || !strings.Contains(result.Body.String(), "one.direct.example.net/_mf/bootstrap") || strings.Contains(result.Body.String(), "two.direct.example.net") {
				t.Fatal("wrong application route")
			}
			if f.app.maybePublicApp(httptest.NewRecorder(), request, state.Devices["dev_one"], state.Services["svc_one"], session, sessionKey) {
				t.Fatal("navigation grant replayed")
			}
			for _, ticket := range f.st.View().Tickets {
				if ticket.SourceOrigin != "" && ticket.Expires > out.Deadline {
					t.Fatal("handoff extended navigation deadline")
				}
			}
		}
	}
	endpoint.Close()
	w := invoke(f.app, "POST", "api.example.test", "/v1/services/svc_one/launches", "alice-token", map[string]any{"nonce_sha256": core.Hash(nonce), "navigation_contract": 1})
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 201 || out["public_gateway_origin"] != nil || len(out["navigation_origins"].([]any)) != 1 {
		t.Fatal("probe failure did not fall back", w.Body.String())
	}
}
