package core

import "testing"

func TestNetworkReportRejectsInvalidAddressesAndNormalizes(t *testing.T) {
	report, err := (NetworkReport{InterfaceIPs: []InterfaceIP{
		{Interface: "eth0", IP: "::ffff:192.168.1.10"},
		{Interface: "eth0", IP: "192.168.1.10"},
		{Interface: "br0", IP: "2001:db8::10"},
	}}).Normalize()
	if err != nil || len(report.InterfaceIPs) != 2 || report.InterfaceIPs[0].Interface != "br0" || report.InterfaceIPs[1].IP != "192.168.1.10" {
		t.Fatalf("normalization: %+v %v", report, err)
	}
	for _, ip := range []string{"example.com", "192.168.1.10:80", "192.168.1.10/24", "127.0.0.1", "::1", "0.0.0.0", "ff02::1", "fe80::1%eth0"} {
		if _, err := (NetworkReport{InterfaceIPs: []InterfaceIP{{Interface: "eth0", IP: ip}}}).Normalize(); err == nil {
			t.Errorf("invalid address accepted: %s", ip)
		}
	}
	if _, err := (NetworkReport{InterfaceIPs: make([]InterfaceIP, MaxInterfaceIPs+1)}).Normalize(); err == nil {
		t.Fatal("unbounded interface list accepted")
	}
}
