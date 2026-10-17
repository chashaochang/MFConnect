package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

func signedCall(f *fixture, device, path string, input any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(input)
	r := httptest.NewRequest("POST", "https://api.example.test"+path, bytes.NewReader(b))
	core.Sign(r, device, f.key, b)
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	return w
}

func deviceCertificate(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawStdEncoding.EncodeToString(der)
}

func TestEasyTierEnrollmentIdentityAndDeviceIsolation(t *testing.T) {
	f := setup(t, "")
	f.app.Config.EasyTier = &EasyTierConfig{PortStart: 21000, PortEnd: 21001}
	in := core.SyncRequest{Transport: "easytier", GatewayPort: 18743, GatewayCertificate: deviceCertificate(t, f.key), Targets: []core.Target{{Key: "web", Name: "Web", URL: "http://127.0.0.1:80"}}}
	cookie := session(t, f)
	first := signedCall(f, "dev_one", "/v1/device/sync", in)
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	var one, two core.SyncResponse
	if err := json.Unmarshal(first.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.st.View().Sessions[core.Hash(cookie.Value)]; ok {
		t.Fatal("transport migration retained an old session")
	}
	if err := f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.ID = "dev_other"
		d.EasyTierPort = 0
		d.EasyTierSecret = ""
		st.Devices[d.ID] = d
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	second := signedCall(f, "dev_other", "/v1/device/sync", in)
	if second.Code != 200 {
		t.Fatal(second.Code, second.Body.String())
	}
	if err := json.Unmarshal(second.Body.Bytes(), &two); err != nil {
		t.Fatal(err)
	}
	if one.EasyTier.Network == two.EasyTier.Network || one.EasyTier.Secret == two.EasyTier.Secret || one.EasyTier.Peer == two.EasyTier.Peer {
		t.Fatal("devices share network credentials or listeners")
	}
	_, foreign, _ := ed25519.GenerateKey(rand.Reader)
	in.GatewayCertificate = deviceCertificate(t, foreign)
	if w := signedCall(f, "dev_one", "/v1/device/sync", in); w.Code != 400 {
		t.Fatal("foreign TLS identity accepted", w.Code)
	}
}

func TestDeviceAccessBindsHostDeviceRevisionAndRevocation(t *testing.T) {
	f := setup(t, "")
	if err := f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.Transport = "easytier"
		st.Devices[d.ID] = d
		d.ID = "dev_other"
		st.Devices[d.ID] = d
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cookie := session(t, f)
	in := core.AccessRequest{Host: "one.apps.example.test", Session: cookie.Value}
	call := func(device string) int { return signedCall(f, device, "/v1/device/access", in).Code }
	if call("dev_one") != 200 {
		t.Fatal("authorized request denied")
	}
	if call("dev_other") != 401 {
		t.Fatal("another device used the session")
	}
	in.Host = "two.apps.example.test"
	if call("dev_one") != 401 {
		t.Fatal("another application host used the session")
	}
	in.Host = "one.apps.example.test"
	if err := f.st.Update(func(st *store.State) error {
		svc := st.Services["svc_one"]
		svc.Revision++
		st.Services[svc.ID] = svc
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if call("dev_one") != 401 {
		t.Fatal("old target revision used the session")
	}
	in.Session = session(t, f).Value
	if call("dev_one") != 200 {
		t.Fatal("new session denied")
	}
	w := invoke(f.app, "DELETE", "api.example.test", "/v1/browser-sessions", "alice-token", nil)
	if w.Code != 200 || call("dev_one") != 401 {
		t.Fatal("logout did not invalidate device authorization")
	}
	in.Session = session(t, f).Value
	w = invoke(f.app, "DELETE", "api.example.test", "/v1/devices/dev_one", "alice-token", nil)
	if w.Code != 200 || call("dev_one") != 401 {
		t.Fatal("revoked device accessed service")
	}
}
