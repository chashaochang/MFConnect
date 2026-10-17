package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

const EndpointContract = "2"

type originalURIKey struct{}
type legacyRootKey struct{}

// EntryHost accepts a DNS HTTPS origin, independently of the number of labels.
// Production entry URLs use the standard HTTPS port; API paths are not origins.
func EntryHost(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || u.Host != u.Hostname() || len(u.Host) > 216 || (!validConnectHost(u.Host) || net.ParseIP(u.Host) != nil) {
		return "", errors.New("entry_origin must be an HTTPS DNS origin without a port or path")
	}
	return u.Host, nil
}

func (c Config) EntryHost() string {
	if c.EntryOrigin == "" {
		return ""
	}
	h, _ := EntryHost(c.EntryOrigin)
	return h
}
func (c Config) APIBaseURL() string {
	if c.EntryOrigin != "" {
		return strings.TrimRight(c.EntryOrigin, "/") + "/api"
	}
	return "https://" + c.APIHost
}
func (s *Server) canonical(requests ...*http.Request) bool {
	if s.Config.EntryOrigin == "" || len(requests) == 0 || requests[0] == nil {
		return false
	}
	r := requests[0]
	legacy, _ := r.Context().Value(legacyRootKey{}).(bool)
	return (hostname(r.Host) == s.Config.EntryHost() && !legacy) || r.Header.Get("X-MF-Endpoint-Contract") == EndpointContract
}
func (s *Server) browserDomain(requests ...*http.Request) string {
	if s.canonical(requests...) {
		return s.Config.EntryHost()
	}
	return s.Config.ConnectHost
}
func (s *Server) appsDomain(requests ...*http.Request) string {
	if s.canonical(requests...) {
		return s.Config.EntryHost()
	}
	return s.Config.BaseDomain
}
func (s *Server) apiBase(r *http.Request) string {
	legacy, _ := r.Context().Value(legacyRootKey{}).(bool)
	if hostname(r.Host) == s.Config.EntryHost() && !legacy {
		return s.Config.APIBaseURL()
	}
	return "https://" + s.Config.APIHost
}

// The unified /api route selects v2 implicitly. A legacy route only selects v2
// with the explicit header; learning that a server supports v2 is not consent
// to change a persisted client's endpoint or response contract.
func (s *Server) negotiatedContract(r *http.Request) string {
	if s.canonical(r) || r.Header.Get("X-MF-Endpoint-Contract") == EndpointContract {
		return EndpointContract
	}
	return "1"
}

func (s *Server) connectEndpointInfo(r *http.Request) map[string]any {
	if s.negotiatedContract(r) == EndpointContract {
		return s.endpointInfo(r)
	}
	// Preserve the original v1 shape, including the absence of v2 descriptors.
	return map[string]any{"schema_version": 1, "api_origin": "https://" + s.Config.APIHost}
}

