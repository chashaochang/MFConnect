package connector

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mfconnect/internal/core"
	"mfconnect/internal/server"
	"mfconnect/internal/store"
)

func TestNativePortalThroughPinnedGateway(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "home.mfconnect.example.test" || r.Header.Get(core.PortalProofHeader) != "" {
			t.Error("native host/proof mismatch")
		}
		if _, e := r.Cookie("nas_login"); e != nil {
			http.Redirect(w, r, "http://"+r.Host+"/login", 302)
			return
		}
		w.Write([]byte("native authenticated NAS"))
	}))
	defer backend.Close()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app, err := server.New(server.Config{APIHost: "127.0.0.1", BaseDomain: "apps.example.test", ConnectHost: "mfconnect.example.test", TunnelHost: "relay.example.test", TunnelPort: 17000, PortStart: 20000, PortEnd: 20999}, st)
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewTLSServer(app)
	defer cloud.Close()
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cloud.Certificate().Raw}), 0600)
	config := filepath.Join(dir, "connector.json")
	if err := Init(config, cloud.URL, "Test", ca); err != nil {
		t.Fatal(err)
	}
	c, err := Load(config)
	if err != nil {
		t.Fatal(err)
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c.WebGUIURL = backend.URL
	c.GatewayPort = port
	c.DeviceID = "dev_portal"
	c.TunnelToken = core.Random()
	core.WriteJSON(config, c)
	key, _ := base64.RawStdEncoding.DecodeString(c.PrivateKey)
	st.Update(func(st *store.State) error {
		st.Principals["owner"] = store.Principal{ID: "owner"}
		st.Devices[c.DeviceID] = store.Device{ID: c.DeviceID, Owner: "owner", PublicKey: base64.RawStdEncoding.EncodeToString(ed25519.PrivateKey(key).Public().(ed25519.PublicKey)), TunnelTokenHash: core.Hash(c.TunnelToken)}
		return nil
	})
	if _, err := Sync(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"action":"configure","id":"home","enabled":true}`)
	if _, err := Connect(context.Background(), c, input); err != nil {
		t.Fatal(err)
	}
	g, err := startGateway(config, c)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	st.Update(func(st *store.State) error {
		d := st.Devices[c.DeviceID]
		d.PortalPort = port
		st.Devices[d.ID] = d
		return nil
	})
	request := httptest.NewRequest("GET", "https://home.mfconnect.example.test/Main", nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, request)
	if w.Code != 302 || w.Header().Get("Location") != "https://home.mfconnect.example.test/login" {
		t.Fatalf("native login inaccessible: %d %s", w.Code, w.Body.String())
	}
	request = httptest.NewRequest("GET", "https://home.mfconnect.example.test/Main", nil)
	request.AddCookie(&http.Cookie{Name: "nas_login", Value: "native-session"})
	w = httptest.NewRecorder()
	app.ServeHTTP(w, request)
	if w.Code != 200 || w.Body.String() != "native authenticated NAS" {
		t.Fatalf("native session inaccessible: %d %s", w.Code, w.Body.String())
	}
	// Directly hitting the local gateway with a fabricated cloud proof cannot
	// reach the NAS WebGUI, even with a valid native cookie.
	request = httptest.NewRequest("GET", "https://home.mfconnect.example.test/Main", nil)
	request.Header.Set(core.PortalProofHeader, "forged")
	request.AddCookie(&http.Cookie{Name: "nas_login", Value: "native-session"})
	w = httptest.NewRecorder()
	g.ServeHTTP(w, request)
	if w.Code != 403 {
		t.Fatal("forged cloud proof accepted")
	}
	if _, err := Connect(context.Background(), c, json.RawMessage(`{"action":"configure","id":"home","enabled":false}`)); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, request)
	if w.Code != 404 {
		t.Fatal("disabled portal still open")
	}
}
