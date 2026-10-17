package connector

import (
	"errors"
	"net"
	"testing"
)

func TestCollectInterfaceIPsIncludesPrivateAndGlobalWithoutProbing(t *testing.T) {
	interfaces := []net.Interface{{Name: "lo", Flags: net.FlagUp | net.FlagLoopback}, {Name: "down0"}, {Name: "br0", Flags: net.FlagUp}}
	queries := 0
	addresses := func(iface net.Interface) ([]net.Addr, error) {
		queries++
		if iface.Name != "br0" {
			t.Fatal("queried down or loopback interface")
		}
		var result []net.Addr
		for _, raw := range []string{"192.168.1.5/24", "2001:db8::5/64", "fe80::5/64"} {
			ip, network, _ := net.ParseCIDR(raw)
			network.IP = ip
			result = append(result, network)
		}
		return result, nil
	}
	report, err := collectInterfaceIPs(interfaces, addresses)
	if err != nil || queries != 1 || len(report.InterfaceIPs) != 2 || report.InterfaceIPs[0].IP != "192.168.1.5" || report.InterfaceIPs[1].IP != "2001:db8::5" {
		t.Fatalf("unexpected interfaces: %+v %v", report, err)
	}
	if _, err = collectInterfaceIPs(interfaces, func(net.Interface) ([]net.Addr, error) { return nil, errors.New("interface disappeared") }); err == nil {
		t.Fatal("partial collection was incorrectly reported as a complete snapshot")
	}
}
