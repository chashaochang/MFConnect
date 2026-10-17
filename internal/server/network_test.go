package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"mfconnect/internal/core"
)

func TestObservedIPTrustsOnlyConfiguredProxy(t *testing.T) {
	s := &Server{Config: Config{TrustedProxyIPs: []string{"127.0.0.1"}}}
	for _, tc := range []struct{ peer, header, want string }{
		{"8.8.4.4:12000", "1.1.1.1", "8.8.4.4"},
		{"127.0.0.1:12000", "1.1.1.1", "1.1.1.1"},
		{"127.0.0.1:12000", "1.1.1.1, 8.8.4.4", ""},
		{"127.0.0.1:12000", "", ""},
		{"192.168.1.1:12000", "1.1.1.1", ""},
		{"100.64.1.1:12000", "", ""},
		{"[2606:4700:4700::1111]:12000", "", "2606:4700:4700::1111"},
	} {
		r := httptest.NewRequest("POST", "https://api.example.test/v1/device/sync", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-MF-Client-IP", tc.header)
		r.Header.Set("X-Forwarded-For", "9.9.9.9")
		if got := s.observedPublicIP(r); got != tc.want {
			t.Errorf("peer %s, header %s: got %s want %s", tc.peer, tc.header, got, tc.want)
		}
	}
	r := httptest.NewRequest("POST", "https://api.example.test/v1/device/sync", nil)
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Add("X-MF-Client-IP", "1.1.1.1")
	r.Header.Add("X-MF-Client-IP", "8.8.4.4")
	if s.observedPublicIP(r) != "" {
		t.Fatal("multiple client IP headers accepted")
	}
}

func TestSignedNetworkSyncPersistsAndRemainsOwnerOnly(t *testing.T) {
	f := setup(t, "")
	sync := func(network *core.NetworkReport, peer string) int {
		b, _ := json.Marshal(core.SyncRequest{Network: network, Targets: []core.Target{}})
		r := httptest.NewRequest("POST", "https://api.example.test/v1/device/sync", bytes.NewReader(b))
		r.RemoteAddr = peer
		core.Sign(r, "dev_one", f.key, b)
		w := httptest.NewRecorder()
		f.app.ServeHTTP(w, r)
		return w.Code
	}
	report := &core.NetworkReport{InterfaceIPs: []core.InterfaceIP{{Interface: "br0", IP: "192.168.1.5"}, {Interface: "br0", IP: "2001:db8::5"}}}
	if code := sync(report, "8.8.4.4:12000"); code != 200 {
		t.Fatal("network sync rejected", code)
	}
	n := f.st.View().Devices["dev_one"].Network
	if n == nil || len(n.InterfaceIPs) != 2 || n.PublicIP != "8.8.4.4" || n.InterfacesUpdatedAt == 0 || n.PublicIPObservedAt == 0 {
		t.Fatalf("missing network information: %+v", n)
	}
	for _, token := range []string{"", "bob-token", "alice-token"} {
		w := invoke(f.app, "GET", "api.example.test", "/v1/devices", token, nil)
		if token == "" && w.Code != 401 {
			t.Fatal("anonymous IP disclosure")
		}
		if token == "bob-token" && strings.TrimSpace(w.Body.String()) != "[]" {
			t.Fatal("cross-user IP disclosure")
		}
		if token == "alice-token" && (!strings.Contains(w.Body.String(), "192.168.1.5") || strings.Contains(w.Body.String(), "tunnel_token")) {
			t.Fatal("incorrect owner response")
		}
	}
	if code := sync(nil, "1.1.1.1:12000"); code != 200 {
		t.Fatal(code)
	}
	n = f.st.View().Devices["dev_one"].Network
	if len(n.InterfaceIPs) != 2 || n.PublicIP != "1.1.1.1" {
		t.Fatal("old client erased interface snapshot or public IP did not update")
	}
	if code := sync(&core.NetworkReport{InterfaceIPs: []core.InterfaceIP{{Interface: "br0", IP: "not-an-ip"}}}, "8.8.4.4:12000"); code != 400 {
		t.Fatal("malformed address accepted")
	}
	if f.st.View().Devices["dev_one"].Network.PublicIP != "1.1.1.1" {
		t.Fatal("failed sync changed persisted IP")
	}
	if code := sync(&core.NetworkReport{}, "127.0.0.1:12000"); code != 200 {
		t.Fatal(code)
	}
	n = f.st.View().Devices["dev_one"].Network
	if len(n.InterfaceIPs) != 0 || n.PublicIP != "" || n.PublicIPObservedAt != 0 {
		t.Fatal("removed addresses remained in the snapshot")
	}
	if w := invoke(f.app, "DELETE", "api.example.test", "/v1/devices/dev_one", "alice-token", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := invoke(f.app, "GET", "api.example.test", "/v1/devices", "alice-token", nil); strings.Contains(w.Body.String(), "network") {
		t.Fatal("revoked device still exposes network snapshot")
	}
}
