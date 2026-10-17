package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

func TestDockerSyncStableServiceAndAccessLifecycle(t *testing.T) {
	f := setup(t, "")
	f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.ConnectEnabled = true
		st.Devices[d.ID] = d
		return nil
	})
	target := f.st.View().Services["svc_one"].Target
	report := &core.DockerReport{Schema: 1, State: "ready", ScannedAt: time.Now().Unix(), Applications: []core.DockerApplication{{ID: "app_one", ContainerID: strings.Repeat("a", 64), Name: "Web", IdentityKind: "unraid_template", State: "ready", Running: true, TargetKey: target.Key, TargetURL: target.URL, EntryPath: "/web/?a=1"}}}
	sync := func(targets []core.Target) {
		t.Helper()
		w := signedCall(f, "dev_one", "/v1/device/sync", core.SyncRequest{Targets: targets, Docker: report})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	sync([]core.Target{target})
	original := f.st.View().Services["svc_one"]
	if !original.DockerManaged || !original.DockerAvailable || !original.Published {
		t.Fatal("existing service lost", original)
	}
	cookie := session(t, f)
	target.URL = "http://192.168.1.99:8080"
	report.Applications[0].TargetURL = target.URL
	report.Applications[0].ContainerID = strings.Repeat("b", 64)
	sync([]core.Target{target})
	state := f.st.View()
	rebuilt := state.Services["svc_one"]
	if rebuilt.ID != original.ID || rebuilt.HostKey != original.HostKey || !rebuilt.Published || rebuilt.Revision == original.Revision {
		t.Fatal("rebuild changed service or lost intent")
	}
	if _, ok := state.Sessions[core.Hash(cookie.Value)]; ok {
		t.Fatal("changed target retained old session")
	}
	for _, phase := range []string{"stopped", "unavailable", "removed", "omitted"} {
		report.State = "ready"
		report.Applications[0].State = "ready"
		report.Applications[0].Running = true
		targets := []core.Target{target}
		saved := report
		switch phase {
		case "stopped":
			report.Applications[0].State = "stopped"
			report.Applications[0].Running = false
		case "unavailable":
			report.State = "unavailable"
		case "removed":
			targets = nil
			report = &core.DockerReport{Schema: 1, State: "ready", ScannedAt: time.Now().Unix(), Applications: []core.DockerApplication{}}
		case "omitted":
			report = nil
		}
		sync(targets)
		state = f.st.View()
		svc := state.Services["svc_one"]
		if !svc.Published || store.Authorized(&state, state.Principals["alice"], svc) {
			t.Fatal("unavailable application published or intent lost", phase)
		}
		if invoke(f.app, "POST", "api.example.test", "/v1/services/svc_one/launches", "alice-token", map[string]string{"nonce_sha256": core.Hash(core.Random())}).Code != 403 {
			t.Fatal("unavailable launch", phase)
		}
		report = saved
	}
	report.State = "ready"
	report.Applications[0].State = "ready"
	report.Applications[0].Running = true
	sync([]core.Target{target})
	state = f.st.View()
	if !store.Authorized(&state, state.Principals["alice"], state.Services["svc_one"]) {
		t.Fatal("restart did not restore")
	}
	report.ScannedAt = time.Now().Unix() - 91
	sync([]core.Target{target})
	state = f.st.View()
	if store.Authorized(&state, state.Principals["alice"], state.Services["svc_one"]) {
		t.Fatal("stale discovery authorized")
	}
}

