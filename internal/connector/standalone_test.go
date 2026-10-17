package connector

import (
	"context"
	"testing"
)

func TestLocalTargetMembershipAndManagementExclusion(t *testing.T) {
	c := dockerContainer{ID: "container"}
	c.NetworkSettings.Networks = map[string]dockerNetwork{"br0": {IPAddress: "192.0.2.20", GlobalIPv6Address: "2001:db8::20"}}
	c.Ports = []dockerPort{{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 18080, Type: "tcp"}}
	hosts := map[string]bool{"127.0.0.1": true, "192.0.2.10": true}
	ports := map[int]bool{80: true, 443: true, 8000: true}
	for _, tc := range []struct {
		url     string
		allowed bool
	}{{"http://192.0.2.20:8096", true}, {"https://[2001:db8::20]:8443", true}, {"http://127.0.0.1:18080", true}, {"http://192.0.2.99:8096", false}, {"http://169.254.169.254", false}, {"http://127.0.0.1:8000", false}, {"http://127.0.0.1:2375", false}} {
		a := localAddresses(context.Background(), c, []dockerContainer{c}, tc.url, hosts, ports)
		if (len(a) > 0) != tc.allowed {
			t.Errorf("%s: %v", tc.url, a)
		}
	}
	c.HostConfig.NetworkMode = "host"
	if len(localAddresses(context.Background(), c, nil, "http://192.0.2.10:8000", hosts, ports)) != 0 {
		t.Fatal("host networking exposed NAS management port")
	}
	if len(localAddresses(context.Background(), c, nil, "http://127.0.0.1:9000", hosts, ports)) != 1 {
		t.Fatal("host network WebUI rejected")
	}
}
