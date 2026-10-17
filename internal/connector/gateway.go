package connector

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/webproxy"
)

func gatewayCertificate(c Config) (tls.Certificate, string, error) {
	key, err := base64.RawStdEncoding.DecodeString(c.PrivateKey)
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return tls.Certificate{}, "", errors.New("invalid device private key")
	}
	// Stable certificate from the enrollment key; the controller verifies that
	// the advertised certificate has exactly this registered public key.
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mfconnect-device"}, DNSNames: []string{"mfconnect-device"}, NotBefore: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2125, 1, 1, 0, 0, 0, 0, time.UTC), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, ed25519.PrivateKey(key).Public(), ed25519.PrivateKey(key))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: ed25519.PrivateKey(key)}, base64.RawStdEncoding.EncodeToString(der), nil
}

type deviceGateway struct {
	configPath   string
	server       *http.Server
	done         chan struct{}
	mu           sync.Mutex
	connections  map[net.Conn]struct{}
	portalNonces map[string]int64
}

var errLocalAccess = errors.New("service is not locally approved")

func startGateway(configPath string, c Config) (*deviceGateway, error) {
	certificate, _, err := gatewayCertificate(c)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.GatewayPort)))
	if err != nil {
		return nil, err
	}
	g := &deviceGateway{configPath: configPath, done: make(chan struct{}), connections: map[net.Conn]struct{}{}}
	g.server = &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}}
	go func() { _ = g.server.Serve(tls.NewListener(l, g.server.TLSConfig)); close(g.done) }()
	return g, nil
}

func (g *deviceGateway) Close() {
	if g == nil {
		return
	}
	_ = g.server.Close()
	g.mu.Lock()
	for c := range g.connections {
		_ = c.Close()
	}
	g.mu.Unlock()
	<-g.done
}

func authorize(ctx context.Context, c Config, host, token string) (core.AccessResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var out core.AccessResponse
	err := request(ctx, c, "/v1/device/access", core.AccessRequest{Host: host, Session: token}, &out, true)
	if err != nil {
		return out, err
	}
	for _, local := range c.Targets {
		if local == out.Target && out.Expires > time.Now().Unix() && core.Identifier.MatchString(out.ServiceID) {
			if c.docker != nil {
				for _, a := range c.docker.Applications {
					if a.TargetKey == local.Key && !c.docker.Available(local.Key) {
						return out, errLocalAccess
					}
				}
			}
			return out, nil
		}
	}
	return out, errLocalAccess
}

func (g *deviceGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get(core.PortalProofHeader) != "" {
		g.servePortal(w, r)
		return
	}
	host := strings.ToLower(r.Host)
	if strings.ContainsAny(host, ":/ ") || strings.HasPrefix(r.URL.Path, "/_mf/") {
		http.Error(w, "not_found", 404)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+host {
		http.Error(w, "origin_not_allowed", 403)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		http.Error(w, "cross_origin_request", 403)
		return
	}
	cookie, err := r.Cookie(webproxy.SessionCookie)
	if err != nil || len(cookie.Value) < 32 || len(cookie.Value) > 256 {
		http.Error(w, "authentication_required", 401)
		return
	}
	c, err := Load(g.configPath)
	if err != nil || c.GatewayPort == 0 {
		http.Error(w, "service_unavailable", 503)
		return
	}
	grant, err := authorize(r.Context(), c, host, cookie.Value)
	if err != nil {
		var he HTTPError
		if errors.Is(err, errLocalAccess) || (errors.As(err, &he) && (he.Status == 401 || he.Status == 403)) {
			http.Error(w, "invalid_session", 401)
		} else {
			http.Error(w, "authorization_unavailable", 503)
		}
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var mu sync.Mutex
	var socket net.Conn
	closeSocket := func() {
		mu.Lock()
		defer mu.Unlock()
		if socket != nil {
			_ = socket.Close()
		}
	}
	defer closeSocket()
	// Recheck active transfers too. A controller outage fails closed here;
	// no indefinitely cached authorization survives a disconnect or revocation.
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		timer := time.NewTimer(time.Until(time.Unix(grant.Expires, 0)))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				cancel()
				closeSocket()
				return
			case <-tick.C:
				current, e := Load(g.configPath)
				var next core.AccessResponse
				checkCtx, finish := context.WithTimeout(ctx, 3*time.Second)
				if e == nil && current.DeviceID == c.DeviceID && current.Transport == c.Transport && current.GatewayPort == c.GatewayPort {
					next, e = authorize(checkCtx, current, host, cookie.Value)
				} else {
					e = errors.New("device configuration changed")
				}
				finish()
				if e != nil || next != grant {
					cancel()
					closeSocket()
					return
				}
			}
		}
	}()
	transport := webproxy.Transport()
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(dialCtx, network, address)
		if e != nil {
			return nil, e
		}
		g.mu.Lock()
		g.connections[conn] = struct{}{}
		g.mu.Unlock()
		mu.Lock()
		socket = conn
		mu.Unlock()
		if ctx.Err() != nil {
			_ = conn.Close()
			return nil, ctx.Err()
		}
		return conn, nil
	}
	defer func() {
		mu.Lock()
		conn := socket
		mu.Unlock()
		g.mu.Lock()
		delete(g.connections, conn)
		g.mu.Unlock()
	}()
	webproxy.Serve(w, r.WithContext(ctx), grant.Target, host, transport)
}
