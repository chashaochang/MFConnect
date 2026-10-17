package server

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
	"mfconnect/internal/updates"
)

type UpdateConfig struct {
	DeviceID   string `json:"device_id,omitempty"`
	Catalog    string `json:"catalog"`
	ControlDir string `json:"control_dir"`
	Owner      string `json:"owner,omitempty"`
}

func updateReport(st *store.State, report *updates.Report, component, device string) {
	if report == nil || !updates.ValidReport(*report) {
		return
	}
	j, ok := st.UpdateJobs[report.JobID]
	if !ok || j.Component != component || j.DeviceID != device || updates.Terminal(j.State) {
		return
	}
	if report.State == "succeeded" && report.InstalledVersion != j.Version {
		return
	}
	if report.State == "rolled_back" && report.InstalledVersion != j.PreviousVersion {
		return
	}
	j.State, j.Error, j.UpdatedAt = report.State, report.Error, time.Now().Unix()
	st.UpdateJobs[j.ID] = j
}
func expireJobs(st *store.State) {
	now := time.Now().Unix()
	for id, j := range st.UpdateJobs {
		if !updates.Terminal(j.State) && j.ExpiresAt < now {
			j.State = "failed"
			j.Error = "job_expired"
			j.UpdatedAt = now
			st.UpdateJobs[id] = j
		}
		if updates.Terminal(j.State) && j.UpdatedAt < now-30*86400 {
			delete(st.UpdateJobs, id)
		}
	}
}
func applyDeviceUpdate(st *store.State, d store.Device, report *updates.Report, out *core.SyncResponse) {
	updateReport(st, report, "plugin", d.ID)
	expireJobs(st)
	for _, j := range st.UpdateJobs {
		if j.Component == "plugin" && j.DeviceID == d.ID && !updates.Terminal(j.State) {
			copy := j
			out.UpdateJob = &copy
			return
		}
	}
}
func (s *Server) refreshUpdates() {
	var report updates.Report
	if s.Config.Updates != nil {
		_ = updates.Read(filepath.Join(s.Config.Updates.ControlDir, "update-status.json"), &report)
	}
	_ = s.Store.Update(func(st *store.State) error { updateReport(st, &report, "server", ""); expireJobs(st); return nil })
}
func latestJob(st store.State, component, device string) *updates.Job {
	var out *updates.Job
	for _, j := range st.UpdateJobs {
		if j.Component == component && j.DeviceID == device && (out == nil || j.CreatedAt > out.CreatedAt) {
			copy := j
			out = &copy
		}
	}
	return out
}
func publicJob(j updates.Job) map[string]any {
	return map[string]any{"id": j.ID, "component": j.Component, "device_id": j.DeviceID, "release_id": j.ReleaseID, "version": j.Version, "previous_version": j.PreviousVersion, "state": j.State, "error": j.Error, "created_at": j.CreatedAt, "updated_at": j.UpdatedAt, "expires_at": j.ExpiresAt}
}
func (s *Server) updateRoute(w http.ResponseWriter, r *http.Request, p store.Principal) bool {
	path := r.URL.Path
	component, device := "", ""
	if path == "/v1/updates/server" {
		component = "server"
	}
	if strings.HasPrefix(path, "/v1/devices/") && strings.HasSuffix(path, "/updates") {
		device = strings.TrimSuffix(strings.TrimPrefix(path, "/v1/devices/"), "/updates")
		component = "plugin"
	}
	if strings.HasPrefix(path, "/v1/update-jobs/") {
		if r.Method != "GET" {
			reject(w, 405, "method_not_allowed")
			return true
		}
		s.refreshUpdates()
		st := s.Store.View()
		j, ok := st.UpdateJobs[strings.TrimPrefix(path, "/v1/update-jobs/")]
		allowed := p.Admin || j.RequestedBy == p.ID
		if j.Component == "plugin" {
			d := st.Devices[j.DeviceID]
			allowed = store.DeviceAuthorized(&st, p, d.ID)
		}
		if !ok || !allowed {
			reject(w, 404, "update_job_not_found")
			return true
		}
		respond(w, 200, publicJob(j))
		return true
	}
	if component == "" {
		return false
	}
	if r.Method != "GET" && r.Method != "POST" {
		reject(w, 405, "method_not_allowed")
		return true
	}
	s.refreshUpdates()
	st := s.Store.View()
	current := core.Version
	permitted := false
	online := false
	if component == "plugin" {
		d, ok := st.Devices[device]
		if !ok || !store.DeviceAuthorized(&st, p, device) {
			reject(w, 404, "device_not_found")
			return true
		}
		permitted = true
		current = ""
		if d.Software != nil {
			current = d.Software.Plugin
			online = time.Now().Unix()-d.LastSeen < 45 && d.Software.UpdateProtocol == updates.Protocol
		}
	} else if c := s.Config.Updates; c != nil {
		permitted = p.Admin || (c.Owner != "" && p.ID == c.Owner) || (c.DeviceID != "" && p.DeviceID == c.DeviceID && p.OwnerID == c.Owner && store.DeviceAuthorized(&st, p, p.DeviceID))
		var beat updates.Heartbeat
		online = updates.Read(filepath.Join(c.ControlDir, "update-heartbeat.json"), &beat) == nil && beat.Protocol == updates.Protocol && time.Now().Unix()-beat.UpdatedAt < 35
	}
	signed := updates.SignedRelease{}
	release := updates.Release{}
	catalogError := "updates_not_configured"
	if s.Config.Updates != nil {
		var e error
		signed, release, e = updates.LatestWithKey(s.Config.Updates.Catalog, component, s.releaseKey)
		catalogError = ""
		if e != nil {
			catalogError = e.Error()
		}
	}
	job := latestJob(st, component, device)
	available := catalogError == "" && updates.ValidVersion(current) && updates.Compare(release.Version, current) > 0
	if r.Method == "GET" {
		data := map[string]any{"schema_version": 1, "component": component, "current_version": current, "latest": nil, "update_available": available, "can_update": permitted, "executor_online": online, "check_error": catalogError, "checked_at": time.Now().Unix(), "latest_job": nil}
		if component == "plugin" {
			data["device_id"] = device
		}
		if release.ID != "" {
			data["latest"] = map[string]any{"release_id": release.ID, "version": release.Version, "notes": release.Notes, "size": release.Size, "requires_restart": true}
		}
		if job != nil && (component == "plugin" || permitted) {
			data["latest_job"] = publicJob(*job)
		}
		respond(w, 200, data)
		return true
	}
	if !permitted {
		reject(w, 403, "update_forbidden")
		return true
	}
	var in struct {
		ReleaseID string `json:"release_id"`
		RequestID string `json:"request_id"`
	}
	if body(r, &in) != nil || !core.Identifier.MatchString(in.RequestID) || len(in.RequestID) < 8 {
		reject(w, 400, "invalid_update_request")
		return true
	}
	// 同一用户的幂等请求即使版本已经更新，也返回原任务。
	for _, old := range st.UpdateJobs {
		if old.RequestID == in.RequestID && old.RequestedBy == p.ID {
			if old.Component != component || old.DeviceID != device || old.ReleaseID != in.ReleaseID {
				reject(w, 409, "idempotency_conflict")
			} else {
				respond(w, 200, publicJob(old))
			}
			return true
		}
	}
	if catalogError != "" {
		reject(w, 503, catalogError)
		return true
	}
	if in.ReleaseID != release.ID {
		reject(w, 409, "release_changed")
		return true
	}
	if !online {
		reject(w, 409, "update_executor_offline")
		return true
	}
	if !available {
		reject(w, 409, "already_current_or_version_unknown")
		return true
	}
	now := time.Now().Unix()
	next := updates.Job{ID: core.ID("upd"), Component: component, DeviceID: device, RequestedBy: p.ID, RequestID: in.RequestID, ReleaseID: release.ID, Version: release.Version, PreviousVersion: current, State: "queued", CreatedAt: now, UpdatedAt: now, ExpiresAt: now + 1800, Release: signed}
	e := s.Store.Update(func(state *store.State) error {
		for _, j := range state.UpdateJobs {
			if j.RequestID == in.RequestID && j.RequestedBy == p.ID {
				return errors.New("idempotency_conflict")
			}
			if !updates.Terminal(j.State) && (j.Component == "server" || component == "server" || j.DeviceID == device) {
				return errors.New("update_in_progress")
			}
		}
		if len(state.UpdateJobs) >= 1000 {
			return errors.New("update_history_full")
		}
		state.UpdateJobs[next.ID] = next
		return nil
	})
	if e != nil {
		reject(w, 409, e.Error())
		return true
	}
	if component == "server" {
		if e = updates.Write(filepath.Join(s.Config.Updates.ControlDir, "update-request.json"), next); e != nil {
			_ = s.Store.Update(func(st *store.State) error {
				j := st.UpdateJobs[next.ID]
				j.State = "failed"
				j.Error = "update_queue_unavailable"
				st.UpdateJobs[j.ID] = j
				return nil
			})
			reject(w, 503, "update_queue_unavailable")
			return true
		}
	}
	respond(w, 202, publicJob(next))
	return true
}
