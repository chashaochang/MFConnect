package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"mfconnect/internal/store"
)

// Only the NAS device key can call this endpoint. The plugin's local HTTP
// adapter additionally requires an authenticated native administrator session.
func (s *Server) deviceConnect(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil || len(b) > 65536 {
		reject(w, 400, "invalid_request")
		return
	}
	d, err := s.authenticateDevice(r, b)
	if err != nil {
		reject(w, 401, "invalid_device_signature")
		return
	}
	var in struct {
		BrowserPublicDirect *bool  `json:"browser_public_direct"`
		Ticket              string `json:"ticket"`
		Nonce               string `json:"nonce"`
		SourceOrigin        string `json:"source_origin"`
		CredentialHash      string `json:"credential_hash"`
		Action              string `json:"action"`
		ID                  string `json:"id"`
		Enabled             *bool  `json:"enabled"`
		ServiceID           string `json:"service_id"`
		Published           *bool  `json:"published"`
		Host                string `json:"host"`
	}
	if json.Unmarshal(b, &in) != nil {
		reject(w, 400, "invalid_request")
		return
	}
	switch in.Action {
	case "public_bootstrap":
		s.publicBootstrap(w, r, d, in.Host, in.Ticket, in.Nonce, in.SourceOrigin)
	case "public_portal_access":
		if !s.publicPortalAllowed(d, in.Host) {
			reject(w, 403, "portal_disabled")
			return
		}
		respond(w, 200, map[string]any{"allowed": true, "strip_authorization": s.controlCredential(in.CredentialHash)})
	case "endpoints":
		w.Header().Set("X-MF-Endpoint-Contract", EndpointContract)
		out := s.endpointInfo(r)
		out["device_id"], out["connect"] = d.ID, s.connectInfo(d, r)
		state := s.Store.View()
		out["docker"] = s.dockerInventory(state, d, r)
		services := []map[string]any{}
		for _, svc := range state.Services {
			if svc.DeviceID == d.ID {
				services = append(services, map[string]any{"id": svc.ID, "key": svc.Target.Key, "name": svc.Target.Name, "origin": s.origin(svc, r), "published": svc.Published})
			}
		}
		out["services"] = services
		respond(w, 200, out)
	case "status":
		respond(w, 200, s.connectInfo(d, r))
	case "session":
		s.issueDeviceSession(w, r, d)
	case "docker_apps":
		respond(w, 200, s.dockerInventory(s.Store.View(), d, r))
	case "configure":
		if in.Enabled == nil {
			reject(w, 400, "invalid_request")
			return
		}
		_, code := s.setConnect(d.ID, in.ID, *in.Enabled, in.BrowserPublicDirect)
		if code != "" {
			connectError(w, code)
		} else {
			respond(w, 200, s.connectInfo(s.Store.View().Devices[d.ID], r))
		}
	case "portal_access":
		if !d.ConnectEnabled || !d.PortalSupported || d.ConnectID == "" || s.Config.ConnectHost == "" || !s.portalHost(d, in.Host) {
			reject(w, 403, "portal_disabled")
			return
		}
		respond(w, 200, map[string]bool{"allowed": true})
	case "services", "overview":
		out := []map[string]any{}
		for _, svc := range s.Store.View().Services {
			if svc.DeviceID == d.ID {
				out = append(out, map[string]any{"id": svc.ID, "device_id": d.ID, "key": svc.Target.Key, "name": svc.Target.Name, "origin": s.origin(svc, r), "published": svc.Published, "revision": svc.Revision, "docker_managed": svc.DockerManaged})
			}
		}
		if in.Action == "overview" {
			respond(w, 200, map[string]any{"connect": s.connectInfo(d, r), "services": out, "docker": s.dockerInventory(s.Store.View(), d, r)})
		} else {
			respond(w, 200, out)
		}
	case "publish":
		if in.Published == nil {
			reject(w, 400, "invalid_request")
			return
		}
		err := s.Store.Update(func(st *store.State) error {
			current := st.Devices[d.ID]
			svc, ok := st.Services[in.ServiceID]
			if !ok || current.Revoked || svc.DeviceID != d.ID {
				return errors.New("forbidden")
			}
			if svc.DockerManaged {
				return errors.New("docker_automatically_managed")
			}
			if svc.Published != *in.Published {
				svc.Revision++
				svc.Published = *in.Published
				st.Services[svc.ID] = svc
				invalidate(st, svc.ID)
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
	default:
		reject(w, 400, "invalid_action")
	}
}
