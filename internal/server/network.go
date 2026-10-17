package server

import (
	"net"
	"net/http"
	"net/netip"
)

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// observedPublicIP records the source of an authenticated device sync. It does
// not probe the NAS, infer port mappings, or select a path for a mobile client.
func (s *Server) observedPublicIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return ""
	}
	ip = ip.Unmap()
	for _, proxy := range s.Config.TrustedProxyIPs {
		trusted, err := netip.ParseAddr(proxy)
		if err == nil && ip == trusted.Unmap() {
			// The configured proxy must overwrite this header with its socket
			// peer address. Never accept X-Forwarded-For from arbitrary clients.
			values := r.Header.Values("X-MF-Client-IP")
			if len(values) != 1 {
				return ""
			}
			ip, err = netip.ParseAddr(values[0])
			if err != nil || ip.Zone() != "" {
				return ""
			}
			ip = ip.Unmap()
			break
		}
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || sharedAddressSpace.Contains(ip) {
		return ""
	}
	return ip.String()
}
