package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
	"mfconnect/internal/webproxy"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validConnectHost(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}
func (s *Server) connectInfo(d store.Device, requests ...*http.Request) core.ConnectInfo {
	i := core.ConnectInfo{BrowserPublicDirect: d.BrowserPublicDirect, WebAccess: webAccessInfo(d), ID: d.ConnectID, Enabled: d.ConnectEnabled, Online: !d.Revoked && time.Now().Unix()-d.LastSeen < 45}
	domain := s.browserDomain(requests...)
	if d.ConnectID != "" && domain != "" {
		i.EntryURL = "https://" + domain + "/" + d.ConnectID
		i.Origin = "https://" + d.ConnectID + "." + domain
	}
	return i
}
func (s *Server) connectDevice(id string) (store.Device, bool) {
	for _, d := range s.Store.View().Devices {
		if d.ConnectID == id && !d.Revoked && d.ConnectEnabled && d.PortalSupported {
			return d, true
		}
	}
	return store.Device{}, false
}
func (s *Server) setConnect(device, id string, enabled bool, publicDirect ...*bool) (core.ConnectInfo, string) {
	id = core.NormalizeConnectID(id)
	if !core.ValidConnectID(id) && (id != "" || enabled) {
		return core.ConnectInfo{}, "invalid_connect_id"
	}
	var saved store.Device
	code := ""
	err := s.Store.Update(func(st *store.State) error {
		d, ok := st.Devices[device]
		if !ok || d.Revoked {
			code = "device_not_found"
			return errors.New(code)
		}
		if !d.PortalSupported || s.Config.ConnectHost == "" {
			code = "connector_upgrade_required"
			return errors.New(code)
		}
		if id == "" && d.ConnectID != "" {
			id = d.ConnectID
		}
		for _, other := range st.Devices {
			if id != "" && other.ID != device && other.ConnectID == id {
				code = "connect_id_taken"
				return errors.New(code)
			}
		}
		d.ConnectConfigured = true
		d.ConnectID, d.ConnectEnabled = id, enabled
		if len(publicDirect) > 0 && publicDirect[0] != nil {
			d.BrowserPublicDirect = *publicDirect[0]
			if !d.BrowserPublicDirect {
				for key, session := range st.Sessions {
					if svc := st.Services[session.ServiceID]; svc.DeviceID == device && !s.serviceHost(svc, session.Host) {
						delete(st.Sessions, key)
					} else if svc.DeviceID == device {
						session.PublicOrigin = ""
						session.NavigationExpires = 0
						st.Sessions[key] = session
					}
				}
				for key, ticket := range st.Tickets {
					if svc := st.Services[ticket.ServiceID]; svc.DeviceID == device && (ticket.SourceOrigin != "" || ticket.PublicOrigin != "") {
						delete(st.Tickets, key)
					}
				}
			}
		}
		for key, svc := range st.Services {
			if svc.DeviceID == device && svc.DockerManaged {
				svc.Published = enabled
				st.Services[key] = svc
			}
		}
		st.Devices[device] = d
		if err := s.checkHostCollisions(*st); err != nil {
			code = "connect_id_taken"
			return err
		}
		if !enabled {
			for _, svc := range st.Services {
				if svc.DeviceID == device {
					invalidate(st, svc.ID)
				}
			}
		}
		saved = d
		return nil
	})
	if err != nil {
		if code == "" {
			code = "storage_error"
		}
		return core.ConnectInfo{}, code
	}
	s.CloseInvalidConnections()
	return s.connectInfo(saved), ""
}
func connectError(w http.ResponseWriter, code string) {
	status := 409
	switch code {
	case "invalid_connect_id":
		status = 400
	case "device_not_found":
		status = 404
	case "storage_error":
		status = 500
	}
	reject(w, status, code)
}
func (s *Server) connectUserRoute(w http.ResponseWriter, r *http.Request, p store.Principal) bool {
	if r.URL.Path == "/v1/installations" {
		s.createInstallation(w, r, p)
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/devices/") || !strings.HasSuffix(r.URL.Path, "/connect") {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/devices/"), "/connect")
	st := s.Store.View()
	if !store.DeviceAuthorized(&st, p, id) {
		reject(w, 404, "device_not_found")
		return true
	}
	if r.Method == "GET" {
		respond(w, 200, s.connectInfo(st.Devices[id], r))
		return true
	}
	if r.Method != "PUT" {
		reject(w, 405, "method_not_allowed")
		return true
	}
	var in struct {
		BrowserPublicDirect *bool  `json:"browser_public_direct"`
		ID                  string `json:"id"`
		Enabled             *bool  `json:"enabled"`
	}
	if body(r, &in) != nil || in.Enabled == nil {
		reject(w, 400, "invalid_request")
		return true
	}
	_, code := s.setConnect(id, in.ID, *in.Enabled, in.BrowserPublicDirect)
	if code != "" {
		connectError(w, code)
	} else {
		respond(w, 200, s.connectInfo(s.Store.View().Devices[id], r))
	}
	return true
}
func (s *Server) sessionResponse(r *http.Request, p store.Principal, access, refresh string) map[string]any {
	d := s.Store.View().Devices[p.DeviceID]
	out := s.endpointInfo(r)
	out["device_id"], out["access_token"], out["expires_at"] = d.ID, access, p.Expires
	out["refresh_token"], out["refresh_expires_at"], out["connect"] = refresh, p.RefreshExpires, s.connectInfo(d, r)
	return out
}
func (s *Server) issueDeviceSession(w http.ResponseWriter, r *http.Request, d store.Device) {
	access, refresh := core.Random(), core.Random()
	now := time.Now().Unix()
	p := store.Principal{ID: core.ID("client"), OwnerID: d.Owner, DeviceID: d.ID, TokenHash: core.Hash(access), Expires: now + 3600, RefreshHash: core.Hash(refresh), RefreshExpires: now + 30*86400}
	err := s.Store.Update(func(st *store.State) error {
		current, ok := st.Devices[d.ID]
		if !ok || current.Revoked || current.Owner != d.Owner {
			return errors.New("revoked")
		}
		// Expired client identities cannot keep the persistent store growing forever.
		for id, old := range st.Principals {
			if old.DeviceID != "" && old.RefreshExpires <= now {
				delete(st.Principals, id)
			}
		}
		st.Principals[p.ID] = p
		return nil
	})
	if err != nil {
		reject(w, 403, "device_unavailable")
		return
	}
	respond(w, 200, s.sessionResponse(r, p, access, refresh))
}
func (s *Server) connectPublicRoute(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/v1/install/") {
		s.installManifest(w, r)
		return true
	}
	switch r.URL.Path {
	case "/v1/connect/resolve":
		if r.Method != "GET" {
			reject(w, 405, "method_not_allowed")
			return true
		}
		id := core.NormalizeConnectID(r.URL.Query().Get("id"))
		d, ok := s.connectDevice(id)
		if s.Config.ConnectHost == "" || !core.ValidConnectID(id) || !ok {
			reject(w, 404, "connect_not_found")
			return true
		}
		out := s.endpointInfo(r)
		out["device_id"], out["connect"] = d.ID, s.connectInfo(d, r)
		respond(w, 200, out)
		return true
	case "/v1/connect/refresh":
		if r.Method != "POST" {
			reject(w, 405, "method_not_allowed")
			return true
		}
		var in struct {
			Token string `json:"refresh_token"`
		}
		if body(r, &in) != nil || len(in.Token) < 32 || len(in.Token) > 128 {
			reject(w, 400, "invalid_request")
			return true
		}
		access, refresh := core.Random(), core.Random()
		var saved store.Principal
		err := s.Store.Update(func(st *store.State) error {
			for id, p := range st.Principals {
				if p.DeviceID != "" && p.RefreshHash == core.Hash(in.Token) && p.RefreshExpires > time.Now().Unix() {
					d, ok := st.Devices[p.DeviceID]
					if !ok || d.Revoked || d.Owner != p.OwnerID {
						return errors.New("revoked")
					}
					p.TokenHash, p.RefreshHash = core.Hash(access), core.Hash(refresh)
					p.Expires = time.Now().Unix() + 3600
					p.RefreshExpires = time.Now().Unix() + 30*86400
					st.Principals[id] = p
					saved = p
					return nil
				}
			}
			return errors.New("invalid refresh")
		})
		if err != nil {
			reject(w, 401, "invalid_refresh_token")
		} else {
			respond(w, 200, s.sessionResponse(r, saved, access, refresh))
		}
		return true
	case "/v1/device/connect":
		if r.Method != "POST" {
			reject(w, 405, "method_not_allowed")
			return true
		}
		s.deviceConnect(w, r)
		return true
	}
	return false
}
func (s *Server) connectHostRoute(w http.ResponseWriter, r *http.Request) bool {
	host := hostname(r.Host)
	domain := s.connectDomain(host)
	if domain == "" {
		return false
	}
	// A flat application subdomain shares the wildcard with NAS IDs.
	for _, svc := range s.Store.View().Services {
		if s.serviceHost(svc, host) {
			return false
		}
	}
	if host == domain {
		if r.Method != "GET" && r.Method != "HEAD" {
			reject(w, 405, "method_not_allowed")
			return true
		}
		id := strings.TrimPrefix(r.URL.Path, "/")
		if id == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte(`<!doctype html><html lang="zh-CN"><meta name="viewport" content="width=device-width,initial-scale=1"><title>MF Connect</title><body><h1>MF Connect</h1><p>请打开 RaidMate 中显示的完整访问链接。</p></body></html>`))
			return true
		}
		id = core.NormalizeConnectID(id)
		d, ok := s.connectDevice(id)
		if !core.ValidConnectID(id) || !ok {
			reject(w, 404, "connect_not_found")
			return true
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, "https://"+d.ConnectID+"."+domain+"/", http.StatusFound)
		return true
	}
	if !strings.HasSuffix(host, "."+domain) {
		return false
	}
	id := strings.TrimSuffix(host, "."+domain)
	d, ok := s.connectDevice(id)
	if !core.ValidConnectID(id) || !ok {
		reject(w, 404, "connect_not_found")
		return true
	}
	if relayEscape(w, r) {
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/_mf/") {
		reject(w, 404, "not_found")
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+host {
		reject(w, 403, "origin_not_allowed")
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		if r.Method != "GET" && r.Method != "HEAD" {
			reject(w, 403, "cross_origin_request")
			return true
		}
	}
	if d.PortalPort == 0 || d.GatewayPin == "" {
		reject(w, 503, "nas_unavailable")
		return true
	}
	if documentNavigation(r) && !relayPinned(r) && publicEnabled(d) {
		host := core.PublicHost(d.PublicIngress, core.PublicPortalLabel(d.ID))
		if s.verifyPublic(r.Context(), d, host) {
			writeRoutePage(w, r, d, host, "", "")
			return true
		}
	}
	s.portalProxy(w, r, d)
	return true
}
func (s *Server) portalProxy(w http.ResponseWriter, r *http.Request, d store.Device) {
	key := "portal:" + d.ID + ":" + d.ConnectID
	transport := webproxy.Transport()
	transport.TLSClientConfig = webproxy.TLS("mfconnect-device", d.GatewayPin)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(s.Config.ProxyHost, strconv.Itoa(d.PortalPort)))
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.connections[key] == nil {
			s.connections[key] = map[net.Conn]struct{}{}
		}
		s.connections[key][conn] = struct{}{}
		s.mu.Unlock()
		current := s.Store.View().Devices[d.ID]
		if current.Revoked || !current.ConnectEnabled || !current.PortalSupported || current.GatewayPin != d.GatewayPin || current.ConnectID != d.ConnectID {
			conn.Close()
			s.CloseInvalidConnections()
			return nil, errors.New("portal disabled")
		}
		return &trackedConn{Conn: conn, cleanup: func() {
			s.mu.Lock()
			delete(s.connections[key], conn)
			if len(s.connections[key]) == 0 {
				delete(s.connections, key)
			}
			s.mu.Unlock()
		}}, nil
	}
	proxy := httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(&url.URL{Scheme: "https", Host: "mfconnect-device"})
		pr.Out.Host = hostname(pr.In.Host)
		for name := range pr.Out.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-mf-") {
				pr.Out.Header.Del(name)
			}
		}
		pr.Out.Header.Set(core.PortalProofHeader, core.PortalProof(pr.Out, d.ID, d.TunnelTokenHash, time.Now()))
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { reject(w, 503, "nas_unavailable") }}
	proxy.ServeHTTP(w, r)
	transport.CloseIdleConnections()
}