func (s *Server) endpointInfo(r *http.Request) map[string]any {
	base := s.apiBase(r)
	u, _ := url.Parse(base)
	entry := s.Config.EntryOrigin
	if entry == "" && s.Config.ConnectHost != "" {
		entry = "https://" + s.Config.ConnectHost
	}
	legacy := s.Config.APIHost != s.Config.EntryHost() || s.Config.LegacyAPIRoot
	return map[string]any{
		"schema_version": 2, "endpoint_contract": 2,
		"api_origin": u.Scheme + "://" + u.Host, "api_base_url": base,
		"entry_origin": entry, "canonical_api_base_url": s.Config.APIBaseURL(),
		"browser_domain": s.browserDomain(r), "apps_domain": s.appsDomain(r),
		"capabilities": map[string]any{"unified_entry": s.Config.EntryOrigin != "", "api_base_url": 1, "docker_discovery": 1, "docker_auto_publish": 1, "browser_public_direct": 1, "launch_navigation": 1, "updates": 1, "webdav": false, "self_install": 1, "legacy_api": legacy, "app_host_layout": "flat", "host_bound_sessions": true},
	}
}
func (s *Server) serviceHost(svc store.Service, host string) bool {
	return host == svc.HostKey+"."+s.Config.BaseDomain || (s.Config.EntryOrigin != "" && host == svc.HostKey+"."+s.Config.EntryHost())
}
func (s *Server) sessionHost(session store.Session, svc store.Service, host string) bool {
	// Pre-migration cookies remain valid only on the original application origin.
	if session.Host == "" {
		return host == svc.HostKey+"."+s.Config.BaseDomain
	}
	return session.Host == host
}
func (s *Server) portalHost(d store.Device, host string) bool {
	return d.ConnectID != "" && ((s.Config.ConnectHost != "" && host == d.ConnectID+"."+s.Config.ConnectHost) || (s.Config.EntryOrigin != "" && host == d.ConnectID+"."+s.Config.EntryHost()))
}
func (s *Server) connectDomain(host string) string {
	// A legacy browser root may itself be one level below the new entry.
	// Exact roots must win before interpreting any hostname as a NAS ID.
	for _, domain := range []string{s.Config.EntryHost(), s.Config.ConnectHost} {
		if domain != "" && host == domain {
			return domain
		}
	}
	for _, domain := range []string{s.Config.EntryHost(), s.Config.ConnectHost} {
		if domain != "" && (host == domain || (strings.HasSuffix(host, "."+domain) && !strings.Contains(strings.TrimSuffix(host, "."+domain), "."))) {
			return domain
		}
	}
	return ""
}
func (s *Server) routeAPI(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	host := hostname(r.Host)
	if host == s.Config.EntryHost() {
		if r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/") {
			if s.Config.LegacyAPIRoot && s.Config.APIHost == host && (r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/v1/")) {
				return r.WithContext(context.WithValue(r.Context(), legacyRootKey{}, true)), true
			}
			return r, false
		}
		if r.URL.EscapedPath() != r.URL.Path || path.Clean(r.URL.Path) != r.URL.Path || strings.Contains(r.URL.Path, "\\") {
			reject(w, 400, "ambiguous_api_path")
			return nil, true
		}
		r = r.Clone(context.WithValue(r.Context(), originalURIKey{}, r.URL.RequestURI()))
		u := *r.URL
		u.Path = strings.TrimPrefix(u.Path, "/api")
		if u.Path == "" {
			u.Path = "/"
		}
		r.URL = &u
		return r, true
	}
	return r, host == s.Config.APIHost
}
func signedURI(r *http.Request) string {
	if uri, ok := r.Context().Value(originalURIKey{}).(string); ok {
		return uri
	}
	return r.URL.RequestURI()
}

// Check all configured aliases before migration. No existing URL is silently
// reassigned to another NAS, application, or control endpoint.
func (s *Server) checkHostCollisions(st store.State) error {
	hosts := map[string]string{}
	add := func(host, owner string) error {
		if host == "" {
			return nil
		}
		if old, ok := hosts[host]; ok && old != owner {
			return fmt.Errorf("hostname collision %s (%s, %s)", host, old, owner)
		}
		hosts[host] = owner
		return nil
	}
	for _, host := range []string{s.Config.EntryHost(), s.Config.APIHost, s.Config.ConnectHost, s.Config.BaseDomain, s.Config.TunnelHost} {
		hosts[host] = "infrastructure"
	}
	for _, d := range st.Devices {
		if d.ConnectID == "" {
			continue
		}
		for _, domain := range []string{s.Config.EntryHost(), s.Config.ConnectHost} {
			if domain != "" {
				if err := add(d.ConnectID+"."+domain, "NAS "+d.ID); err != nil {
					return err
				}
			}
		}
	}
	for _, svc := range st.Services {
		for _, domain := range []string{s.Config.EntryHost(), s.Config.BaseDomain} {
			if domain != "" {
				if err := add(svc.HostKey+"."+domain, "application "+svc.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func newApplicationHost(st *store.State) string {
	for {
		// 36 characters cannot collide with a user-selected connection ID (<=32).
		key := "app-" + core.RandomHex()
		unique := true
		for _, svc := range st.Services {
			if svc.HostKey == key {
				unique = false
				break
			}
		}
		if unique {
			return key
		}
	}
}

// Applications may have their own Authorization header. Remove only credentials
// issued by this controller, including expired ones, before any proxy hop.
func (s *Server) stripControlToken(r *http.Request) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return
	}
	hash := core.Hash(fields[1])
	for _, p := range s.Store.View().Principals {
		if p.TokenHash == hash || p.RefreshHash == hash {
			r.Header.Del("Authorization")
			return
		}
	}
}
