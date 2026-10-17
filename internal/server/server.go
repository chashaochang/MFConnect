package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
	"mfconnect/internal/updates"
)

type Config struct {
	EntryOrigin        string          `json:"entry_origin,omitempty"`
	LegacyAPIRoot      bool            `json:"legacy_api_root,omitempty"`
	InstallMode        string          `json:"install_mode,omitempty"`
	InstallDeviceLimit int             `json:"install_device_limit,omitempty"`
	PluginManifest     string          `json:"plugin_manifest,omitempty"`
	ConnectHost        string          `json:"connect_host,omitempty"`
	Updates            *UpdateConfig   `json:"updates,omitempty"`
	FRP                *FRPConfig      `json:"frp,omitempty"`
	EasyTier           *EasyTierConfig `json:"easytier,omitempty"`
	Listen             string          `json:"listen"`
	HookListen         string          `json:"hook_listen"`
	APIHost            string          `json:"api_host"`
	BaseDomain         string          `json:"base_domain"`
	Database           string          `json:"database"`
	Certificate        string          `json:"certificate"`
	PrivateKey         string          `json:"private_key"`
	TunnelHost         string          `json:"tunnel_host"`
	TunnelPort         int             `json:"tunnel_port"`
	ProxyHost          string          `json:"proxy_host"`
	PortStart          int             `json:"port_start"`
	PortEnd            int             `json:"port_end"`
	SessionSeconds     int             `json:"session_seconds"`
	TrustedProxyIPs    []string        `json:"trusted_proxy_ips,omitempty"`
}

type Server struct {
	publicTransportFactory func(context.Context, string) (*http.Transport, error)
	releaseKey             string
	easy                   *easyTierManager
	Config                 Config
	Store                  *store.Store
	mu                     sync.Mutex
	connections            map[string]map[net.Conn]struct{}
}

