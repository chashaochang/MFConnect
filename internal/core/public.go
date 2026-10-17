package core

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var publicDNSLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// A separately administered HTTPS wildcard ingress, never a discovered IP plus
// an application port. Browser credentials require distinct hosts, not paths.
func PublicIngressHost(origin string) (string, error) {
	u, e := url.Parse(origin)
	if e != nil || u.Scheme != "https" || u.Host != u.Hostname() || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || len(u.Host) > 210 || !strings.Contains(u.Host, ".") || net.ParseIP(u.Host) != nil {
		return "", errors.New("public ingress must be a DNS HTTPS origin on port 443")
	}
	for _, label := range strings.Split(u.Host, ".") {
		if !publicDNSLabel.MatchString(label) {
			return "", errors.New("invalid public ingress hostname")
		}
	}
	return u.Host, nil
}
func PublicHost(ingress, label string) string {
	host, err := PublicIngressHost(ingress)
	if err != nil || !publicDNSLabel.MatchString(label) {
		return ""
	}
	return label + "." + host
}
func PublicPortalLabel(device string) string { return "nas-" + Hash(device)[:32] }
func PublicProbeMessage(device, origin, nonce string) []byte {
	return []byte("mfconnect-public-v1\n" + device + "\n" + origin + "\n" + nonce)
}

type PublicProbe struct {
	Schema    int    `json:"schema_version"`
	DeviceID  string `json:"device_id"`
	Origin    string `json:"origin"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}
