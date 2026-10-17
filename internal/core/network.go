package core

import (
	"errors"
	"net/netip"
	"sort"
	"strings"
)

const MaxInterfaceIPs = 256

// NetworkReport contains host interface addresses, not tunnel addresses or
// assertions about whether another device can reach them.
type NetworkReport struct {
	InterfaceIPs []InterfaceIP `json:"interface_ips"`
}

type InterfaceIP struct {
	Interface string `json:"interface"`
	IP        string `json:"ip"`
}

type DeviceNetwork struct {
	InterfaceIPs        []InterfaceIP `json:"interface_ips"`
	InterfacesUpdatedAt int64         `json:"interfaces_updated_at"`
	PublicIP            string        `json:"public_ip,omitempty"`
	PublicIPObservedAt  int64         `json:"public_ip_observed_at,omitempty"`
}

func (n NetworkReport) Normalize() (NetworkReport, error) {
	if len(n.InterfaceIPs) > MaxInterfaceIPs {
		return NetworkReport{}, errors.New("too many interface IPs")
	}
	out := NetworkReport{InterfaceIPs: []InterfaceIP{}}
	seen := map[InterfaceIP]bool{}
	for _, entry := range n.InterfaceIPs {
		if entry.Interface == "" || len(entry.Interface) > 64 || strings.ContainsAny(entry.Interface, "/\\") {
			return NetworkReport{}, errors.New("invalid interface name")
		}
		for _, ch := range entry.Interface {
			if ch <= ' ' || ch >= 127 {
				return NetworkReport{}, errors.New("invalid interface name")
			}
		}
		ip, err := netip.ParseAddr(entry.IP)
		if err != nil || ip.Zone() != "" || !ip.Unmap().IsGlobalUnicast() || ip.Unmap().IsLoopback() {
			return NetworkReport{}, errors.New("invalid interface IP")
		}
		entry.IP = ip.Unmap().String()
		if !seen[entry] {
			seen[entry] = true
			out.InterfaceIPs = append(out.InterfaceIPs, entry)
		}
	}
	sort.Slice(out.InterfaceIPs, func(i, j int) bool {
		a, b := out.InterfaceIPs[i], out.InterfaceIPs[j]
		if a.Interface != b.Interface {
			return a.Interface < b.Interface
		}
		return a.IP < b.IP
	})
	return out, nil
}