func New(c Config, st *store.Store) (*Server, error) {
	if c.EntryOrigin != "" {
		host, err := EntryHost(c.EntryOrigin)
		if err != nil {
			return nil, err
		}
		c.EntryOrigin = "https://" + host
		if c.APIHost == "" {
			c.APIHost = host
		}
		if c.BaseDomain == "" {
			c.BaseDomain = host
		}
		if c.ConnectHost == "" {
			c.ConnectHost = host
		}
		if c.TunnelHost == "" {
			c.TunnelHost = host
		}
		if c.APIHost == host && c.BaseDomain != host && !c.LegacyAPIRoot {
			return nil, errors.New("reusing a legacy API hostname as entry requires explicit legacy_api_root=true to preserve /v1 routes")
		}
	}

	if c.InstallMode == "" {
		c.InstallMode = "disabled"
	}
	if c.InstallMode != "disabled" && c.InstallMode != "self_service" {
		return nil, errors.New("invalid installation mode")
	}
	if c.InstallDeviceLimit == 0 {
		c.InstallDeviceLimit = 10
	}
	if c.InstallDeviceLimit < 1 || c.InstallDeviceLimit > 1000 {
		return nil, errors.New("invalid installation device limit")
	}
	if c.ConnectHost != "" && (!validConnectHost(c.ConnectHost) || (c.EntryOrigin == "" && (c.ConnectHost == c.APIHost || c.ConnectHost == c.BaseDomain))) {
		return nil, errors.New("invalid connect hostname")
	}
	if c.APIHost == "" || c.BaseDomain == "" || strings.ContainsAny(c.APIHost+c.BaseDomain, "/: ") || (c.EntryOrigin == "" && c.APIHost == c.BaseDomain) {
		return nil, errors.New("valid separate API and service domains required")
	}
	if c.PortStart < 1024 || c.PortEnd < c.PortStart || c.PortEnd > 65535 {
		return nil, errors.New("invalid private tunnel port range")
	}
	if c.ProxyHost == "" {
		c.ProxyHost = "127.0.0.1"
	}
	if c.SessionSeconds == 0 {
		c.SessionSeconds = 3600
	}
	for _, proxy := range c.TrustedProxyIPs {
		ip, err := netip.ParseAddr(proxy)
		if err != nil || ip.Zone() != "" || (!ip.IsGlobalUnicast() && !ip.IsLoopback()) {
			return nil, errors.New("trusted_proxy_ips must contain explicit unicast IP addresses")
		}
	}
	app := &Server{Config: c, Store: st, releaseKey: updates.PublicKey, connections: map[string]map[net.Conn]struct{}{}}
	if err := app.checkHostCollisions(st.View()); err != nil {
		return nil, err
	}
	return app, nil
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func reject(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func body(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("unexpected additional input")
	}
	return nil
}
func hostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(host)
}
func (s *Server) origin(service store.Service, requests ...*http.Request) string {
	return "https://" + service.HostKey + "." + s.appsDomain(requests...)
}
func (s *Server) principal(r *http.Request) (store.Principal, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return store.Principal{}, false
	}
	st := s.Store.View()
	hash := core.Hash(tok)
	for _, p := range st.Principals {
		if p.TokenHash == hash && (p.Expires == 0 || p.Expires > time.Now().Unix()) {
			return p, true
		}
	}
	return store.Principal{}, false
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.Config.EntryOrigin != "" && r.TLS != nil && r.TLS.ServerName != "" && hostname(r.TLS.ServerName) != hostname(r.Host) {
		reject(w, 421, "sni_host_mismatch")
		return
	}
	if h := r.Header.Get("X-MF-Endpoint-Contract"); h != "" && h != "1" && h != EndpointContract {
		reject(w, 400, "unsupported_endpoint_contract")
		return
	}
	if r.URL.Path == "/.well-known/mfconnect" && (hostname(r.Host) == s.Config.EntryHost() || hostname(r.Host) == s.Config.APIHost) {
		if r.Method != "GET" {
			reject(w, 405, "method_not_allowed")
			return
		}
		respond(w, 200, s.endpointInfo(r))
		return
	}
	routed, api := s.routeAPI(w, r)
	if !api {
		s.stripControlToken(r)
		if s.connectHostRoute(w, r) {
			return
		}
		s.gateway(w, r)
		return
	}
	if routed == nil {
		return
	}
	r = routed
	w.Header().Set("X-MF-Endpoint-Contract", EndpointContract)
	if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+hostname(r.Host) {
		reject(w, 403, "origin_not_allowed")
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		reject(w, 403, "cross_origin_request")
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/endpoints" {
		respond(w, 200, s.endpointInfo(r))
		return
	}
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		respond(w, 200, map[string]string{"status": "ok", "version": core.Version})
		return
	}
	if r.Method == "POST" && r.URL.Path == "/v1/devices/enroll" {
		s.enroll(w, r)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/v1/device/sync" {
		s.sync(w, r)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/v1/device/access" {
		s.deviceAccess(w, r)
		return
	}
	if s.connectPublicRoute(w, r) {
		return
	}
	if s.installationPublicRoute(w, r) {
		return
	}
	p, ok := s.principal(r)
	if !ok {
		reject(w, 401, "authentication_required")
		return
	}
	if s.updateRoute(w, r, p) {
		return
	}
	if s.connectUserRoute(w, r, p) {
		return
	}
	if s.dockerRoute(w, r, p) {
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/enrollments":
		if !p.Admin {
			reject(w, 403, "forbidden")
			return
		}
		var in struct {
			Owner string `json:"owner"`
		}
		if body(r, &in) != nil {
			reject(w, 400, "invalid_request")
			return
		}
		if in.Owner == "" {
			in.Owner = p.ID
		}
		token := core.Random()
		exp := time.Now().Add(10 * time.Minute).Unix()
		err := s.Store.Update(func(st *store.State) error {
			if _, ok := st.Principals[in.Owner]; !ok {
				return errors.New("unknown owner")
			}
			st.Enrollments[core.Hash(token)] = store.Enrollment{Owner: in.Owner, Expires: exp}
			return nil
		})
		if err != nil {
			reject(w, 400, "enrollment_failed")
			return
		}
		respond(w, 201, map[string]any{"enrollment_token": token, "expires_at": exp})
	case r.Method == "GET" && r.URL.Path == "/v1/devices":
		st := s.Store.View()
		out := []map[string]any{}
		for _, d := range st.Devices {
			if store.DeviceAuthorized(&st, p, d.ID) {
				entry := map[string]any{"id": d.ID, "name": d.Name, "owner": d.Owner, "revoked": d.Revoked, "last_seen": d.LastSeen, "online": !d.Revoked && time.Now().Unix()-d.LastSeen < 45}
				if !d.Revoked && d.Network != nil {
					entry["network"] = d.Network
				}
				if d.Software != nil {
					entry["software"] = d.Software
				}
				out = append(out, entry)
			}
		}
		respond(w, 200, out)
	case r.Method == "GET" && r.URL.Path == "/v1/services":
		st := s.Store.View()
		out := []map[string]any{}
		for _, svc := range st.Services {
			if store.DeviceAuthorized(&st, p, svc.DeviceID) {
				out = append(out, map[string]any{"id": svc.ID, "device_id": svc.DeviceID, "key": svc.Target.Key, "name": svc.Target.Name, "origin": s.origin(svc, r), "published": svc.Published, "revision": svc.Revision})
			}
		}
		respond(w, 200, out)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/v1/services/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/services/")
		var in struct {
			Published *bool `json:"published"`
		}
		if body(r, &in) != nil || in.Published == nil {
			reject(w, 400, "invalid_request")
			return
		}
		err := s.Store.Update(func(st *store.State) error {
			svc, ok := st.Services[id]
			if !ok || !store.DeviceAuthorized(st, p, svc.DeviceID) {
				return errors.New("forbidden")
			}
			if svc.DockerManaged {
				return errors.New("docker_automatically_managed")
			}
			if svc.Published != *in.Published {
				svc.Revision++
				svc.Published = *in.Published
				st.Services[id] = svc
				invalidate(st, id)
			}
			return nil
		})
		if err != nil {
			if err.Error() == "docker_automatically_managed" {
				reject(w, 409, err.Error())
				return
			}
			reject(w, 403, "forbidden")
			return
		}
		s.CloseInvalidConnections()
		respond(w, 200, map[string]bool{"updated": true})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/devices/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/devices/")
		err := s.Store.Update(func(st *store.State) error {
			d, ok := st.Devices[id]
			if !ok || !store.DeviceAuthorized(st, p, id) {
				return errors.New("forbidden")
			}
			d.Revoked = true
			st.Devices[id] = d
			for _, svc := range st.Services {
				if svc.DeviceID == id {
					invalidate(st, svc.ID)
				}
			}
			return nil
		})
		if err != nil {
			reject(w, 403, "forbidden")
			return
		}
		s.CloseInvalidConnections()
		respond(w, 200, map[string]bool{"revoked": true})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/services/") && strings.HasSuffix(r.URL.Path, "/launches"):
		s.launch(w, r, p)
	case r.Method == "DELETE" && r.URL.Path == "/v1/browser-sessions":
		err := s.Store.Update(func(st *store.State) error {
			for k, v := range st.Sessions {
				if v.UserID == p.ID {
					delete(st.Sessions, k)
				}
			}
			for k, v := range st.Tickets {
				if v.UserID == p.ID {
					delete(st.Tickets, k)
				}
			}
			return nil
		})
		if err != nil {
			reject(w, 500, "storage_error")
			return
		}
		s.CloseInvalidConnections()
		respond(w, 200, map[string]bool{"revoked": true})
	default:
		reject(w, 404, "not_found")
	}
}

