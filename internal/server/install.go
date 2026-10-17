package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

const pluginEndpointMarker = "<!-- mfconnect-endpoint-contract:2 -->"

func (s *Server) installerReady(r *http.Request) bool {
	if s.Config.PluginManifest == "" {
		return false
	}
	if s.apiBase(r) != s.Config.APIBaseURL() || s.Config.EntryOrigin == "" {
		return true
	}
	manifest, err := os.ReadFile(s.Config.PluginManifest)
	return err == nil && len(manifest) <= 262144 && strings.Contains(string(manifest), pluginEndpointMarker)
}

func (s *Server) createInstallation(w http.ResponseWriter, r *http.Request, p store.Principal) {
	if r.Method != "POST" {
		reject(w, 405, "method_not_allowed")
		return
	}
	if !p.Admin {
		reject(w, 403, "forbidden")
		return
	}
	if s.Config.PluginManifest == "" || !validConnectHost(s.Config.APIHost) {
		reject(w, 503, "installers_not_configured")
		return
	}
	if !s.installerReady(r) {
		reject(w, 503, "installer_contract_2_required")
		return
	}
	var in struct {
		Owner string `json:"owner"`
	}
	if body(r, &in) != nil {
		reject(w, 400, "invalid_request")
		return
	}
	token := core.Random()
	expires := time.Now().Unix() + 1800
	err := s.Store.Update(func(st *store.State) error {
		if in.Owner == "" {
			in.Owner = core.ID("owner")
			st.Principals[in.Owner] = store.Principal{ID: in.Owner, TokenHash: core.Hash(core.Random())}
		}
		if _, ok := st.Principals[in.Owner]; !ok {
			return errors.New("unknown owner")
		}
		st.Enrollments[core.Hash(token)] = store.Enrollment{Owner: in.Owner, Expires: expires, Install: true}
		return nil
	})
	if err != nil {
		reject(w, 400, "invalid_owner")
		return
	}
	respond(w, 201, map[string]any{"install_url": s.apiBase(r) + "/v1/install/" + token + "/mfconnect.plg", "expires_at": expires})
}
func (s *Server) installManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		reject(w, 405, "method_not_allowed")
		return
	}
	if !s.installerReady(r) {
		reject(w, 503, "installer_contract_2_required")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/install/")
	token, rest, ok := strings.Cut(path, "/")
	if !ok || rest != "mfconnect.plg" || len(token) != 43 {
		reject(w, 404, "installer_not_found")
		return
	}
	i, ok := s.Store.View().Enrollments[core.Hash(token)]
	if !ok || !i.Install || i.Expires <= time.Now().Unix() {
		reject(w, 404, "installer_expired")
		return
	}
	manifest, err := os.ReadFile(s.Config.PluginManifest)
	if err != nil || len(manifest) > 262144 || strings.Count(string(manifest), "</PLUGIN>") != 1 {
		reject(w, 503, "installer_unavailable")
		return
	}
	// Unraid stores the manifest under the stable mfconnect.plg name. Enrollment
	// is consumed once; subsequent boots leave the already-enrolled NAS intact.
	provision := "--server 'https://" + s.Config.APIHost + "'"
	if s.Config.EntryOrigin != "" && s.apiBase(r) == s.Config.APIBaseURL() {
		provision = "--entry '" + s.Config.EntryOrigin + "'"
	}
	script := fmt.Sprintf(`<FILE Run="/bin/bash"><INLINE><![CDATA[set -euo pipefail
umask 077
/usr/local/lib/mfconnect/current/mfconnectd provision %s <<'MFCONNECT_PROVISION'
%s
MFCONNECT_PROVISION
/etc/rc.d/rc.mfconnect restart
]]></INLINE></FILE>
`, provision, token)
	out := strings.Replace(string(manifest), "</PLUGIN>", script+"</PLUGIN>", 1)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Disposition", `attachment; filename="mfconnect.plg"`)
	if r.Method == "GET" {
		w.Write([]byte(out))
	}
}
