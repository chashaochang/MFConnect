package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
	"mfconnect/internal/updates"
)

func updateFixture(t *testing.T) *fixture {
	t.Helper()
	f := setup(t, "")
	root := t.TempDir()
	f.app.Config.Updates = &UpdateConfig{Catalog: filepath.Join(root, "catalog.json"), ControlDir: root, Owner: "alice"}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	f.app.releaseKey = base64.StdEncoding.EncodeToString(pub)
	catalog := updates.Catalog{}
	for _, component := range []string{"plugin", "server"} {
		release := updates.Release{ID: component + "-9.0.0", Component: component, Version: "9.0.0", Architecture: "linux/amd64", Protocol: 1, URL: "https://release.example/9.0.0.tar.gz", SHA256: strings.Repeat("a", 64), Size: 123, IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 3600}
		if component == "plugin" {
			release.URL = "https://release.example/mfconnect-9.0.0.plg"
		} else {
			release.Image = "mfconnect-server:9.0.0-docker.1"
			release.ImageID = "sha256:" + strings.Repeat("b", 64)
		}
		payload, _ := json.Marshal(release)
		catalog.Releases = append(catalog.Releases, updates.SignedRelease{Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
	}
	if e := updates.Write(f.app.Config.Updates.Catalog, catalog); e != nil {
		t.Fatal(e)
	}
	if e := updates.Write(filepath.Join(root, "update-heartbeat.json"), updates.Heartbeat{Protocol: 1, UpdatedAt: time.Now().Unix()}); e != nil {
		t.Fatal(e)
	}
	if e := f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.LastSeen = time.Now().Unix()
		d.Software = &updates.Software{Plugin: "0.3.1", Connector: core.Version, UpdateProtocol: 1}
		st.Devices[d.ID] = d
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	return f
}
func TestUpdatePermissionsCatalogAndIdempotency(t *testing.T) {
	f := updateFixture(t)
	host := "api.example.test"
	path := "/v1/devices/dev_one/updates"
	for _, tc := range []struct {
		method, path, token string
		code                int
	}{{"GET", path, "", 401}, {"GET", path, "bob-token", 404}, {"POST", "/v1/updates/server", "bob-token", 403}} {
		w := invoke(f.app, tc.method, host, tc.path, tc.token, map[string]string{})
		if w.Code != tc.code {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	check := invoke(f.app, "GET", host, path, "alice-token", nil)
	var state map[string]any
	json.Unmarshal(check.Body.Bytes(), &state)
	if check.Code != 200 || state["update_available"] != true || state["executor_online"] != true {
		t.Fatal(check.Body.String())
	}
	in := map[string]string{"release_id": "plugin-9.0.0", "request_id": "request_123"}
	w := invoke(f.app, "POST", host, path, "alice-token", in)
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var j updates.Job
	json.Unmarshal(w.Body.Bytes(), &j)
	if strings.Contains(w.Body.String(), "signature") || strings.Contains(w.Body.String(), "payload") {
		t.Fatal("internal release leaked")
	}
	retry := invoke(f.app, "POST", host, path, "alice-token", in)
	if retry.Code != 200 || !strings.Contains(retry.Body.String(), j.ID) {
		t.Fatal(retry.Body.String())
	}
	in["request_id"] = "other_request"
	conflict := invoke(f.app, "POST", host, path, "alice-token", in)
	if conflict.Code != 409 {
		t.Fatal(conflict.Body.String())
	}
	if invoke(f.app, "GET", host, "/v1/update-jobs/"+j.ID, "bob-token", nil).Code != 404 {
		t.Fatal("cross-owner job disclosed")
	}
	in["release_id"] = "server-9.0.0"
	if invoke(f.app, "POST", host, "/v1/updates/server", "alice-token", in).Code != 409 {
		t.Fatal("concurrent server and device update accepted")
	}
	var response core.SyncResponse
	_ = f.st.Update(func(st *store.State) error { applyDeviceUpdate(st, st.Devices["dev_one"], nil, &response); return nil })
	if response.UpdateJob == nil || response.UpdateJob.ID != j.ID {
		t.Fatal("job not delivered via authenticated sync")
	}
	_ = f.st.Update(func(st *store.State) error {
		applyDeviceUpdate(st, st.Devices["dev_one"], &updates.Report{JobID: j.ID, State: "succeeded", InstalledVersion: "0.3.1"}, &core.SyncResponse{})
		return nil
	})
	if f.st.View().UpdateJobs[j.ID].State != "queued" {
		t.Fatal("false success accepted")
	}
	_ = f.st.Update(func(st *store.State) error {
		applyDeviceUpdate(st, st.Devices["dev_one"], &updates.Report{JobID: j.ID, State: "succeeded", InstalledVersion: "9.0.0"}, &core.SyncResponse{})
		return nil
	})
	if f.st.View().UpdateJobs[j.ID].State != "succeeded" {
		t.Fatal("completion not persisted")
	}
	cloud := invoke(f.app, "POST", host, "/v1/updates/server", "alice-token", in)
	if cloud.Code != 202 {
		t.Fatal(cloud.Body.String())
	}
	var queued updates.Job
	if updates.Read(filepath.Join(f.app.Config.Updates.ControlDir, "update-request.json"), &queued) != nil || queued.Component != "server" {
		t.Fatal("cloud queue missing")
	}
}
func TestUpdateOfflineExpiredInvalidCatalogAndOldDevice(t *testing.T) {
	f := updateFixture(t)
	path := "/v1/devices/dev_one/updates"
	_ = f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.Software = nil
		st.Devices[d.ID] = d
		return nil
	})
	in := map[string]string{"release_id": "plugin-9.0.0", "request_id": "request_123"}
	w := invoke(f.app, "POST", "api.example.test", path, "alice-token", in)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "offline") {
		t.Fatal(w.Body.String())
	}
	updates.Write(f.app.Config.Updates.Catalog, updates.Catalog{Releases: []updates.SignedRelease{{Payload: "invalid", Signature: "invalid"}}})
	check := invoke(f.app, "GET", "api.example.test", path, "alice-token", nil)
	if !strings.Contains(check.Body.String(), "no_verified_release") || !strings.Contains(check.Body.String(), `"update_available":false`) {
		t.Fatal(check.Body.String())
	}
	_ = f.st.Update(func(st *store.State) error {
		st.UpdateJobs["old"] = updates.Job{ID: "old", Component: "plugin", State: "queued", ExpiresAt: time.Now().Unix() - 1}
		return nil
	})
	f.app.refreshUpdates()
	if f.st.View().UpdateJobs["old"].State != "failed" {
		t.Fatal("expired queue remained active")
	}
}
