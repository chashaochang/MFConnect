package connector

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mfconnect/internal/core"
)

func dockerFixture(char, name, mode, ip, webui string) dockerContainer {
	c := dockerContainer{ID: strings.Repeat(char, 64), Names: []string{"/" + name}, State: "running", Labels: map[string]string{"net.unraid.docker.webui": webui}}
	c.HostConfig.NetworkMode = mode
	c.NetworkSettings.Networks = map[string]dockerNetwork{mode: {IPAddress: ip}}
	return c
}
func TestDockerWebUIResolution(t *testing.T) {
	base := dockerFixture("a", "Jellyfin", "br0", "192.168.50.96", "http://[IP]:[PORT:8096]/web/index.html?lang=zh")
	tests := []struct {
		name                string
		c                   dockerContainer
		origin, path, state string
	}{
		{"independent IP and query", base, "http://192.168.50.96:8096", "/web/index.html?lang=zh", "ready"},
		{"host network", dockerFixture("b", "CookieCloud", "host", "", "http://[IP]:[PORT:8088]"), "http://127.0.0.1:8088", "/", "ready"},
		{"path without port", dockerFixture("b", "FileBrowser", "br0", "192.168.50.98", "http://[IP]/files/"), "http://192.168.50.98", "/files/", "ready"},
		{"ipv6", dockerFixture("b", "v6", "br0", "fd12::98", "http://[IP]:[PORT:8080]/"), "http://[fd12::98]:8080", "/", "ready"},
		{"missing WebUI", dockerFixture("b", "game", "host", "", ""), "", "/", "no_webui"},
		{"no address", dockerFixture("b", "unresolved", "none", "", "http://[IP]:[PORT:8080]"), "", "/", "network_unresolved"},
		{"credentials", dockerFixture("b", "bad", "host", "", "http://user:pass@[IP]/"), "", "/", "invalid_webui"},
		{"invalid port", dockerFixture("b", "bad", "host", "", "http://[IP]:[PORT:65536]/"), "", "/", "invalid_webui"},
		{"reserved path", dockerFixture("b", "bad", "host", "", "http://[IP]/_mf/bootstrap"), "", "/", "invalid_webui"},
	}
	bridge := dockerFixture("b", "bridge", "bridge", "172.17.0.2", "http://[IP]:[PORT:8080]/ui")
	bridge.Ports = []dockerPort{{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 18080, Type: "tcp"}, {IP: "::", PrivatePort: 8080, PublicPort: 18080, Type: "tcp"}}
	tests = append(tests, struct {
		name                string
		c                   dockerContainer
		origin, path, state string
	}{"published port", bridge, "http://127.0.0.1:18080", "/ui", "ready"})
	ambiguous := bridge
	ambiguous.Ports = append(append([]dockerPort{}, bridge.Ports...), dockerPort{IP: "192.168.50.3", PrivatePort: 8080, PublicPort: 28080, Type: "tcp"})
	tests = append(tests, struct {
		name                string
		c                   dockerContainer
		origin, path, state string
	}{"ambiguous port", ambiguous, "", "/", "network_unresolved"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origin, path, state := resolveDockerWebUI(tt.c, []dockerContainer{tt.c}, tt.c.Labels["net.unraid.docker.webui"])
			if origin != tt.origin || path != tt.path || state != tt.state {
				t.Fatalf("got %q %q %q", origin, path, state)
			}
		})
	}
	shared := dockerFixture("c", "shared", "container:"+bridge.ID, "", "http://[IP]:[PORT:8080]/ui")
	origin, _, state := resolveDockerWebUI(shared, []dockerContainer{bridge, shared}, shared.Labels["net.unraid.docker.webui"])
	if origin != "http://127.0.0.1:18080" || state != "ready" {
		t.Fatal("shared namespace", origin, state)
	}
	shared.HostConfig.NetworkMode = "container:" + shared.ID
	if _, _, state = resolveDockerWebUI(shared, []dockerContainer{shared}, shared.Labels["net.unraid.docker.webui"]); state != "network_unresolved" {
		t.Fatal("namespace cycle accepted")
	}
}

