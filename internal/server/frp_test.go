package server

import (
	"encoding/json"
	"strings"
	"testing"

	"mfconnect/internal/core"
)

func TestFRPGatewayMigrationPinsIdentityAndRejectsDowngrade(t *testing.T) {
	f := setup(t, "")
	oldSession := session(t, f)
	in := core.SyncRequest{Transport: "frp", GatewayPort: 18743, GatewayCertificate: deviceCertificate(t, f.key), Targets: []core.Target{{Key: "web", Name: "Web", URL: "http://127.0.0.1:80"}}}
	w := signedCall(f, "dev_one", "/v1/device/sync", in)
	var out core.SyncResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !out.Gateway || out.Transport != "frp" {
		t.Fatalf("gateway not negotiated: %d %s", w.Code, w.Body.String())
	}
	d := f.st.View().Devices["dev_one"]
	if d.GatewayPin == "" || d.GatewayPort != 18743 {
		t.Fatal("gateway identity was not persisted")
	}
	if _, exists := f.st.View().Sessions[core.Hash(oldSession.Value)]; exists {
		t.Fatal("migration retained old browser session")
	}
	if w = invoke(f.app, "PATCH", "api.example.test", "/v1/services/svc_one", "alice-token", map[string]bool{"published": true}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	cookie := session(t, f)
	if w = signedCall(f, "dev_one", "/v1/device/access", core.AccessRequest{Host: "one.apps.example.test", Session: cookie.Value}); w.Code != 200 {
		t.Fatal("FRP NAS authorization rejected", w.Code)
	}
	in.GatewayPort, in.GatewayCertificate = 0, ""
	if w = signedCall(f, "dev_one", "/v1/device/sync", in); w.Code != 409 {
		t.Fatal("gateway downgrade accepted", w.Code)
	}
	if f.st.View().Devices["dev_one"].GatewayPin != d.GatewayPin {
		t.Fatal("failed downgrade changed pin")
	}
	config := FRPServerConfig(f.app.Config)
	if config["proxyBindAddr"] != "127.0.0.1" {
		t.Fatal("FRP application ports are not loopback-only")
	}
	encoded, _ := json.Marshal(config)
	if !strings.Contains(string(encoded), `"force":true`) {
		t.Fatal("FRP TLS is not required")
	}
}
