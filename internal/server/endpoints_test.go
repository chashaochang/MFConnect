package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/pki"
	"mfconnect/internal/store"
)

func unified(t *testing.T, host, backend string) *fixture {
	t.Helper()
	f := setup(t, backend)
	c := f.app.Config
	c.EntryOrigin, c.ConnectHost = "https://"+host, "legacy.example.test"
	var err error
	f.app, err = New(c, f.st)
	if err != nil {
		t.Fatal(err)
	}
	err = f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.ConnectID, d.ConnectEnabled, d.PortalSupported = "home", true, true
		st.Devices[d.ID] = d
		st.Devices["dev_two"] = store.Device{ID: "dev_two", Owner: "bob", ConnectID: "office", ConnectEnabled: true, PortalSupported: true}
		st.Services["svc_three"] = store.Service{ID: "svc_three", DeviceID: "dev_two", HostKey: "three", Published: true, Revision: 1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUnifiedRoutesAndLegacyContract(t *testing.T) {
	for _, host := range []string{"example.test", "connect.example.test"} {
		t.Run(host, func(t *testing.T) {
			f := unified(t, host, "")
			for _, p := range []string{"/.well-known/mfconnect", "/api/v1/endpoints"} {
				w := invoke(f.app, "GET", host, p, "", nil)
				var out map[string]any
				json.Unmarshal(w.Body.Bytes(), &out)
				if w.Code != 200 || out["api_base_url"] != "https://"+host+"/api" || out["api_origin"] != "https://"+host || out["apps_domain"] != host {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			for _, domain := range []string{host, "legacy.example.test"} {
				for _, id := range []string{"home", "office"} {
					w := invoke(f.app, "GET", domain, "/"+id, "", nil)
					if w.Code != 302 || w.Header().Get("Location") != "https://"+id+"."+domain+"/" {
						t.Fatal("NAS entry", w.Code, w.Header())
					}
					if invoke(f.app, "GET", id+"."+domain, "/", "", nil).Code != 503 {
						t.Fatal("NAS route was not selected")
					}
				}
			}
			for _, p := range []string{"/api//v1/devices", "/api/v1/../healthz", "/%61pi/healthz", "/api%2fv1/devices"} {
				if w := invoke(f.app, "GET", host, p, "alice-token", nil); w.Code != 400 {
					t.Fatal("ambiguous path", p, w.Code)
				}
			}
			for _, p := range []string{"/v1/services", "/home/extra", "/apix"} {
				if w := invoke(f.app, "GET", host, p, "alice-token", nil); w.Code != 404 {
					t.Fatal("unexpected route", p, w.Code)
				}
			}
			for _, domain := range []string{host, "apps.example.test"} {
				for _, label := range []string{"one", "two", "three"} {
					if invoke(f.app, "GET", label+"."+domain, "/api/v1/devices", "alice-token", nil).Code != 401 {
						t.Fatal("app must require browser session")
					}
				}
			}
			for _, h := range []string{"one.home." + host, "one." + host + ".evil.test", "unknown." + host} {
				if invoke(f.app, "GET", h, "/", "", nil).Code != 404 {
					t.Fatal("foreign or nested host routed")
				}
			}
			for _, test := range []struct {
				host, path, domain string
				contract           bool
			}{
				{host, "/api/v1/services", host, false},
				{"api.example.test", "/v1/services", "apps.example.test", false},
				{"api.example.test", "/v1/services", host, true},
			} {
				r := httptest.NewRequest("GET", "https://"+test.host+test.path, nil)
				r.Header.Set("Authorization", "Bearer alice-token")
				if test.contract {
					r.Header.Set("X-MF-Endpoint-Contract", "2")
				}
				w := httptest.NewRecorder()
				f.app.ServeHTTP(w, r)
				var apps []map[string]any
				json.Unmarshal(w.Body.Bytes(), &apps)
				if w.Code != 200 || len(apps) != 2 {
					t.Fatal(w.Code, w.Body.String())
				}
				for _, a := range apps {
					if !strings.HasSuffix(a["origin"].(string), "."+test.domain) {
						t.Fatal(a)
					}
				}
			}
			if w := invoke(f.app, "GET", host, "/api/v1/updates", "", nil); w.Code != 401 {
				t.Fatal("API auth bypass")
			}
		})
	}
}

func TestUnifiedSignedURIAndNamespace(t *testing.T) {
	f := unified(t, "example.test", "")
	input := []byte(`{"action":"session"}`)
	r := httptest.NewRequest("POST", "https://example.test/api/v1/device/connect?request=test", bytes.NewReader(input))
	core.Sign(r, "dev_one", f.key, input)
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out["api_base_url"] != "https://example.test/api" {
		t.Fatal(w.Code, w.Body.String())
	}
	replay := r.Clone(context.Background())
	replay.Body = io.NopCloser(bytes.NewReader(input))
	w = httptest.NewRecorder()
	f.app.ServeHTTP(w, replay)
	if w.Code != 401 {
		t.Fatal("signature replay")
	}
	r = httptest.NewRequest("POST", "https://api.example.test/v1/device/connect", bytes.NewReader(input))
	core.Sign(r, "dev_one", f.key, input)
	r.Host = "example.test"
	r.TLS.ServerName = "example.test"
	r.URL.Path = "/api/v1/device/connect"
	w = httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("prefix was not signed")
	}
	for _, id := range []string{"api", "one", "office"} {
		if _, code := f.app.setConnect("dev_one", id, true); code == "" {
			t.Fatal("namespace collision", id)
		}
	}
	st := f.st.View()
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		key := newApplicationHost(&st)
		if seen[key] || core.ValidConnectID(key) || !dnsLabel.MatchString(key) {
			t.Fatal("invalid app namespace", key)
		}
		seen[key] = true
		st.Services[key] = store.Service{ID: key, HostKey: key}
	}
	c := f.app.Config
	d := st.Devices["dev_one"]
	d.ConnectID = "one"
	st.Devices[d.ID] = d
	if f.app.checkHostCollisions(st) == nil {
		t.Fatal("migration did not detect existing collision")
	}
	c.EntryOrigin = "https://example.test/api"
	if _, err := New(c, f.st); err == nil {
		t.Fatal("API base accepted as origin")
	}
}

func TestUnifiedInstallAndPureConfiguration(t *testing.T) {
	f := setup(t, "")
	c := Config{EntryOrigin: "https://connect.example.test", PortStart: 20000, PortEnd: 20999, InstallMode: "self_service", PluginManifest: filepath.Join(t.TempDir(), "plugin.plg")}
	os.WriteFile(c.PluginManifest, []byte(`<PLUGIN name="mfconnect"></PLUGIN>`), 0600)
	var err error
	f.app, err = New(c, f.st)
	if err != nil {
		t.Fatal(err)
	}
	if f.app.Config.APIHost != "connect.example.test" || f.app.Config.TunnelHost != "connect.example.test" {
		t.Fatal("single entry not expanded")
	}
	if w := invoke(f.app, "POST", "connect.example.test", "/api/v1/installations/claim", "", map[string]string{"request_id": core.Random()}); w.Code != 503 || !strings.Contains(w.Body.String(), "installer_contract_2_required") {
		t.Fatal("old plugin offered as unified installer", w.Code)
	}
	if err := os.WriteFile(c.PluginManifest, []byte(`<PLUGIN name="mfconnect">`+pluginEndpointMarker+`</PLUGIN>`), 0600); err != nil {
		t.Fatal(err)
	}
	w := invoke(f.app, "POST", "connect.example.test", "/api/v1/installations/claim", "", map[string]string{"request_id": core.Random()})
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	u, _ := url.Parse(out["install_url"].(string))
	if u.Host != "connect.example.test" || !strings.HasPrefix(u.Path, "/api/v1/install/") {
		t.Fatal(u)
	}
	w = invoke(f.app, "GET", u.Host, u.Path, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "--entry 'https://connect.example.test'") {
		t.Fatal("installer provision mode", w.Code, w.Body.String())
	}
	if invoke(f.app, "GET", u.Host, "/v1/services", "alice-token", nil).Code != 404 {
		t.Fatal("pure unified silently exposes legacy API")
	}
}

// Exercise real TLS (private CA), SNI, a generic HTTP application, redirects and
// a bidirectional upgraded stream. No public DNS or ACME issuance is simulated.
func TestUnifiedTLSProxyAndWebSocket(t *testing.T) {
	for _, host := range []string{"example.test", "connect.example.test"} {
		t.Run(host, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer alice-token" || strings.Contains(r.Header.Get("Cookie"), "__Host-mf_") {
					t.Error("control credential reached application")
				}
				if r.URL.Path == "/ws" {
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
					rw.Flush()
					line, _ := rw.ReadString('\n')
					rw.WriteString("echo:" + line)
					rw.Flush()
					return
				}
				if r.URL.Path == "/relative" {
					w.Header().Set("Location", "login?next=home")
					w.WriteHeader(302)
					return
				}
				if r.URL.Path == "/absolute" {
					w.Header().Set("Location", "http://"+r.Host+"/login")
					w.WriteHeader(302)
					return
				}
				http.SetCookie(w, &http.Cookie{Name: "app", Value: "ok", Domain: "example.test"})
				w.Write([]byte("generic WebUI"))
			}))
			defer backend.Close()
			f := unified(t, host, backend.URL)
			dir := t.TempDir()
			if err := pki.Development(dir, []string{host, "*." + host, "api.example.test", "*.apps.example.test", "legacy.example.test", "*.legacy.example.test"}); err != nil {
				t.Fatal(err)
			}
			cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
			if err != nil {
				t.Fatal(err)
			}
			pem, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(pem)
			cloud := httptest.NewUnstartedServer(f.app)
			cloud.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			cloud.StartTLS()
			defer cloud.Close()
			addr := cloud.Listener.Addr().String()
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			}}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			do := func(method, target, body string, headers map[string]string) *http.Response {
				t.Helper()
				r, _ := http.NewRequest(method, target, strings.NewReader(body))
				for k, v := range headers {
					if k == "Host" {
						r.Host = v
					} else {
						r.Header.Set(k, v)
					}
				}
				resp, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				return resp
			}
			resp := do("GET", "https://"+host+"/api/healthz", "", nil)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
			resp = do("GET", "https://"+host+"/api/healthz", "", map[string]string{"Host": "one." + host})
			resp.Body.Close()
			if resp.StatusCode != 421 {
				t.Fatal("SNI mismatch accepted")
			}
			resp = do("GET", "https://"+host+"/api/v1/devices", "", map[string]string{"Origin": "https://one." + host, "Authorization": "Bearer alice-token"})
			resp.Body.Close()
			if resp.StatusCode != 403 {
				t.Fatal("app origin accessed control API")
			}
			nonce := core.Random()
			body, _ := json.Marshal(map[string]string{"path": "/", "nonce_sha256": core.Hash(nonce)})
			resp = do("POST", "https://"+host+"/api/v1/services/svc_one/launches", string(body), map[string]string{"Authorization": "Bearer alice-token", "Content-Type": "application/json"})
			var launch map[string]any
			json.NewDecoder(resp.Body).Decode(&launch)
			resp.Body.Close()
			if resp.StatusCode != 201 || launch["origin"] != "https://one."+host {
				t.Fatal(launch)
			}
			form := url.Values{"ticket": {launch["ticket"].(string)}, "nonce": {nonce}}
			resp = do("POST", "https://one."+host+"/_mf/bootstrap", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
			resp.Body.Close()
			if resp.StatusCode != 303 {
				t.Fatal(resp.StatusCode)
			}
			cookie := resp.Cookies()[0]
			if cookie.Domain != "" || !cookie.Secure || !cookie.HttpOnly {
				t.Fatal("unsafe cookie")
			}
			headers := map[string]string{"Cookie": cookie.String(), "Authorization": "Bearer alice-token"}
			resp = do("GET", "https://one."+host+"/", "", headers)
			payload, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(payload) != "generic WebUI" || resp.Cookies()[0].Domain != "" {
				t.Fatal(resp.StatusCode, string(payload))
			}
			for _, alias := range []string{"two." + host, "three." + host, "one.apps.example.test"} {
				resp = do("GET", "https://"+alias+"/", "", headers)
				resp.Body.Close()
				if resp.StatusCode != 401 {
					t.Fatal("cookie crossed origin", alias, resp.StatusCode)
				}
			}
			for path, location := range map[string]string{"/relative": "login?next=home", "/absolute": "https://one." + host + "/login"} {
				resp = do("GET", "https://one."+host+path, "", headers)
				resp.Body.Close()
				if resp.StatusCode != 302 || resp.Header.Get("Location") != location {
					t.Fatal("redirect", resp.Header)
				}
			}
			conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: "one." + host})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: one.%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: https://one.%s\r\nCookie: %s\r\n\r\n", host, host, cookie.String())
			rw := bufio.NewReader(conn)
			ws, err := http.ReadResponse(rw, &http.Request{Method: "GET"})
			if err != nil || ws.StatusCode != 101 {
				t.Fatal("upgrade failed", err)
			}
			fmt.Fprint(conn, "ping\n")
			if line, err := rw.ReadString('\n'); err != nil || line != "echo:ping\n" {
				t.Fatal("bidirectional upgrade", line, err)
			}
			// A wildcard for D deliberately cannot authenticate <app>.<ID>.D.
			bad, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: "one.home." + host})
			if err == nil {
				bad.Close()
				t.Fatal("nested wildcard accepted")
			}
		})
	}
}

