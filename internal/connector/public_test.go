package connector

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
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
	"mfconnect/internal/server"
	"mfconnect/internal/store"
)

func TestPublicGatewayAuthenticatedDirectAndDisable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || strings.Contains(r.Header.Get("Cookie"), "__Host-mf_") {
			t.Error("control credential reached application")
		}
		if r.URL.Path == "/native" && !strings.Contains(r.Header.Get("Cookie"), "nas_login=") {
			http.Error(w, "native login required", 401)
			return
		}
		w.Header().Add("Set-Cookie", "app_cookie=ok; Domain=internal.test")
		w.Write([]byte("direct application bytes"))
	}))
	defer backend.Close()
	dir := t.TempDir()
	st, e := store.Open(filepath.Join(dir, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	app, e := server.New(server.Config{APIHost: "127.0.0.1", BaseDomain: "apps.example.test", ConnectHost: "connect.example.test", TunnelHost: "relay.example.test", PortStart: 20000, PortEnd: 20999}, st)
	if e != nil {
		t.Fatal(e)
	}
	cloud := httptest.NewTLSServer(app)
	defer cloud.Close()
	ca := filepath.Join(dir, "cloud-ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cloud.Certificate().Raw}), 0600)
	config := filepath.Join(dir, "connector.json")
	if e = Init(config, cloud.URL, "Test", ca); e != nil {
		t.Fatal(e)
	}
	c, e := Load(config)
	if e != nil {
		t.Fatal(e)
	}
	c.DeviceID = "dev_direct"
	c.TunnelToken = core.Random()
	c.WebGUIURL = backend.URL
	c.Targets = []core.Target{{Key: "web", URL: backend.URL}}
	c.PublicGateway = &PublicGatewayConfig{Origin: "https://direct.example.net", Listen: "127.0.0.1:18744", Certificate: filepath.Join(dir, "server.crt"), PrivateKey: filepath.Join(dir, "server.key")}
	if e = pki.Development(dir, []string{"*.direct.example.net"}); e != nil {
		t.Fatal(e)
	}
	core.WriteJSON(config, c)
	key, _ := base64.RawStdEncoding.DecodeString(c.PrivateKey)
	now := time.Now().Unix()
	nonce, ticket := core.Random(), core.Random()
	appHost := "app-test.direct.example.net"
	st.Update(func(st *store.State) error {
		st.Principals["owner"] = store.Principal{ID: "owner", TokenHash: core.Hash("control-secret")}
		st.Devices[c.DeviceID] = store.Device{ID: c.DeviceID, Owner: "owner", PublicKey: base64.RawStdEncoding.EncodeToString(ed25519.PrivateKey(key).Public().(ed25519.PublicKey)), TunnelTokenHash: core.Hash(c.TunnelToken), GatewayPin: strings.Repeat("a", 64), ConnectID: "home", ConnectEnabled: true, PortalSupported: true, BrowserPublicDirect: true, PublicIngress: c.PublicGateway.Origin, LastSeen: now}
		st.Services["svc_web"] = store.Service{ID: "svc_web", DeviceID: c.DeviceID, HostKey: "app-test", Target: c.Targets[0], Published: true, Revision: 1}
		st.Tickets[core.Hash(ticket)] = store.Ticket{SourceOrigin: "https://app-test.apps.example.test", Host: appHost, UserID: "owner", ServiceID: "svc_web", Revision: 1, Path: "/web/", NonceHash: core.Hash(nonce), Expires: now + 30}
		return nil
	})
	g := &deviceGateway{configPath: config, connections: map[net.Conn]struct{}{}}
	cert, e := tls.LoadX509KeyPair(c.PublicGateway.Certificate, c.PublicGateway.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	direct := httptest.NewUnstartedServer(http.HandlerFunc(g.servePublic))
	direct.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	direct.StartTLS()
	defer direct.Close()
	roots := x509.NewCertPool()
	bytes, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	roots.AppendCertsFromPEM(bytes)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, direct.Listener.Addr().String())
	}}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	send := func(method, host, path string, form url.Values, cookie *http.Cookie) *http.Response {
		t.Helper()
		raw := ""
		if form != nil {
			raw = form.Encode()
		}
		r, _ := http.NewRequest(method, "https://"+host+path, strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if form != nil {
			r.Header.Set("Origin", "https://app-test.apps.example.test")
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		r.Header.Set("Authorization", "Bearer control-secret")
		if method == "GET" {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")
		}
		response, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		return response
	}
	response := send("GET", appHost, "/", nil, nil)
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("anonymous app access")
	}
	form := url.Values{"ticket": {ticket}, "nonce": {nonce}}
	response = send("POST", appHost, "/_mf/bootstrap", form, nil)
	response.Body.Close()
	if response.StatusCode != 303 || response.Header.Get("Location") != "/web/" || len(response.Cookies()) != 1 {
		t.Fatal("handoff failed", response.StatusCode)
	}
	cookie := response.Cookies()[0]
	if cookie.Domain != "" || !cookie.Secure || !cookie.HttpOnly {
		t.Fatal("cookie isolation")
	}
	response = send("GET", appHost, "/web/", nil, cookie)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "direct application bytes" || response.Cookies()[0].Domain != "" {
		t.Fatal("direct proxy failed", response.StatusCode, string(body))
	}
	response = send("POST", appHost, "/_mf/bootstrap", form, nil)
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("replay succeeded")
	}
	portal := core.PublicHost(c.PublicGateway.Origin, core.PublicPortalLabel(c.DeviceID))
	response = send("GET", portal, "/native", nil, nil)
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("native login bypass")
	}
	response = send("GET", portal, "/native", nil, &http.Cookie{Name: "nas_login", Value: "native-cookie"})
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("native login failed")
	}
	st.Update(func(st *store.State) error {
		d := st.Devices[c.DeviceID]
		d.BrowserPublicDirect = false
		st.Devices[d.ID] = d
		return nil
	})
	response = send("GET", appHost, "/web/", nil, cookie)
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("public app switch ignored")
	}
	response = send("GET", portal, "/native", nil, &http.Cookie{Name: "nas_login", Value: "native-cookie"})
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("public portal switch ignored")
	}
}

func TestAutomaticDockerProbeChecksNASReachability(t *testing.T) {
	for _, status := range []int{200, 401, 403, 405, 500} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "HEAD" {
				t.Error("mutating probe")
			}
			w.WriteHeader(status)
		}))
		cache := dockerCache{Targets: []core.Target{{Key: "web", URL: upstream.URL}}, Report: core.DockerReport{Applications: []core.DockerApplication{{TargetKey: "web", State: "ready", EntryPath: "/ui/"}}}}
		probeDockerTargets(context.Background(), &cache)
		upstream.Close()
		if (cache.Report.Applications[0].State == "ready") != (status < 500) {
			t.Fatal("health mismatch", status)
		}
		cache.Report.Applications[0].State = "ready"
		probeDockerTargets(context.Background(), &cache)
		if cache.Report.Applications[0].State != "unreachable" {
			t.Fatal("unreachable WebUI auto-enabled")
		}
	}
}
