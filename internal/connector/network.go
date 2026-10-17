package connector

import (
	"net"
	"net/netip"

	"mfconnect/internal/core"
)

func networkReport() (*core.NetworkReport, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	return collectInterfaceIPs(interfaces, func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() })
}

func collectInterfaceIPs(interfaces []net.Interface, addresses func(net.Interface) ([]net.Addr, error)) (*core.NetworkReport, error) {
	report := core.NetworkReport{InterfaceIPs: []core.InterfaceIP{}}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := addresses(iface)
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			prefix, err := netip.ParsePrefix(addr.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if !ip.IsGlobalUnicast() || ip.IsLoopback() {
				continue
			}
			report.InterfaceIPs = append(report.InterfaceIPs, core.InterfaceIP{Interface: iface.Name, IP: ip.String()})
		}
	}
	normalized, err := report.Normalize()
	return &normalized, err
}