func TestLegacyUnboundCookieCannotMoveToNewHost(t *testing.T) {
	f := unified(t, "example.test", "")
	if err := f.st.Update(func(st *store.State) error {
		st.Sessions[core.Hash("legacy-cookie")] = store.Session{UserID: "alice", ServiceID: "svc_one", Revision: 1, Expires: time.Now().Unix() + 60}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://one.example.test/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "legacy-cookie"})
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("legacy cookie moved to unified host")
	}
	if !f.app.sessionHost(f.st.View().Sessions[core.Hash("legacy-cookie")], f.st.View().Services["svc_one"], "one.apps.example.test") {
		t.Fatal("legacy cookie was broken")
	}
}

func TestReuseOldAPIHostRequiresExplicitRootCompatibility(t *testing.T) {
	f := setup(t, "")
	c := f.app.Config
	c.EntryOrigin = "https://api.example.test"
	c.ConnectHost = "browser.example.test"
	if _, err := New(c, f.st); err == nil {
		t.Fatal("would silently remove old API root")
	}
	c.LegacyAPIRoot = true
	var err error
	f.app, err = New(c, f.st)
	if err != nil {
		t.Fatal(err)
	}
	for path, domain := range map[string]string{"/v1/services": "apps.example.test", "/api/v1/services": "api.example.test"} {
		w := invoke(f.app, "GET", "api.example.test", path, "alice-token", nil)
		var out []map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 || len(out) != 2 {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, svc := range out {
			if !strings.HasSuffix(svc["origin"].(string), "."+domain) {
				t.Fatal("wrong compatibility origin", svc)
			}
		}
	}
	w := invoke(f.app, "GET", "api.example.test", "/v1/endpoints", "", nil)
	var info map[string]any
	json.Unmarshal(w.Body.Bytes(), &info)
	if info["api_base_url"] != "https://api.example.test" {
		t.Fatal("active legacy base changed")
	}
}

func TestUnifiedDockerMappingAcrossNASAndRebuild(t *testing.T) {
	f := unified(t, "example.test", "")
	if err := f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_two"]
		d.PublicKey = st.Devices["dev_one"].PublicKey
		st.Devices[d.ID] = d
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sync := func(device, container string) {
		t.Helper()
		target := core.Target{Key: "docker_web", Name: "Generic Docker UI", URL: "http://192.168.1.80:8080"}
		report := &core.DockerReport{Schema: 1, State: "ready", ScannedAt: time.Now().Unix(), Applications: []core.DockerApplication{{ID: "app_local_identity", ContainerID: container, Name: "Generic Docker UI", IdentityKind: "unraid_template", State: "ready", Running: true, TargetKey: target.Key, TargetURL: target.URL, EntryPath: "/web/"}}}
		b, _ := json.Marshal(core.SyncRequest{Transport: "frp", Targets: []core.Target{target}, Docker: report})
		r := httptest.NewRequest("POST", "https://example.test/api/v1/device/sync", bytes.NewReader(b))
		core.Sign(r, device, f.key, b)
		w := httptest.NewRecorder()
		f.app.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	origins, ids := map[string]string{}, map[string]string{}
	for _, device := range []string{"dev_one", "dev_two"} {
		token := "alice-token"
		if device == "dev_two" {
			token = "bob-token"
		}
		for phase := 0; phase < 2; phase++ {
			sync(device, strings.Repeat(fmt.Sprint(phase+1), 64))
			w := invoke(f.app, "GET", "example.test", "/api/v1/devices/"+device+"/docker-apps", token, nil)
			var out struct {
				Apps []struct {
					ID        string `json:"application_id"`
					Service   string `json:"service_id"`
					Origin    string `json:"origin"`
					Published bool   `json:"published"`
				} `json:"applications"`
			}
			json.Unmarshal(w.Body.Bytes(), &out)
			if w.Code != 200 || len(out.Apps) != 1 {
				t.Fatal(w.Code, w.Body.String())
			}
			a := out.Apps[0]
			if a.ID != "app_local_identity" || !strings.HasPrefix(a.Origin, "https://app-") || !strings.HasSuffix(a.Origin, ".example.test") || a.Published {
				t.Fatal(a)
			}
			if phase == 0 {
				origins[device], ids[device] = a.Origin, a.Service
			} else if origins[device] != a.Origin || ids[device] != a.Service {
				t.Fatal("container rebuild changed stable relay mapping")
			}
		}
	}
	if origins["dev_one"] == origins["dev_two"] || ids["dev_one"] == ids["dev_two"] {
		t.Fatal("same application key on two NAS collided")
	}
	if invoke(f.app, "GET", "example.test", "/api/v1/devices/dev_two/docker-apps", "alice-token", nil).Code != 404 {
		t.Fatal("cross-NAS inventory exposed")
	}
}