func invalidate(st *store.State, id string) {
	for k, v := range st.Tickets {
		if v.ServiceID == id {
			delete(st.Tickets, k)
		}
	}
	for k, v := range st.Sessions {
		if v.ServiceID == id {
			delete(st.Sessions, k)
		}
	}
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token     string `json:"enrollment_token"`
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
		Proof     string `json:"proof"`
	}
	if body(r, &in) != nil || len(in.Name) > 128 {
		reject(w, 400, "invalid_request")
		return
	}
	pub, err := base64.RawStdEncoding.DecodeString(in.PublicKey)
	proof, e := base64.RawStdEncoding.DecodeString(in.Proof)
	if err != nil || e != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte("mfconnect-enroll\n"+in.Token+"\n"+in.Name), proof) {
		reject(w, 401, "invalid_identity")
		return
	}
	token, id := core.Random(), core.ID("dev")
	err = s.Store.Update(func(st *store.State) error {
		invite, ok := st.Enrollments[core.Hash(in.Token)]
		if !ok || invite.Expires <= time.Now().Unix() {
			return errors.New("invalid enrollment")
		}
		if invite.InstallRequest != "" {
			install, exists := st.Installations[invite.InstallRequest]
			if !exists || install.Consumed || install.Expires <= time.Now().Unix() || install.EnrollmentHash != core.Hash(in.Token) {
				return errors.New("invalid installation")
			}
			if activeDeviceCount(st) >= s.Config.InstallDeviceLimit {
				return errors.New("device_limit_reached")
			}
			install.Consumed = true
			st.Installations[invite.InstallRequest] = install
		}
		delete(st.Enrollments, core.Hash(in.Token))
		st.Devices[id] = store.Device{ID: id, Name: in.Name, Owner: invite.Owner, PublicKey: in.PublicKey, TunnelTokenHash: core.Hash(token), LastSeen: time.Now().Unix()}
		return nil
	})
	if err != nil {
		if err.Error() == "device_limit_reached" {
			reject(w, 409, "device_limit_reached")
			return
		}
		reject(w, 401, "invalid_enrollment")
		return
	}
	respond(w, 201, map[string]string{"device_id": id, "tunnel_token": token})
}

