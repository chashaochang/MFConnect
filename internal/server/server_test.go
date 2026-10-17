package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

type fixture struct {
	app *Server
	st  *store.Store
	key ed25519.PrivateKey
}

func setup(t *testing.T, backend string) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	port := 20100
	if backend != "" {
		u, _ := url.Parse(backend)
		port, _ = strconv.Atoi(u.Port())
	} else {
		backend = "http://127.0.0.1:80"
	}
	err = st.Update(func(st *store.State) error {
		st.Principals["alice"] = store.Principal{ID: "alice", TokenHash: core.Hash("alice-token")}
		st.Principals["bob"] = store.Principal{ID: "bob", TokenHash: core.Hash("bob-token")}
		st.Devices["dev_one"] = store.Device{ID: "dev_one", Owner: "alice", PublicKey: base64.RawStdEncoding.EncodeToString(pub), TunnelTokenHash: core.Hash("device-token")}
		st.Services["svc_one"] = store.Service{ID: "svc_one", DeviceID: "dev_one", HostKey: "one", Target: core.Target{Key: "web", Name: "Web", URL: backend}, RemotePort: port, Published: true, Revision: 1}
		st.Services["svc_two"] = store.Service{ID: "svc_two", DeviceID: "dev_one", HostKey: "two", Target: core.Target{Key: "other", Name: "Other", URL: backend}, RemotePort: port + 1, Published: true, Revision: 1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(Config{APIHost: "api.example.test", BaseDomain: "apps.example.test", TunnelHost: "localhost", TunnelPort: 17000, PortStart: 20000, PortEnd: 20999}, st)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{app, st, key}
}
func invoke(app http.Handler, method, host, path, token string, input any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(input)
	r := httptest.NewRequest(method, "https://"+host+path, bytes.NewReader(b))
	r.Host = host
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}
func ticket(t *testing.T, f *fixture, id string) (string, string) {
	t.Helper()
	nonce := core.Random()
	w := invoke(f.app, "POST", "api.example.test", "/v1/services/"+id+"/launches", "alice-token", map[string]string{"path": "/", "nonce_sha256": core.Hash(nonce)})
	if w.Code != 201 {
		t.Fatalf("launch: %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return out["ticket"].(string), nonce
}
func bootstrap(f *fixture, host, ticket, nonce string) *httptest.ResponseRecorder {
	form := url.Values{"ticket": {ticket}, "nonce": {nonce}}
	r := httptest.NewRequest("POST", "https://"+host+"/_mf/bootstrap", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	return w
}
func session(t *testing.T, f *fixture) *http.Cookie {
	t.Helper()
	tok, nonce := ticket(t, f, "svc_one")
	w := bootstrap(f, "one.apps.example.test", tok, nonce)
	if w.Code != 303 {
		t.Fatalf("bootstrap: %d %s", w.Code, w.Body.String())
	}
	return w.Result().Cookies()[0]
}

func TestLaunchIsolationAndAtomicConsumption(t *testing.T) {
	f := setup(t, "")
	for _, p := range []string{"//other.test/", "/%2fother.test/", "https://other.test/", "/\\evil", "/_mf/bootstrap"} {
		w := invoke(f.app, "POST", "api.example.test", "/v1/services/svc_one/launches", "alice-token", map[string]string{"path": p, "nonce_sha256": core.Hash(core.Random())})
		if w.Code != 400 {
			t.Fatalf("unsafe redirect accepted %q: %d", p, w.Code)
		}
	}
	w := invoke(f.app, "POST", "api.example.test", "/v1/services/svc_one/launches", "bob-token", map[string]string{"nonce_sha256": core.Hash(core.Random())})
	if w.Code != 403 {
		t.Fatal("cross-user access allowed")
	}
	tok, nonce := ticket(t, f, "svc_one")
	if bootstrap(f, "two.apps.example.test", tok, nonce).Code != 401 {
		t.Fatal("cross-service ticket accepted")
	}
	if bootstrap(f, "one.apps.example.test", tok, core.Random()).Code != 401 {
		t.Fatal("wrong nonce accepted")
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if bootstrap(f, "one.apps.example.test", tok, nonce).Code == 303 {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("ticket consumed %d times", successes.Load())
	}
}

func TestExpiredTicketAndCookieScope(t *testing.T) {
	f := setup(t, "")
	tok, nonce := ticket(t, f, "svc_one")
	if err := f.st.Update(func(st *store.State) error {
		v := st.Tickets[core.Hash(tok)]
		v.Expires = time.Now().Unix() - 1
		st.Tickets[core.Hash(tok)] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if bootstrap(f, "one.apps.example.test", tok, nonce).Code != 401 {
		t.Fatal("expired ticket accepted")
	}
	c := session(t, f)
	if !c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe cookie %+v", c)
	}
	r := httptest.NewRequest("GET", "https://two.apps.example.test/", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("cookie reused for another service")
	}
}

func TestProxyStripsGatewayCredentialsAndScopesCookies(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), "mf_session") || r.Header.Get("X-MF-Device") != "" {
			t.Error("gateway credentials reached upstream")
		}
		if c, err := r.Cookie("app_session"); err != nil || c.Value != "app" {
			t.Error("application cookie lost")
		}
		if r.Header.Get("Authorization") != "Bearer app-token" {
			t.Error("application auth lost")
		}
		w.Header().Add("Set-Cookie", "app_session=ok; Domain=.example.test; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", sessionCookie+"=malicious; Path=/")
		w.Header().Set("Location", "http://"+r.Host+"/after")
		w.WriteHeader(302)
	}))
	defer backend.Close()
	f := setup(t, backend.URL)
	c := session(t, f)
	r := httptest.NewRequest("GET", "https://one.apps.example.test/", nil)
	r.AddCookie(c)
	r.AddCookie(&http.Cookie{Name: "app_session", Value: "app"})
	r.Header.Set("Authorization", "Bearer app-token")
	r.Header.Set("X-MF-Device", "untrusted")
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 302 {
		t.Fatalf("proxy: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Location") != "https://one.apps.example.test/after" {
		t.Fatal("redirect not mapped")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Domain != "" || !cookies[0].Secure {
		t.Fatal("upstream cookies escaped service origin")
	}
	r = httptest.NewRequest("POST", "https://one.apps.example.test/", nil)
	r.AddCookie(c)
	r.Header.Set("Origin", "https://two.apps.example.test")
	w = httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("sibling origin accepted")
	}
}

func TestSignedSyncRejectsReplayAndTampering(t *testing.T) {
	f := setup(t, "")
	input, _ := json.Marshal(core.SyncRequest{Targets: []core.Target{{Key: "web", Name: "Web", URL: "http://127.0.0.1:80"}}})
	r := httptest.NewRequest("POST", "https://api.example.test/v1/device/sync", bytes.NewReader(input))
	core.Sign(r, "dev_one", f.key, input)
	replay := r.Clone(r.Context())
	replay.Body = io.NopCloser(bytes.NewReader(input))
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("signed sync %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	f.app.ServeHTTP(w, replay)
	if w.Code != 401 {
		t.Fatal("signature replay accepted")
	}
	r = httptest.NewRequest("POST", "https://api.example.test/v1/device/sync", strings.NewReader(`{"targets":[]}`))
	core.Sign(r, "dev_one", f.key, input)
	w = httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("modified signed body accepted")
	}
}

func TestFRPHookUsesRealRPCEnvelopeAndDeviceServiceBoundary(t *testing.T) {
	f := setup(t, "")
	call := func(op string, content any) bool {
		b, _ := json.Marshal(map[string]any{"version": "0.1.0", "op": op, "content": content})
		r := httptest.NewRequest("POST", "http://localhost/frp?op="+op, bytes.NewReader(b))
		w := httptest.NewRecorder()
		f.app.Hook(w, r)
		if w.Code != 200 {
			t.Fatalf("hook envelope: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			Reject bool `json:"reject"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return !out.Reject
	}
	if !call("Login", map[string]any{"user": "dev_one", "metas": map[string]string{"mf_token": "device-token"}}) {
		t.Fatal("valid login denied")
	}
	if call("Login", map[string]any{"user": "dev_one", "metas": map[string]string{"mf_token": "wrong"}}) {
		t.Fatal("bad token accepted")
	}
	user := map[string]any{"user": "dev_one", "metas": map[string]string{"mf_token": "device-token"}}
	good := map[string]any{"user": user, "proxy_name": "dev_one.svc_one", "proxy_type": "tcp", "remote_port": 20100}
	if !call("NewProxy", good) {
		t.Fatal("assigned proxy denied")
	}
	good["remote_port"] = 20105
	if call("NewProxy", good) {
		t.Fatal("unassigned port accepted")
	}
	good["remote_port"] = 20100
	good["group"] = "shared"
	if call("NewProxy", good) {
		t.Fatal("proxy group bypass accepted")
	}
}

func TestRevocationClosesActiveConnectionsAndRejectsCookie(t *testing.T) {
	f := setup(t, "")
	cookie := session(t, f)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	f.app.connections[core.Hash(cookie.Value)] = map[net.Conn]struct{}{a: {}}
	w := invoke(f.app, "PATCH", "api.example.test", "/v1/services/svc_one", "alice-token", map[string]bool{"published": false})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := b.Read(buf); err != io.EOF {
		t.Fatalf("active connection not closed: %v", err)
	}
	r := httptest.NewRequest("GET", "https://one.apps.example.test/", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("unpublished service accessible")
	}
}

func TestEnrollmentProofAndOneTimeUse(t *testing.T) {
	f := setup(t, "")
	token := core.Random()
	f.st.Update(func(st *store.State) error {
		st.Enrollments[core.Hash(token)] = store.Enrollment{Owner: "alice", Expires: time.Now().Add(time.Minute).Unix()}
		return nil
	})
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	proof := ed25519.Sign(key, []byte("mfconnect-enroll\n"+token+"\nNAS"))
	input := map[string]string{"enrollment_token": token, "name": "NAS", "public_key": base64.RawStdEncoding.EncodeToString(pub), "proof": base64.RawStdEncoding.EncodeToString(proof)}
	if w := invoke(f.app, "POST", "api.example.test", "/v1/devices/enroll", "", input); w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	if w := invoke(f.app, "POST", "api.example.test", "/v1/devices/enroll", "", input); w.Code != 401 {
		t.Fatal("enrollment replay accepted")
	}
}
