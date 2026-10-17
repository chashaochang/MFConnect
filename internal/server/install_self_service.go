package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

func activeDeviceCount(st *store.State) int {
	n := 0
	for _, d := range st.Devices {
		if !d.Revoked {
			n++
		}
	}
	return n
}

// Self-service installation only creates an isolated new NAS owner. It never
// grants access to existing devices, enrollment administration or server updates.
func (s *Server) installationPublicRoute(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/v1/installation-policy" {
		if r.Method != "GET" {
			reject(w, 405, "method_not_allowed")
			return true
		}
		respond(w, 200, map[string]any{"schema_version": 1, "mode": s.Config.InstallMode, "configured": s.installerReady(r), "claim_path": "/v1/installations/claim", "claim_url": s.apiBase(r) + "/v1/installations/claim", "api_base_url": s.apiBase(r), "install_ttl_seconds": 1800, "requires_invite": false})
		return true
	}
	if r.URL.Path != "/v1/installations/claim" {
		return false
	}
	if r.Method != "POST" {
		reject(w, 405, "method_not_allowed")
		return true
	}
	if s.Config.InstallMode != "self_service" {
		reject(w, 403, "self_install_disabled")
		return true
	}
	if s.Config.PluginManifest == "" {
		reject(w, 503, "installers_not_configured")
		return true
	}
	if !s.installerReady(r) {
		reject(w, 503, "installer_contract_2_required")
		return true
	}
	var in struct {
		RequestID string `json:"request_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if body(r, &in) != nil || len(in.RequestID) < 32 || !core.Identifier.MatchString(in.RequestID) {
		reject(w, 400, "invalid_request")
		return true
	}
	key := core.Hash(in.RequestID)
	expires := int64(0)
	replayed := false
	code := "storage_error"
	token := ""
	err := s.Store.Update(func(st *store.State) error {
		now := time.Now().Unix()
		install, known := st.Installations[key]
		if known {
			if install.Consumed {
				code = "installation_consumed"
				return errors.New(code)
			}
			if install.Expires <= now {
				code = "installation_expired"
				return errors.New(code)
			}
			enrollment, exists := st.Enrollments[install.EnrollmentHash]
			if !exists || enrollment.Expires <= now {
				code = "installation_expired"
				return errors.New(code)
			}
			replayed = true
		} else {
			if activeDeviceCount(st) >= s.Config.InstallDeviceLimit {
				code = "device_limit_reached"
				return errors.New(code)
			}
			pending, retained := 0, 0
			for _, i := range st.Installations {
				if !i.Consumed && i.Expires > now {
					pending++
				}
				if i.Expires+86400 > now {
					retained++
				}
			}
			if pending >= 64 || retained >= 1024 {
				code = "installation_rate_limited"
				return errors.New(code)
			}
			install = store.Installation{Owner: core.ID("owner"), Expires: now + 1800}
		}
		if st.InstallKey == "" {
			st.InstallKey = core.Random()
		}
		mac := hmac.New(sha256.New, []byte(st.InstallKey))
		mac.Write([]byte("mfconnect-self-install-v1\n" + in.RequestID))
		token = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		expires = install.Expires
		if !replayed {
			install.EnrollmentHash = core.Hash(token)
			st.Installations[key] = install
			st.Principals[install.Owner] = store.Principal{ID: install.Owner, TokenHash: core.Hash(core.Random())}
			st.Enrollments[install.EnrollmentHash] = store.Enrollment{Owner: install.Owner, Expires: expires, Install: true, InstallRequest: key}
		}
		return nil
	})
	if err != nil {
		status := 500
		switch code {
		case "installation_consumed", "device_limit_reached":
			status = 409
		case "installation_expired":
			status = 410
		case "installation_rate_limited":
			status = 429
			w.Header().Set("Retry-After", "60")
		}
		reject(w, status, code)
		return true
	}
	status := 201
	if replayed {
		status = 200
	}
	respond(w, status, map[string]any{"install_url": s.apiBase(r) + "/v1/install/" + token + "/mfconnect.plg", "expires_at": expires, "replayed": replayed})
	return true
}