func (s *Server) authenticateDevice(r *http.Request, b []byte) (store.Device, error) {
	var d store.Device
	err := s.Store.Update(func(st *store.State) error {
		var ok bool
		d, ok = st.Devices[r.Header.Get("X-MF-Device")]
		if !ok || d.Revoked {
			return errors.New("unknown device")
		}
		stamp, nonce := r.Header.Get("X-MF-Time"), r.Header.Get("X-MF-Nonce")
		n, err := strconv.ParseInt(stamp, 10, 64)
		now := time.Now().Unix()
		if err != nil || n < now-120 || n > now+120 || len(nonce) < 32 || len(nonce) > 128 {
			return errors.New("invalid timestamp or nonce")
		}
		sig, err := base64.RawStdEncoding.DecodeString(r.Header.Get("X-MF-Signature"))
		pub, _ := base64.RawStdEncoding.DecodeString(d.PublicKey)
		if err != nil || len(pub) != 32 || !ed25519.Verify(pub, core.SignatureMessage(r.Method, signedURI(r), stamp, nonce, b), sig) {
			return errors.New("invalid signature")
		}
		key := d.ID + ":" + nonce
		if _, ok := st.Nonces[key]; ok {
			return errors.New("replayed request")
		}
		st.Nonces[key] = now + 300
		return nil
	})
	return d, err
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		reject(w, 400, "request_too_large")
		return
	}
	d, err := s.authenticateDevice(r, b)
	if err != nil {
		reject(w, 401, "invalid_device_signature")
		return
	}
	var in core.SyncRequest
	if json.Unmarshal(b, &in) != nil || len(in.Targets) > 320 {
		reject(w, 400, "invalid_targets")
		return
	}
	if in.Software != nil && (!updates.ValidVersion(in.Software.Plugin) || !updates.ValidVersion(in.Software.Connector) || in.Software.UpdateProtocol < 0 || in.Software.UpdateProtocol > updates.Protocol) {
		reject(w, 400, "invalid_software_report")
		return
	}
	if in.UpdateReport != nil && !updates.ValidReport(*in.UpdateReport) {
		reject(w, 400, "invalid_update_report")
		return
	}
	if in.Network != nil {
		normalized, err := in.Network.Normalize()
		if err != nil {
			reject(w, 400, "invalid_network")
			return
		}
		in.Network = &normalized
	}
	if in.Transport == "" {
		in.Transport = "frp"
	}
	if in.Transport != "frp" && in.Transport != "easytier" {
		reject(w, 400, "unsupported_transport")
		return
	}
	var gatewayPin string
	if in.Transport == "easytier" && s.Config.EasyTier == nil {
		reject(w, 409, "easytier_not_enabled")
		return
	}
	if in.Transport == "easytier" || in.GatewayPort != 0 || in.GatewayCertificate != "" {
		gatewayPin, err = validateGateway(d, in)
		if err != nil {
			reject(w, 400, "invalid_gateway_identity")
			return
		}
	}
	keys := map[string]bool{}
	for _, t := range in.Targets {
		if t.Validate() != nil || keys[t.Key] {
			reject(w, 400, "invalid_targets")
			return
		}
		keys[t.Key] = true
	}
	if validateDocker(in) != nil {
		reject(w, 400, "invalid_docker_report")
		return
	}
	if !s.validPublicIngress(in.PublicIngress) {
		reject(w, 400, "invalid_public_ingress")
		return
	}
	managed := dockerKeys(in.Docker)
	out := core.SyncResponse{Gateway: gatewayPin != "", Transport: in.Transport, TunnelHost: s.Config.TunnelHost, TunnelPort: s.Config.TunnelPort, Assignments: []core.Assignment{}}
	err = s.Store.Update(func(st *store.State) error {
		current := st.Devices[d.ID]
		if current.Revoked {
			return errors.New("revoked")
		}
		if current.GatewayPin != "" && gatewayPin == "" {
			return errors.New("cannot remove the authenticated NAS gateway")
		}
		previousTransport := current.Transport
		if previousTransport == "" {
			previousTransport = "frp"
		}
		if previousTransport != in.Transport || current.GatewayPin != gatewayPin {
			for id, svc := range st.Services {
				if svc.DeviceID == d.ID {
					svc.Revision++
					st.Services[id] = svc
					invalidate(st, id)
				}
			}
		}
		current.Transport = in.Transport
		current.PortalSupported = in.PortalSupported && gatewayPin != "" && in.Transport == "frp"
		current.GatewayPort, current.GatewayPin = in.GatewayPort, gatewayPin
		if in.Transport == "easytier" {
			if current.EasyTierSecret == "" {
				current.EasyTierSecret = core.Random()
			}
			if current.EasyTierPort == 0 {
				used := map[int]bool{}
				for _, device := range st.Devices {
					used[device.EasyTierPort] = true
				}
				port := s.Config.EasyTier.PortStart
				for port <= s.Config.EasyTier.PortEnd && used[port] {
					port++
				}
				if port > s.Config.EasyTier.PortEnd {
					return errors.New("EasyTier port pool exhausted")
				}
				current.EasyTierPort = port
			}
			out.EasyTier = &core.EasyTierAssignment{Network: current.ID, Secret: current.EasyTierSecret, Peer: "tcp://" + net.JoinHostPort(s.Config.TunnelHost, strconv.Itoa(current.EasyTierPort))}
		}
		if current.PublicIngress != in.PublicIngress {
			for key, session := range st.Sessions {
				if svc := st.Services[session.ServiceID]; svc.DeviceID == d.ID && !s.serviceHost(svc, session.Host) {
					delete(st.Sessions, key)
				}
			}
		}
		current.PublicIngress = in.PublicIngress
		current.LastSeen = time.Now().Unix()
		if in.Docker != nil {
			current.Docker = in.Docker
		} else if current.Docker != nil {
			current.Docker.State = "unavailable"
		}
		current.Software = in.Software
		applyDeviceUpdate(st, current, in.UpdateReport, &out)
		if current.Network == nil {
			current.Network = &core.DeviceNetwork{InterfaceIPs: []core.InterfaceIP{}}
		}
		if in.Network != nil {
			current.Network.InterfaceIPs = in.Network.InterfaceIPs
			current.Network.InterfacesUpdatedAt = current.LastSeen
		}
		current.Network.PublicIP = s.observedPublicIP(r)
		current.Network.PublicIPObservedAt = 0
		if current.Network.PublicIP != "" {
			current.Network.PublicIPObservedAt = current.LastSeen
		}
		st.Devices[d.ID] = current
		used := map[int]bool{}
		for _, v := range st.Services {
			used[v.RemotePort] = true
		}
		for _, v := range st.Devices {
			if v.PortalPort != 0 {
				used[v.PortalPort] = true
			}
		}
		if s.Config.ConnectHost != "" && current.PortalSupported {
			if current.PortalPort == 0 {
				port := s.Config.PortStart
				for port <= s.Config.PortEnd && used[port] {
					port++
				}
				if port > s.Config.PortEnd {
					return errors.New("port pool exhausted")
				}
				current.PortalPort = port
				used[port] = true
				st.Devices[d.ID] = current
			}
			out.Portal = &core.PortalAssignment{ConnectInfo: s.connectInfo(current, r), RemotePort: current.PortalPort}
		}
		for _, target := range in.Targets {
			var svc store.Service
			for _, v := range st.Services {
				if v.DeviceID == d.ID && v.Target.Key == target.Key {
					svc = v
					break
				}
			}
			if svc.ID == "" {
				port := s.Config.PortStart
				for port <= s.Config.PortEnd && used[port] {
					port++
				}
				if port > s.Config.PortEnd {
					return errors.New("port pool exhausted")
				}
				used[port] = true
				svc = store.Service{ID: core.ID("svc"), DeviceID: d.ID, HostKey: newApplicationHost(st), Target: target, RemotePort: port, Revision: 1}
			}
			wasAvailable := svc.DockerAvailable
			svc.DockerManaged = svc.DockerManaged || managed[target.Key]
			if svc.DockerManaged {
				svc.Published = current.ConnectEnabled
			}
			svc.DockerAvailable = managed[target.Key] && in.Docker.Available(target.Key)
			if svc.Target != target {
				svc.Target = target
				svc.Revision++
				if !svc.DockerManaged {
					svc.Published = false
				}
				invalidate(st, svc.ID)
			}
			if svc.DockerManaged && wasAvailable && !svc.DockerAvailable {
				svc.Revision++
				invalidate(st, svc.ID)
			}
			st.Services[svc.ID] = svc
			out.Assignments = append(out.Assignments, core.Assignment{ServiceID: svc.ID, Key: target.Key, RemotePort: svc.RemotePort, Published: svc.Published && store.RemoteEnabled(current) && store.DockerAvailable(current, svc)})
		}
		for id, svc := range st.Services {
			if svc.DeviceID == d.ID && !keys[svc.Target.Key] {
				if svc.DockerManaged {
					if svc.DockerAvailable {
						svc.Revision++
					}
					svc.DockerAvailable = false
				} else {
					if svc.Published {
						svc.Revision++
					}
					svc.Published = false
				}
				st.Services[id] = svc
				invalidate(st, id)
			}
		}
		return s.checkHostCollisions(*st)
	})
	if err != nil {
		reject(w, 409, "sync_failed")
		return
	}
	s.CloseInvalidConnections()
	respond(w, 200, out)
}