func TestDockerIdentityRebuildRenameAdoptionAndRemoval(t *testing.T) {
	c := dockerFixture("a", "jellyfin", "br0", "192.168.50.96", "http://[IP]:[PORT:8096]/web/")
	manual := []core.Target{{Key: "existing-jellyfin", Name: "Jellyfin", URL: "http://192.168.50.96:8096"}}
	templates := map[string]dockerTemplate{"jellyfin": {Name: "jellyfin", File: "my-jellyfin.xml"}}
	first, err := discoverDocker([]dockerContainer{c}, templates, manual, dockerCache{})
	if err != nil {
		t.Fatal(err)
	}
	original := first.Report.Applications[0]
	if original.TargetKey != manual[0].Key || original.IdentityKind != "unraid_template" {
		t.Fatal("manual service not adopted", original)
	}
	c.ID = strings.Repeat("b", 64)
	c.NetworkSettings.Networks["br0"] = dockerNetwork{IPAddress: "192.168.50.97"}
	second, err := discoverDocker([]dockerContainer{c}, templates, manual, first)
	if err != nil {
		t.Fatal(err)
	}
	app := second.Report.Applications[0]
	if app.ID != original.ID || app.TargetKey != original.TargetKey || app.State != "ready" || second.Targets[0].URL != "http://192.168.50.97:8096" {
		t.Fatal("rebuild lost identity", app)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	enabled := true
	core.WriteJSON(discoveryPath(path), second)
	config := Config{DockerDiscovery: &enabled, path: path, Targets: manual}
	if err := loadDocker(&config); err != nil || config.Targets[0].URL != "http://192.168.50.97:8096" {
		t.Fatal("adopted target did not follow address", err)
	}
	c.Names = []string{"/jellyfin-renamed"}
	renamed, err := discoverDocker([]dockerContainer{c}, nil, manual, second)
	if err != nil || renamed.Report.Applications[0].ID != original.ID {
		t.Fatal("same-container rename lost identity", err)
	}
	c.State = "exited"
	c.NetworkSettings.Networks = nil
	stopped, err := discoverDocker([]dockerContainer{c}, nil, manual, renamed)
	if err != nil || stopped.Report.Applications[0].State != "stopped" || stopped.Report.Available(original.TargetKey) {
		t.Fatal("stopped container available", err)
	}
	empty, err := discoverDocker(nil, nil, manual, stopped)
	if err != nil || len(empty.Report.Applications) != 0 || len(empty.Bindings) != 1 {
		t.Fatal("removal erased association", err)
	}
	c.State = "running"
	c.NetworkSettings.Networks = map[string]dockerNetwork{"br0": {IPAddress: "192.168.50.97"}}
	restored, err := discoverDocker([]dockerContainer{c}, nil, manual, empty)
	if err != nil || restored.Report.Applications[0].ID != original.ID {
		t.Fatal("return lost identity", err)
	}
}

func TestDockerComposeConflictAndNamesReused(t *testing.T) {
	a := dockerFixture("a", "first", "host", "", "http://[IP]:[PORT:8080]")
	b := dockerFixture("b", "second", "host", "", "http://[IP]:[PORT:8081]")
	first, err := discoverDocker([]dockerContainer{a}, nil, nil, dockerCache{})
	if err != nil {
		t.Fatal(err)
	}
	a.Names = []string{"/renamed"}
	b.Names = []string{"/first"}
	renamed, err := discoverDocker([]dockerContainer{a, b}, nil, nil, first)
	if err != nil || renamed.Report.Applications[0].ID == renamed.Report.Applications[1].ID {
		t.Fatal("name reuse merged identities", err)
	}
	for _, c := range []*dockerContainer{&a, &b} {
		c.Labels["com.docker.compose.project"] = "project"
		c.Labels["com.docker.compose.service"] = "service"
	}
	conflict, err := discoverDocker([]dockerContainer{a, b}, nil, nil, dockerCache{})
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range conflict.Report.Applications {
		if app.State != "identity_conflict" || conflict.Report.Available(app.TargetKey) {
			t.Fatal("ambiguous identity available")
		}
	}
	composed, err := discoverDocker([]dockerContainer{a}, nil, nil, dockerCache{})
	if err != nil {
		t.Fatal(err)
	}
	a.ID = strings.Repeat("d", 64)
	a.Names = []string{"/new-compose-name"}
	rebuilt, err := discoverDocker([]dockerContainer{a}, nil, nil, composed)
	if err != nil || rebuilt.Report.Applications[0].ID != composed.Report.Applications[0].ID {
		t.Fatal("compose rebuild lost identity", err)
	}
}

func TestDockerSocketReadOnlyAndMetadataMinimization(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.RequestURI() != "/containers/json?all=1" {
			t.Error("unexpected Docker API call")
		}
		w.Write([]byte(`[{"Id":"` + strings.Repeat("a", 64) + `","Names":["/app"],"State":"running","Labels":{"net.unraid.docker.webui":"http://[IP]:[PORT:80]","secret":"never-upload"},"Env":["PASSWORD=never-upload"],"Mounts":[{"Source":"never-upload"}],"HostConfig":{"NetworkMode":"host"}}]`))
	})}
	go srv.Serve(listener)
	defer srv.Close()
	containers, err := readDocker(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := discoverDocker(containers, nil, nil, dockerCache{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cache)
	if strings.Contains(string(raw), "never-upload") {
		t.Fatal("private metadata leaked")
	}
	cache.Report.ScannedAt = time.Now().Unix() - 91
	if cache.Report.Available(cache.Report.Applications[0].TargetKey) {
		t.Fatal("stale discovery accepted")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.xml"), []byte(`<Container><Name>app</Name><WebUI>http://[IP]/files/</WebUI></Container>`), 0600)
	templates, err := readTemplates(dir)
	if err != nil || templates["app"].WebUI != "http://[IP]/files/" {
		t.Fatal("template unreadable", err)
	}
}
