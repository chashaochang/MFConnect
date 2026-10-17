package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/webproxy"
)

func defaultWebGUIURL() string {
	if _, err := os.Stat("/etc/unraid-version"); err != nil {
		return ""
	}
	port := 80
	if b, err := os.ReadFile("/boot/config/ident.cfg"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(key) == "PORT" {
				n, e := strconv.Atoi(strings.Trim(strings.TrimSpace(value), "\"'"))
				if e == nil && n > 0 && n <= 65535 {
					port = n
				}
			}
		}
	}
	return "http://127.0.0.1:" + strconv.Itoa(port)
}
func validWebGUIURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	if u.Port() != "" {
		n, e := strconv.Atoi(u.Port())
		if e != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

// Connect exposes only the fixed device-management API to the local native
// WebGUI adapter. Neither its request nor response is written to process logs.
func Connect(ctx context.Context, c Config, input json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	err := request(ctx, c, "/v1/device/connect", input, &out, true)
	return out, err
}

func Provision(ctx context.Context, path, server, token string) error {
	return provision(ctx, path, server, token, false)
}
func ProvisionEntry(ctx context.Context, path, entry, token string) error {
	return provision(ctx, path, entry, token, true)
}
func provision(ctx context.Context, path, server, token string, unified bool) error {
	if err := validateServer(server); err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		ca := ""
		for _, candidate := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem", "/etc/pki/tls/certs/ca-bundle.crt"} {
			if _, e := os.Stat(candidate); e == nil {
				ca = candidate
				break
			}
		}
		if ca == "" {
			return errors.New("trusted system CA bundle not found")
		}
		init := Init
		if unified {
			init = InitEntry
		}
		if err := init(path, server, "Unraid", ca); err != nil {
			return err
		}
	}
	c, err := Load(path)
	if err != nil {
		return err
	}
	expectedBase := strings.TrimRight(server, "/")
	if unified {
		expectedBase += "/api"
	}
	if c.Server != strings.TrimRight(server, "/") || c.APIBase() != expectedBase {
		return errors.New("existing NAS belongs to another controller; configuration preserved")
	}
	if c.DeviceID != "" {
		return nil
	}
	return Enroll(ctx, path, token)
}
func (g *deviceGateway) servePortal(w http.ResponseWriter, r *http.Request) {
	c, err := Load(g.configPath)
	if err != nil || c.WebGUIURL == "" || !core.ValidPortalProof(r, c.DeviceID, core.Hash(c.TunnelToken), time.Now()) {
		http.Error(w, "invalid_portal_proof", 403)
		return
	}
	proof := r.Header.Get(core.PortalProofHeader)
	g.mu.Lock()
	if g.portalNonces == nil {
		g.portalNonces = map[string]int64{}
	}
	now := time.Now().Unix()
	for nonce, expiry := range g.portalNonces {
		if expiry < now {
			delete(g.portalNonces, nonce)
		}
	}
	_, replayed := g.portalNonces[proof]
	full := len(g.portalNonces) >= 20000
	if !replayed && !full {
		g.portalNonces[proof] = now + 61
	}
	g.mu.Unlock()
	if replayed || full {
		http.Error(w, "invalid_portal_proof", 403)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_mf/") || strings.ContainsAny(r.Host, "/: ") {
		http.Error(w, "not_found", 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	var result struct {
		Allowed bool `json:"allowed"`
	}
	err = request(ctx, c, "/v1/device/connect", map[string]string{"action": "portal_access", "host": r.Host}, &result, true)
	cancel()
	if err != nil || !result.Allowed {
		http.Error(w, "portal_unavailable", 503)
		return
	}
	transport := webproxy.Transport()
	webproxy.ServeWebGUI(w, r, c.WebGUIURL, r.Host, transport)
}