func (s *Server) launch(w http.ResponseWriter, r *http.Request, p store.Principal) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/services/"), "/launches")
	var in struct {
		NavigationContract int    `json:"navigation_contract"`
		Path               string `json:"path"`
		NonceHash          string `json:"nonce_sha256"`
	}
	if body(r, &in) != nil || !core.SafePath(in.Path) || (in.NavigationContract != 0 && in.NavigationContract != 1) {
		reject(w, 400, "invalid_request")
		return
	}
	nonce, err := hex.DecodeString(in.NonceHash)
	if err != nil || len(nonce) != 32 {
		reject(w, 400, "invalid_nonce")
		return
	}
	if in.Path == "" {
		in.Path = "/"
	}
	publicOrigin := ""
	if in.NavigationContract == 1 {
		state := s.Store.View()
		svc, ok := state.Services[id]
		d := state.Devices[svc.DeviceID]
		if !ok || !svc.Published || !store.Authorized(&state, p, svc) {
			reject(w, 403, "forbidden")
			return
		}
		host := core.PublicHost(d.PublicIngress, svc.HostKey)
		if s.verifyPublic(r.Context(), d, host) {
			publicOrigin = "https://" + host
		}
	}
	expires := time.Now().Unix() + 30
	token := core.Random()
	var origin string
	err = s.Store.Update(func(st *store.State) error {
		svc, ok := st.Services[id]
		if !ok || !svc.Published || !store.Authorized(st, p, svc) {
			return errors.New("forbidden")
		}
		origin = s.origin(svc, r)
		if !s.publicServiceAllowed(st.Devices[svc.DeviceID], svc, strings.TrimPrefix(publicOrigin, "https://")) {
			publicOrigin = ""
		}
		st.Tickets[core.Hash(token)] = store.Ticket{PublicOrigin: publicOrigin, UserID: p.ID, ServiceID: id, Revision: svc.Revision, Host: strings.TrimPrefix(origin, "https://"), Path: in.Path, NonceHash: in.NonceHash, Expires: expires}
		return nil
	})
	if err != nil {
		reject(w, 403, "forbidden")
		return
	}
	out := map[string]any{"mode": "relay", "origin": origin, "bootstrap_url": origin + "/_mf/bootstrap", "bootstrap_path": "/_mf/bootstrap", "bootstrap_method": "POST", "ticket": token, "expires_in": 30, "service_id": id}
	if in.NavigationContract == 1 {
		out["navigation_contract"] = 1
		out["navigation_origins"] = []string{origin}
		out["public_gateway_origin"] = nil
		out["navigation_expires_at"] = expires
		if publicOrigin != "" {
			out["public_gateway_origin"] = publicOrigin
			out["navigation_origins"] = []string{origin, publicOrigin}
		}
	}
	respond(w, 201, out)
}

