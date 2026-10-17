package connector

import (
	"encoding/json"
	"testing"

	"mfconnect/internal/core"
)

func TestFRPConfigOnlyUsesLocallyAllowedTargets(t *testing.T) {
	c := Config{DeviceID: "dev_one", TunnelToken: "secret", CAFile: "/trusted/ca.crt", Targets: []core.Target{{Key: "web", URL: "https://192.168.1.2:444"}}}
	out := core.SyncResponse{TunnelHost: "relay.example.test", TunnelPort: 17000, Assignments: []core.Assignment{{Key: "web", ServiceID: "svc_one", RemotePort: 20000, Published: true}}}
	b, err := FRPConfig(c, out)
	if err != nil {
		t.Fatal(err)
	}
	var generated map[string]any
	json.Unmarshal(b, &generated)
	tls := generated["transport"].(map[string]any)["tls"].(map[string]any)
	if tls["trustedCaFile"] != "/trusted/ca.crt" || tls["serverName"] != "relay.example.test" {
		t.Fatal("TLS trust verification missing")
	}
	proxy := generated["proxies"].([]any)[0].(map[string]any)
	if proxy["localIP"] != "192.168.1.2" || proxy["localPort"] != float64(444) {
		t.Fatal("local target changed")
	}
	out.Assignments[0].Key = "unconfigured"
	if _, err = FRPConfig(c, out); err == nil {
		t.Fatal("controller selected arbitrary LAN target")
	}
}

func TestFRPGatewayDoesNotForwardRawApplicationPortsOrDowngrade(t *testing.T) {
	c := Config{DeviceID: "dev_one", GatewayPort: 18743, TunnelToken: "secret", CAFile: "/trusted/ca.crt", Targets: []core.Target{{Key: "web", URL: "http://192.168.1.2:8080"}}}
	out := core.SyncResponse{Transport: "frp", Gateway: true, TunnelHost: "relay.example.test", TunnelPort: 17000, Assignments: []core.Assignment{{Key: "web", ServiceID: "svc_one", RemotePort: 20000, Published: true}}}
	b, err := FRPConfig(c, out)
	if err != nil {
		t.Fatal(err)
	}
	var generated map[string]any
	if err = json.Unmarshal(b, &generated); err != nil {
		t.Fatal(err)
	}
	proxy := generated["proxies"].([]any)[0].(map[string]any)
	if proxy["localIP"] != "127.0.0.1" || proxy["localPort"] != float64(18743) {
		t.Fatal("raw application port was forwarded")
	}
	out.Gateway = false
	if _, err = FRPConfig(c, out); err == nil {
		t.Fatal("gateway downgrade accepted")
	}
}
