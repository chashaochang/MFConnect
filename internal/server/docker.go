package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

func validateDocker(in core.SyncRequest) error {
	if in.Docker == nil {
		return nil
	}
	if err := in.Docker.Validate(); err != nil {
		return err
	}
	targets := map[string]core.Target{}
	for _, t := range in.Targets {
		targets[t.Key] = t
	}
	for _, a := range in.Docker.Applications {
		if a.State == "ready" {
			t, ok := targets[a.TargetKey]
			if !ok || t.URL != a.TargetURL {
				return errors.New("docker target mismatch")
			}
		}
	}
	return nil
}

func dockerKeys(r *core.DockerReport) map[string]bool {
	out := map[string]bool{}
	if r != nil {
		for _, a := range r.Applications {
			out[a.TargetKey] = true
		}
	}
	return out
}

func (s *Server) dockerInventory(st store.State, d store.Device, requests ...*http.Request) map[string]any {
	apps := []map[string]any{}
	state := "unsupported"
	scanned := int64(0)
	online := !d.Revoked && time.Now().Unix()-d.LastSeen < 45
	fresh := online && d.Docker.Fresh()
	services := map[string]store.Service{}
	for _, svc := range st.Services {
		if svc.DeviceID == d.ID {
			services[svc.Target.Key] = svc
		}
	}
	if d.Docker != nil {
		state = d.Docker.State
		scanned = d.Docker.ScannedAt
		for _, a := range d.Docker.Applications {
			entry := map[string]any{"application_id": a.ID, "container_id": a.ContainerID, "name": a.Name, "identity_kind": a.IdentityKind, "state": a.State, "running": a.Running, "target_key": a.TargetKey, "entry_path": a.EntryPath, "available": fresh && d.Docker.Available(a.TargetKey), "published": false, "relay_enabled": false, "service_id": "", "origin": ""}
			if svc, ok := services[a.TargetKey]; ok && svc.DockerManaged {
				entry["service_id"] = svc.ID
				entry["origin"] = s.origin(svc, requests...)
				entry["published"] = svc.Published
				entry["relay_enabled"] = fresh && store.DockerAvailable(d, svc) && svc.Published && store.RemoteEnabled(d)
			}
			apps = append(apps, entry)
		}
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i]["application_id"].(string) < apps[j]["application_id"].(string) })
	return map[string]any{"schema_version": 1, "publication_mode": "automatic", "device_id": d.ID, "state": state, "online": online, "fresh": fresh, "scanned_at": scanned, "applications": apps}
}

func (s *Server) dockerRoute(w http.ResponseWriter, r *http.Request, p store.Principal) bool {
	if !strings.HasPrefix(r.URL.Path, "/v1/devices/") || !strings.HasSuffix(r.URL.Path, "/docker-apps") {
		return false
	}
	if r.Method != "GET" {
		reject(w, 405, "method_not_allowed")
		return true
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/devices/"), "/docker-apps")
	st := s.Store.View()
	if !store.DeviceAuthorized(&st, p, id) {
		reject(w, 404, "device_not_found")
		return true
	}
	out := s.dockerInventory(st, st.Devices[id], r)
	container, app := r.URL.Query().Get("container_id"), r.URL.Query().Get("application_id")
	if container != "" || app != "" {
		if (container != "" && app != "") || (container != "" && (len(container) < 12 || len(container) > 64 || strings.Trim(container, "0123456789abcdef") != "")) || (app != "" && !core.Identifier.MatchString(app)) {
			reject(w, 400, "invalid_filter")
			return true
		}
		matches := []map[string]any{}
		for _, a := range out["applications"].([]map[string]any) {
			if (container != "" && strings.HasPrefix(a["container_id"].(string), container)) || (app != "" && a["application_id"] == app) {
				matches = append(matches, a)
			}
		}
		if len(matches) > 1 {
			reject(w, 409, "ambiguous_container_id")
			return true
		}
		out["applications"] = matches
	}
	respond(w, 200, out)
	return true
}