// Hook is bound only on a private/loopback listener. Every frp operation checks
// the device credential again; default frp token authentication is not a substitute.
func (s *Server) Hook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string          `json:"version"`
		Op      string          `json:"op"`
		Content json.RawMessage `json:"content"`
	}
	if r.Method != "POST" || body(r, &in) != nil {
		reject(w, 400, "invalid_request")
		return
	}
	var raw struct {
		User  json.RawMessage   `json:"user"`
		Metas map[string]string `json:"metas"`
		Name  string            `json:"proxy_name"`
		Type  string            `json:"proxy_type"`
		Port  int               `json:"remote_port"`
		Group string            `json:"group"`
	}
	if json.Unmarshal(in.Content, &raw) != nil {
		reject(w, 400, "invalid_request")
		return
	}
	op := r.URL.Query().Get("op")
	if in.Version != "0.1.0" || in.Op != op {
		reject(w, 400, "invalid_plugin_protocol")
		return
	}
	var id string
	metas := raw.Metas
	if op == "Login" {
		_ = json.Unmarshal(raw.User, &id)
	} else {
		var u struct {
			User  string            `json:"user"`
			Metas map[string]string `json:"metas"`
		}
		_ = json.Unmarshal(raw.User, &u)
		id = u.User
		metas = u.Metas
	}
	st := s.Store.View()
	d, ok := st.Devices[id]
	allowed := ok && !d.Revoked && d.Transport != "easytier" && metas["mf_token"] != "" && d.TunnelTokenHash == core.Hash(metas["mf_token"])
	switch op {
	case "Login", "Ping", "NewWorkConn":
	case "NewProxy", "NewUserConn":
		match := false
		if d.ConnectEnabled && d.ConnectID != "" && d.PortalSupported && d.PortalPort > 0 && raw.Name == id+".portal" {
			match = op == "NewUserConn" || (raw.Type == "tcp" && raw.Port == d.PortalPort && raw.Group == "")
		}
		for _, svc := range st.Services {
			if svc.DeviceID == id && svc.Published && store.RemoteEnabled(d) && store.DockerAvailable(d, svc) && raw.Name == id+"."+svc.ID {
				match = true
				if op == "NewProxy" && (raw.Type != "tcp" || raw.Port != svc.RemotePort || raw.Group != "") {
					match = false
				}
				break
			}
		}
		allowed = allowed && match
	default:
		allowed = false
	}
	respond(w, 200, map[string]any{"reject": !allowed, "reject_reason": map[bool]string{true: "", false: "device or service not authorized"}[allowed], "unchange": true})
}

