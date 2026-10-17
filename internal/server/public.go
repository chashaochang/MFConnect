package server

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

func webAccessInfo(d store.Device) core.WebAccessInfo {
	out := core.WebAccessInfo{Schema: 1, Mode: "relay_only", Status: "disabled", PublicIngressConfigured: d.PublicIngress != ""}
	if d.BrowserPublicDirect {
		out.Mode = "public_preferred"
		out.Status = "verify_on_access"
		if d.PublicIngress == "" {
			out.Status = "not_configured"
		}
	}
	return out
}
func (s *Server) validPublicIngress(origin string) bool {
	if origin == "" {
		return true
	}
	host, err := core.PublicIngressHost(origin)
	if err != nil {
		return false
	}
	// Public ingress may not overlap any controller namespace. Sharing a parent
	// domain with the controller is fine; sharing cookie/host namespaces is not.
	for _, other := range []string{s.Config.APIHost, s.Config.BaseDomain, s.Config.ConnectHost, s.Config.EntryHost(), s.Config.TunnelHost} {
		if other != "" && (host == other || strings.HasSuffix(host, "."+other) || strings.HasSuffix(other, "."+host)) {
			return false
		}
	}
	return true
}
func publicEnabled(d store.Device) bool {
	return !d.Revoked && d.ConnectEnabled && d.BrowserPublicDirect && d.PublicIngress != "" && time.Now().Unix()-d.LastSeen < 45
}
func (s *Server) publicServiceAllowed(d store.Device, svc store.Service, host string) bool {
	return publicEnabled(d) && host == core.PublicHost(d.PublicIngress, svc.HostKey)
}
func (s *Server) publicPortalAllowed(d store.Device, host string) bool {
	return publicEnabled(d) && d.PortalSupported && d.ConnectID != "" && host == core.PublicHost(d.PublicIngress, core.PublicPortalLabel(d.ID))
}
func (s *Server) controlCredential(hash string) bool {
	if hash == "" {
		return false
	}
	for _, p := range s.Store.View().Principals {
		if p.TokenHash == hash || p.RefreshHash == hash {
			return true
		}
	}
	return false
}

// Resolve once and dial only an inspected public address; HTTPS validates the
// original hostname. No environment proxy, redirect, private range, or rebinding.
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "240.0.0.0/4", "192.88.99.0/24", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}
func publicTransport(ctx context.Context, host string) (*http.Transport, error) {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("public DNS unavailable")
	}
	for _, ip := range ips {
		if !publicAddress(ip) {
			return nil, errors.New("nonpublic destination")
		}
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableKeepAlives: true, Proxy: nil}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var last error
		for _, ip := range ips {
			conn, e := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
			if e == nil {
				return conn, nil
			}
			last = e
		}
		return nil, last
	}
	return transport, nil
}
func probePublic(ctx context.Context, client *http.Client, d store.Device, host string) bool {
	nonce := core.Random()
	origin := "https://" + host
	req, _ := http.NewRequestWithContext(ctx, "GET", origin+"/_mf/public-probe?nonce="+url.QueryEscape(nonce), nil)
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return false
	}
	var out core.PublicProbe
	if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&out) != nil || out.Schema != 1 || out.DeviceID != d.ID || out.Origin != origin || out.Nonce != nonce {
		return false
	}
	pub, e1 := base64.RawStdEncoding.DecodeString(d.PublicKey)
	sig, e2 := base64.RawStdEncoding.DecodeString(out.Signature)
	return e1 == nil && e2 == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, core.PublicProbeMessage(d.ID, origin, nonce), sig)
}
func (s *Server) verifyPublic(ctx context.Context, d store.Device, host string) bool {
	if !publicEnabled(d) || host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	factory := s.publicTransportFactory
	if factory == nil {
		factory = publicTransport
	}
	transport, err := factory(ctx, host)
	if err != nil {
		return false
	}
	defer transport.CloseIdleConnections()
	return probePublic(ctx, &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, d, host)
}

const routeCookie = "__Host-mf_route"

func documentNavigation(r *http.Request) bool {
	if r.Method != "GET" || r.Header.Get("Upgrade") != "" {
		return false
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" {
		return false
	}
	accept := r.Header.Get("Accept")
	return accept == "" || strings.Contains(accept, "text/html")
}
func relayPinned(r *http.Request) bool {
	c, e := r.Cookie(routeCookie)
	return e == nil && c.Value == "relay"
}
func relayEscape(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/_mf/relay" {
		return false
	}
	path := r.URL.Query().Get("path")
	if r.Method != "GET" || path == "" || !core.SafePath(path) {
		reject(w, 400, "invalid_request")
		return true
	}
	http.SetCookie(w, &http.Cookie{Name: routeCookie, Value: "relay", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 300})
	http.Redirect(w, r, path, http.StatusSeeOther)
	return true
}