func TestDockerInventoryScopeFilteringAndNewDefaults(t *testing.T) {
	f := connectFixture(t)
	report := &core.DockerReport{Schema: 1, State: "ready", ScannedAt: time.Now().Unix(), Applications: []core.DockerApplication{{ID: "app_new", ContainerID: strings.Repeat("c", 64), Name: "New Web", IdentityKind: "compose", State: "ready", Running: true, TargetKey: "docker_new", TargetURL: "http://127.0.0.1:8000", EntryPath: "/ui/"}}}
	in := core.SyncRequest{Targets: []core.Target{{Key: "docker_new", Name: "New Web", URL: report.Applications[0].TargetURL}}, Docker: report}
	w := signedCall(f, "dev_one", "/v1/device/sync", in)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	path := "/v1/devices/dev_one/docker-apps"
	w = invoke(f.app, "GET", "api.example.test", path+"?container_id="+strings.Repeat("c", 12), "alice-token", nil)
	var out struct {
		Fresh bool `json:"fresh"`
		Apps  []struct {
			ID        string `json:"application_id"`
			Service   string `json:"service_id"`
			Path      string `json:"entry_path"`
			Published bool   `json:"published"`
			Available bool   `json:"available"`
			Relay     bool   `json:"relay_enabled"`
		} `json:"applications"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || !out.Fresh || len(out.Apps) != 1 || out.Apps[0].ID != "app_new" || out.Apps[0].Service == "" || out.Apps[0].Path != "/ui/" || out.Apps[0].Published || out.Apps[0].Relay || !out.Apps[0].Available {
		t.Fatal("bad automatic mapping", w.Body.String())
	}
	if invoke(f.app, "GET", "api.example.test", path, "bob-token", nil).Code != 404 {
		t.Fatal("cross-owner inventory exposed")
	}
	if invoke(f.app, "GET", "api.example.test", path, "", nil).Code != 401 {
		t.Fatal("anonymous inventory exposed")
	}
	if invoke(f.app, "GET", "api.example.test", path+"?container_id=c", "alice-token", nil).Code != 400 {
		t.Fatal("too-short match")
	}
	w = deviceConnectRequest(f, map[string]string{"action": "session"})
	var session struct {
		Token string `json:"access_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &session)
	if invoke(f.app, "GET", "api.example.test", path, session.Token, nil).Code != 200 || invoke(f.app, "GET", "api.example.test", "/v1/devices/dev_other/docker-apps", session.Token, nil).Code != 404 {
		t.Fatal("device session scope")
	}
	in.Targets[0].URL = "http://127.0.0.1:8001"
	if signedCall(f, "dev_one", "/v1/device/sync", in).Code != 400 {
		t.Fatal("target/report mismatch accepted")
	}
	f.st.Update(func(st *store.State) error {
		d := st.Devices["dev_one"]
		d.LastSeen = time.Now().Unix() - 46
		st.Devices[d.ID] = d
		return nil
	})
	w = invoke(f.app, "GET", "api.example.test", path, "alice-token", nil)
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Fresh || out.Apps[0].Available {
		t.Fatal("offline inventory fresh")
	}
}

func TestDockerAutomaticallyPublishesAndFollowsDeviceSwitch(t *testing.T) {
	f := connectFixture(t)
	report := &core.DockerReport{Schema: 1, State: "ready", ScannedAt: time.Now().Unix(), Applications: []core.DockerApplication{{ID: "app_new", ContainerID: strings.Repeat("f", 64), Name: "New", IdentityKind: "compose", State: "ready", Running: true, TargetKey: "docker_new", TargetURL: "http://127.0.0.1:8000", EntryPath: "/"}}}
	input := core.SyncRequest{Docker: report, Targets: []core.Target{{Key: "docker_new", URL: report.Applications[0].TargetURL}}}
	sync := func() {
		t.Helper()
		w := signedCall(f, "dev_one", "/v1/device/sync", input)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		f.st.Update(func(st *store.State) error {
			d := st.Devices["dev_one"]
			d.PortalSupported = true
			st.Devices[d.ID] = d
			return nil
		})
	}
	sync()
	var id, host string
	for _, svc := range f.st.View().Services {
		if svc.Target.Key == "docker_new" {
			id, host = svc.ID, svc.HostKey
			if svc.Published {
				t.Fatal("enabled before device switch")
			}
		}
	}
	if _, code := f.app.setConnect("dev_one", "home", true); code != "" {
		t.Fatal(code)
	}
	if !f.st.View().Services[id].Published {
		t.Fatal("enable requires manual publishing")
	}
	sync()
	if !f.st.View().Services[id].Published {
		t.Fatal("sync lost automatic state")
	}
	if w := invoke(f.app, "PATCH", "api.example.test", "/v1/services/"+id, "alice-token", map[string]bool{"published": false}); w.Code != 409 {
		t.Fatal("obsolete publish accepted", w.Code)
	}
	report.Applications[0].ContainerID = strings.Repeat("e", 64)
	sync()
	if svc := f.st.View().Services[id]; svc.HostKey != host || !svc.Published {
		t.Fatal("rebuild changed identity")
	}
	f.app.setConnect("dev_one", "home", false)
	if f.st.View().Services[id].Published {
		t.Fatal("off retained access")
	}
	f.app.setConnect("dev_one", "home", true)
	report.Applications = nil
	input.Targets = nil
	sync()
	if svc := f.st.View().Services[id]; svc.HostKey != host || svc.DockerAvailable {
		t.Fatal("deletion lost identity or left access")
	}
}