func (s *Server) CloseInvalidConnections() {
	st := s.Store.View()
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, conns := range s.connections {
		if strings.HasPrefix(key, "portal:") {
			parts := strings.Split(key, ":")
			if len(parts) == 3 {
				d := st.Devices[parts[1]]
				if !d.Revoked && d.ConnectEnabled && d.PortalSupported && d.ConnectID == parts[2] {
					continue
				}
			}
		}
		session, ok := st.Sessions[key]
		svc := st.Services[session.ServiceID]
		p, present := st.Principals[session.UserID]
		if ok && present && session.Expires > now && svc.Published && svc.Revision == session.Revision && store.Authorized(&st, p, svc) {
			continue
		}
		for c := range conns {
			_ = c.Close()
		}
		delete(s.connections, key)
	}
}

func (s *Server) CheckConfig() error {
	if c := s.Config.FRP; c != nil {
		if c.Binary == "" || c.RunDir == "" || net.ParseIP(c.ListenHost) == nil || s.Config.TunnelPort < 1024 || s.Config.TunnelPort > 65535 || !net.ParseIP(s.Config.ProxyHost).IsLoopback() {
			return errors.New("invalid managed FRP configuration or non-loopback proxy host")
		}
	}
	if c := s.Config.EasyTier; c != nil {
		if c.Binary == "" || c.RunDir == "" || net.ParseIP(c.ListenHost) == nil || c.PortStart < 1024 || c.PortEnd < c.PortStart || c.PortEnd > 65535 {
			return errors.New("invalid EasyTier configuration")
		}
	}
	if s.Config.Certificate == "" || s.Config.PrivateKey == "" {
		return fmt.Errorf("TLS certificate and private key are required")
	}
	host, _, err := net.SplitHostPort(s.Config.HookListen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("frp authorization listener must use a loopback IP")
	}
	return s.checkUnifiedCertificate()
}
