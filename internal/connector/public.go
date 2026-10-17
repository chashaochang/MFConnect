package connector

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/webproxy"
)

type PublicGatewayConfig struct {
	Origin      string `json:"origin"`
	Listen      string `json:"listen"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
}

func (p PublicGatewayConfig) Validate() error {
	if _, e := core.PublicIngressHost(p.Origin); e != nil {
		return e
	}
	_, port, e := net.SplitHostPort(p.Listen)
	if e != nil || port == "0" || port == "" || !filepath.IsAbs(p.Certificate) || !filepath.IsAbs(p.PrivateKey) {
		return errors.New("public gateway requires a listen address and absolute TLS certificate paths")
	}
	return nil
}
func (g *deviceGateway) startPublic(c Config) {
	if c.PublicGateway == nil {
		return
	}
	if _, e := tls.LoadX509KeyPair(c.PublicGateway.Certificate, c.PublicGateway.PrivateKey); e != nil {
		return
	}
	listener, err := net.Listen("tcp", c.PublicGateway.Listen)
	if err != nil {
		return
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		current, e := Load(g.configPath)
		if e != nil || current.PublicGateway == nil {
			return nil, errors.New("public gateway disabled")
		}
		cert, e := tls.LoadX509KeyPair(current.PublicGateway.Certificate, current.PublicGateway.PrivateKey)
		return &cert, e
	}}
	g.publicServer = &http.Server{Handler: http.HandlerFunc(g.servePublic), TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10}
	g.publicDone = make(chan struct{})
	go func() { _ = g.publicServer.Serve(tls.NewListener(listener, tlsConfig)); close(g.publicDone) }()
}
func bearerHash(r *http.Request) string {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		return core.Hash(fields[1])
	}
	return ""
}
func (g *deviceGateway) servePublic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	c, err := Load(g.configPath)
	if err != nil || c.PublicGateway == nil {
		http.Error(w, "public_gateway_unavailable", 503)
		return
	}
	base, _ := core.PublicIngressHost(c.PublicGateway.Origin)
	suffix := "." + base
	label := strings.TrimSuffix(r.Host, suffix)
	if r.TLS == nil || r.TLS.ServerName != r.Host || !strings.HasSuffix(r.Host, suffix) || strings.Contains(label, ".") || core.PublicHost(c.PublicGateway.Origin, label) != r.Host {
		http.Error(w, "unknown_host", 421)
		return
	}
	if r.URL.Path == "/_mf/public-probe" {
		nonce := r.URL.Query().Get("nonce")
		if r.Method != "GET" || len(nonce) != 43 || !core.Identifier.MatchString(nonce) {
			http.Error(w, "invalid_probe", 400)
			return
		}
		key, e := base64.RawStdEncoding.DecodeString(c.PrivateKey)
		if e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		origin := "https://" + r.Host
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(core.PublicProbe{Schema: 1, DeviceID: c.DeviceID, Origin: origin, Nonce: nonce, Signature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, core.PublicProbeMessage(c.DeviceID, origin, nonce)))})
		return
	}
	if r.URL.Path == "/_mf/bootstrap" {
		g.publicBootstrap(w, r, c)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_mf/") {
		http.NotFound(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+r.Host {
		http.Error(w, "origin_not_allowed", 403)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "same-site" || site == "cross-site" {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "cross_origin_request", 403)
			return
		}
	}
	if label == core.PublicPortalLabel(c.DeviceID) {
		g.publicPortal(w, r, c)
		return
	}
	// Application access repeats cloud authorization on every request and during
	// active streams. A forged public Host never creates a local target mapping.
	g.serveApplication(w, r, true)
}
func (g *deviceGateway) publicBootstrap(w http.ResponseWriter, r *http.Request, c Config) {
	if r.Method != "POST" || r.URL.RawQuery != "" {
		http.Error(w, "post_required", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if r.ParseForm() != nil {
		http.Error(w, "invalid_request", 400)
		return
	}
	var out struct {
		Session string `json:"session"`
		Path    string `json:"path"`
		Expires int64  `json:"expires_at"`
	}
	input := map[string]string{"action": "public_bootstrap", "host": r.Host, "ticket": r.PostForm.Get("ticket"), "nonce": r.PostForm.Get("nonce"), "source_origin": r.Header.Get("Origin")}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if request(ctx, c, "/v1/device/connect", input, &out, true) != nil || len(out.Session) < 32 || out.Expires <= time.Now().Unix() || !core.SafePath(out.Path) {
		http.Error(w, "invalid_ticket", 401)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: webproxy.SessionCookie, Value: out.Session, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(out.Expires - time.Now().Unix())})
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, out.Path, http.StatusSeeOther)
}
func (g *deviceGateway) publicPortal(w http.ResponseWriter, r *http.Request, c Config) {
	hash := bearerHash(r)
	strip := false
	check := func(ctx context.Context) bool {
		var out struct {
			Allowed bool `json:"allowed"`
			Strip   bool `json:"strip_authorization"`
		}
		e := request(ctx, c, "/v1/device/connect", map[string]string{"action": "public_portal_access", "host": r.Host, "credential_hash": hash}, &out, true)
		if out.Strip {
			strip = true
		}
		return e == nil && out.Allowed
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	first, finish := context.WithTimeout(ctx, 3*time.Second)
	ok := check(first)
	finish()
	if !ok || c.WebGUIURL == "" {
		http.Error(w, "portal_disabled", 403)
		return
	}
	if strip {
		r.Header.Del("Authorization")
	}
	transport := webproxy.Transport()
	var mu sync.Mutex
	connections := []net.Conn{}
	closeConnections := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
	defer closeConnections()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
		if e == nil {
			mu.Lock()
			connections = append(connections, conn)
			mu.Unlock()
			if ctx.Err() != nil {
				_ = conn.Close()
				return nil, ctx.Err()
			}
		}
		return conn, e
	}
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				probe, done := context.WithTimeout(ctx, 3*time.Second)
				allowed := check(probe)
				done()
				if !allowed {
					cancel()
					closeConnections()
					return
				}
			}
		}
	}()
	webproxy.ServeWebGUI(w, r.WithContext(ctx), c.WebGUIURL, r.Host, transport)
}
