package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
	"mfconnect/internal/webproxy"
)

const sessionCookie = webproxy.SessionCookie

func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	host := hostname(r.Host)
	st := s.Store.View()
	var svc store.Service
	for _, v := range st.Services {
		if s.serviceHost(v, host) {
			svc = v
			break
		}
	}
	if svc.ID == "" {
		reject(w, 404, "unknown_service")
		return
	}
	d, ok := st.Devices[svc.DeviceID]
	if !ok || d.Revoked || !svc.Published {
		reject(w, 404, "service_unavailable")
		return
	}
	if relayEscape(w, r) {
		return
	}
	if r.URL.Path == "/_mf/bootstrap" {
		s.bootstrap(w, r, svc)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_mf/") {
		reject(w, 404, "not_found")
		return
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		reject(w, 401, "authentication_required")
		return
	}
	sessionKey := core.Hash(cookie.Value)
	session, ok := st.Sessions[sessionKey]
	p, present := st.Principals[session.UserID]
	if !ok || !present || !s.sessionHost(session, svc, host) || session.ServiceID != svc.ID || session.Revision != svc.Revision || session.Expires <= time.Now().Unix() || !store.Authorized(&st, p, svc) {
		reject(w, 401, "invalid_session")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+hostname(r.Host) {
		reject(w, 403, "origin_not_allowed")
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		reject(w, 403, "cross_origin_request")
		return
	}
	if s.maybePublicApp(w, r, d, svc, session, sessionKey) {
		return
	}
	s.proxy(w, r, svc, sessionKey, session.Expires)
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request, svc store.Service) {
	if r.Method != "POST" || r.URL.RawQuery != "" {
		reject(w, 405, "post_required")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+hostname(r.Host) {
		reject(w, 403, "origin_not_allowed")
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		reject(w, 403, "cross_origin_request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		reject(w, 400, "invalid_request")
		return
	}
	ticketKey := core.Hash(r.PostForm.Get("ticket"))
	nonce := r.PostForm.Get("nonce")
	if len(nonce) < 32 || len(nonce) > 128 {
		reject(w, 401, "invalid_ticket")
		return
	}
	sessionToken := core.Random()
	var path string
	err := s.Store.Update(func(st *store.State) error {
		ticket, ok := st.Tickets[ticketKey]
		current := st.Services[svc.ID]
		p, present := st.Principals[ticket.UserID]
		if !ok || !present || ticket.Expires <= time.Now().Unix() || ticket.Host != hostname(r.Host) || ticket.ServiceID != svc.ID || ticket.Revision != current.Revision || !current.Published || !store.Authorized(st, p, current) || ticket.NonceHash != core.Hash(nonce) {
			return errors.New("invalid ticket")
		}
		delete(st.Tickets, ticketKey)
		path = ticket.Path
		st.Sessions[core.Hash(sessionToken)] = store.Session{PublicOrigin: ticket.PublicOrigin, NavigationExpires: ticket.Expires, Host: hostname(r.Host), UserID: ticket.UserID, ServiceID: svc.ID, Revision: current.Revision, Expires: time.Now().Add(time.Duration(s.Config.SessionSeconds) * time.Second).Unix()}
		return nil
	})
	if err != nil {
		reject(w, 401, "invalid_ticket")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sessionToken, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: s.Config.SessionSeconds})
	if _, e := r.Cookie(routeCookie); e == nil {
		http.SetCookie(w, &http.Cookie{Name: routeCookie, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Location", path)
	w.WriteHeader(http.StatusSeeOther)
}

type trackedConn struct {
	net.Conn
	once    sync.Once
	cleanup func()
}

func (c *trackedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.cleanup); return err }

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, svc store.Service, sessionKey string, expiry int64) {
	upstream := net.JoinHostPort(s.Config.ProxyHost, strconv.Itoa(svc.RemotePort))
	device := s.Store.View().Devices[svc.DeviceID]
	if device.Transport == "easytier" {
		var err error
		upstream, err = s.easyTierEndpoint(svc.DeviceID)
		if err != nil {
			reject(w, 503, "upstream_unavailable")
			return
		}
	}
	transport := webproxy.Transport()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", upstream)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.connections[sessionKey] == nil {
			s.connections[sessionKey] = map[net.Conn]struct{}{}
		}
		s.connections[sessionKey][c] = struct{}{}
		s.mu.Unlock()
		// Register first, then recheck. Revocation either observes this socket,
		// or this read observes the revoked session; no late dial escapes it.
		live, present := s.Store.View().Sessions[sessionKey]
		if !present || live.Expires <= time.Now().Unix() {
			_ = c.Close()
			s.mu.Lock()
			delete(s.connections[sessionKey], c)
			if len(s.connections[sessionKey]) == 0 {
				delete(s.connections, sessionKey)
			}
			s.mu.Unlock()
			return nil, errors.New("session revoked during connection establishment")
		}
		timer := time.AfterFunc(time.Until(time.Unix(expiry, 0)), func() { _ = c.Close() })
		return &trackedConn{Conn: c, cleanup: func() {
			timer.Stop()
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.connections[sessionKey], c)
			if len(s.connections[sessionKey]) == 0 {
				delete(s.connections, sessionKey)
			}
		}}, nil
	}
	if device.GatewayPin != "" {
		// The NAS terminates its own pinned TLS and repeats session authorization.
		// Only this internal hop receives the browser session; applications never do.
		transport.TLSClientConfig = webproxy.TLS("mfconnect-device", device.GatewayPin)
		proxy := httputil.ReverseProxy{Transport: transport, FlushInterval: -1,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(&url.URL{Scheme: "https", Host: "mfconnect-device"})
				pr.Out.Host = hostname(pr.In.Host)
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { reject(w, 503, "upstream_unavailable") },
		}
		proxy.ServeHTTP(w, r)
		transport.CloseIdleConnections()
		return
	}
	webproxy.Serve(w, r, svc.Target, hostname(r.Host), transport)
}