var routePage = template.Must(template.New("route").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="referrer" content="no-referrer"><title>MF Connect</title><p>正在连接… <a href="{{.Fallback}}">使用中继</a></p><form id="next" method="post" action="{{.Action}}"><input type="hidden" name="ticket" value="{{.Ticket}}"><input type="hidden" name="nonce" value="{{.Nonce}}"></form><script nonce="{{.CSP}}">const fallback={{.Fallback}},probe={{.Probe}},destination={{.Destination}},device={{.Device}},expires=Number({{.Expires}});let finished=false;const controller=new AbortController();const timer=setTimeout(()=>{controller.abort();finish(false)},1800);function finish(ok){if(finished)return;finished=true;clearTimeout(timer);if(!ok||(expires&&Date.now()>=expires*1000))location.replace(fallback);else if(destination)location.replace(destination);else document.getElementById('next').submit()}fetch(probe,{mode:'cors',credentials:'omit',cache:'no-store',redirect:'error',signal:controller.signal}).then(r=>{if(!r.ok)throw Error();return r.json()}).then(j=>finish(j.schema_version===1&&j.device_id===device)).catch(()=>finish(false));</script><noscript><p>浏览器未启用脚本，请点击“使用中继”。</p></noscript></html>`))

func writeRoutePage(w http.ResponseWriter, r *http.Request, d store.Device, host, ticket, nonce string, deadline ...int64) {
	origin := "https://" + host
	expires := int64(0)
	if len(deadline) > 0 {
		expires = deadline[0]
	}
	csp := core.Random()
	destination := ""
	if ticket == "" {
		destination = origin + r.URL.RequestURI()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+csp+"'; connect-src "+origin+"; form-action "+origin+"; base-uri 'none'; frame-ancestors 'none'")
	_ = routePage.Execute(w, map[string]string{"Fallback": "/_mf/relay?path=" + url.QueryEscape(r.URL.RequestURI()), "Action": origin + "/_mf/bootstrap", "Probe": origin + "/_mf/public-probe?nonce=" + core.Random(), "Destination": destination, "Device": d.ID, "Ticket": ticket, "Nonce": nonce, "CSP": csp, "Expires": fmt.Sprint(expires)})
}
func (s *Server) maybePublicApp(w http.ResponseWriter, r *http.Request, d store.Device, svc store.Service, session store.Session, sessionKey string) bool {
	if !documentNavigation(r) || relayPinned(r) || !publicEnabled(d) {
		return false
	}
	host := core.PublicHost(d.PublicIngress, svc.HostKey)
	if session.PublicOrigin != "https://"+host || session.NavigationExpires <= time.Now().Unix() || !s.verifyPublic(r.Context(), d, host) {
		return false
	}
	ticket, nonce := core.Random(), core.Random()
	err := s.Store.Update(func(st *store.State) error {
		current := st.Devices[d.ID]
		liveSession, present := st.Sessions[sessionKey]
		if !present || liveSession.PublicOrigin != session.PublicOrigin || liveSession.PublicOrigin == "" || liveSession.NavigationExpires <= time.Now().Unix() {
			return errors.New("navigation grant expired")
		}
		live := st.Services[svc.ID]
		p, ok := st.Principals[session.UserID]
		if !ok || !s.publicServiceAllowed(current, live, host) || live.Revision != session.Revision || !store.Authorized(st, p, live) || !live.Published {
			return errors.New("access changed")
		}
		liveSession.PublicOrigin = ""
		liveSession.NavigationExpires = 0
		st.Sessions[sessionKey] = liveSession
		st.Tickets[core.Hash(ticket)] = store.Ticket{SourceOrigin: "https://" + r.Host, Host: host, ServiceID: svc.ID, Revision: live.Revision, UserID: p.ID, Path: r.URL.RequestURI(), NonceHash: core.Hash(nonce), Expires: session.NavigationExpires}
		return nil
	})
	if err != nil {
		return false
	}
	writeRoutePage(w, r, d, host, ticket, nonce, session.NavigationExpires)
	return true
}
func (s *Server) publicBootstrap(w http.ResponseWriter, r *http.Request, d store.Device, host, ticket, nonce, source string) {
	if len(nonce) < 32 || len(nonce) > 128 || len(ticket) > 128 {
		reject(w, 401, "invalid_ticket")
		return
	}
	token := core.Random()
	path := ""
	expiry := time.Now().Unix() + int64(s.Config.SessionSeconds)
	err := s.Store.Update(func(st *store.State) error {
		t, ok := st.Tickets[core.Hash(ticket)]
		svc := st.Services[t.ServiceID]
		p, present := st.Principals[t.UserID]
		current := st.Devices[d.ID]
		if !ok || !present || t.Expires <= time.Now().Unix() || t.SourceOrigin == "" || t.SourceOrigin != source || t.Host != host || svc.DeviceID != d.ID || t.Revision != svc.Revision || t.NonceHash != core.Hash(nonce) || !svc.Published || !store.Authorized(st, p, svc) || !s.publicServiceAllowed(current, svc, host) {
			return errors.New("invalid ticket")
		}
		delete(st.Tickets, core.Hash(ticket))
		st.Sessions[core.Hash(token)] = store.Session{Host: host, UserID: p.ID, ServiceID: svc.ID, Revision: svc.Revision, Expires: expiry}
		path = t.Path
		return nil
	})
	if err != nil {
		reject(w, 401, "invalid_ticket")
		return
	}
	respond(w, 200, map[string]any{"session": token, "path": path, "expires_at": expiry})
}
