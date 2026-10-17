package connector

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mfconnect/internal/webproxy"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mfconnect/internal/core"
)

type dockerPort struct {
	IP          string
	PrivatePort int
	PublicPort  int
	Type        string
}
type dockerNetwork struct {
	IPAddress         string
	GlobalIPv6Address string
}
type dockerContainer struct {
	ID              string `json:"Id"`
	Names           []string
	Labels          map[string]string
	State           string
	Ports           []dockerPort
	HostConfig      struct{ NetworkMode string }
	NetworkSettings struct{ Networks map[string]dockerNetwork }
}
type dockerTemplate struct {
	Name  string
	WebUI string
	File  string `xml:"-"`
}
type dockerBinding struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Container  string `json:"container"`
	Key        string `json:"key"`
	URL        string `json:"url,omitempty"`
	Path       string `json:"path,omitempty"`
	AdoptedURL string `json:"adopted_url,omitempty"`
	Conflict   bool   `json:"conflict,omitempty"`
}
type dockerCache struct {
	Bindings []dockerBinding   `json:"bindings"`
	Report   core.DockerReport `json:"report"`
	Targets  []core.Target     `json:"targets"`
}

func discoveryEnabled(c Config) bool {
	if c.DockerDiscovery != nil {
		return *c.DockerDiscovery
	}
	_, e := os.Stat("/etc/unraid-version")
	return e == nil
}
func discoveryPath(path string) string {
	return filepath.Join(filepath.Dir(path), "docker-discovery.json")
}

// InspectDocker runs the same read-only scan without changing configuration,
// identity bindings, publication state or cloud registrations.
func InspectDocker(ctx context.Context, path string) (core.DockerReport, error) {
	var c Config
	var old dockerCache
	if err := core.ReadJSON(path, &c); err != nil {
		return core.DockerReport{}, err
	}
	_ = core.ReadJSON(discoveryPath(path), &old)
	containers, err := readDocker(ctx, "/var/run/docker.sock")
	if err != nil {
		return core.DockerReport{}, err
	}
	templates, err := readTemplates("/boot/config/plugins/dockerMan/templates-user")
	if err != nil {
		return core.DockerReport{}, err
	}
	next, err := discoverDocker(containers, templates, c.Targets, old)
	return next.Report, err
}

// The daemon refreshes this private sidecar; normal config writes remain owned
// by the settings editor. Network requests never execute Docker mutations.
func refreshDocker(ctx context.Context, path string) error {
	var c Config
	if e := core.ReadJSON(path, &c); e != nil {
		return e
	}
	if !discoveryEnabled(c) {
		return nil
	}
	var cache dockerCache
	_ = core.ReadJSON(discoveryPath(path), &cache)
	report := core.DockerReport{Schema: 1, State: "unavailable", ScannedAt: cache.Report.ScannedAt, Applications: cache.Report.Applications}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	containers, e := readDocker(ctx, "/var/run/docker.sock")
	if e == nil {
		templates, e2 := readTemplates("/boot/config/plugins/dockerMan/templates-user")
		if e2 == nil {
			cache, e = discoverDocker(containers, templates, c.Targets, cache)
			if e == nil {
				probeDockerTargets(ctx, &cache)
			}
		} else {
			e = e2
		}
	}
	if e != nil {
		cache.Report = report
	}
	if cache.Report.Applications == nil {
		cache.Report.Applications = []core.DockerApplication{}
	}
	if writeErr := core.WriteJSON(discoveryPath(path), cache); writeErr != nil {
		return writeErr
	}
	return e
}

func loadDocker(c *Config) error {
	if !discoveryEnabled(*c) {
		c.docker = &core.DockerReport{Schema: 1, State: "disabled", Applications: []core.DockerApplication{}}
		return nil
	}
	var cache dockerCache
	if err := core.ReadJSON(discoveryPath(c.path), &cache); err != nil {
		c.docker = &core.DockerReport{Schema: 1, State: "unavailable", Applications: []core.DockerApplication{}}
		return nil
	}
	if cache.Report.Validate() != nil {
		return errors.New("invalid docker discovery cache")
	}
	// A restarted daemon must refresh before trusting old IP allocations.
	if time.Now().Unix()-cache.Report.ScannedAt > 90 {
		cache.Report.State = "unavailable"
	}
	c.docker = &cache.Report
	keys := map[string]core.Target{}
	for _, t := range c.Targets {
		keys[t.Key] = t
	}
	for _, t := range cache.Targets {
		if t.Validate() != nil {
			return errors.New("invalid docker target cache")
		}
		if old, ok := keys[t.Key]; ok {
			// Manual target edits are authoritative; a stale discovery binding may
			// never replace an explicit user-configured destination.
			adopted := false
			for _, b := range cache.Bindings {
				if b.Key == t.Key && b.AdoptedURL != "" && b.AdoptedURL == old.URL {
					adopted = true
					break
				}
			}
			if adopted {
				for i := range c.Targets {
					if c.Targets[i].Key == t.Key {
						c.Targets[i] = t
						break
					}
				}
			} else if old.URL != t.URL {
				for i := range c.docker.Applications {
					if c.docker.Applications[i].TargetKey == t.Key {
						c.docker.Applications[i].State = "identity_conflict"
					}
				}
			}
			continue
		}
		c.Targets = append(c.Targets, t)
		keys[t.Key] = t
	}
	return nil
}

func readDocker(ctx context.Context, socket string) ([]dockerContainer, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://docker/containers/json?all=1", nil)
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("docker unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil || len(raw) > 4*1024*1024 {
		return nil, errors.New("docker inventory too large")
	}
	var out []dockerContainer
	if json.Unmarshal(raw, &out) != nil || len(out) > 256 {
		return nil, errors.New("invalid docker inventory")
	}
	return out, nil
}

func readTemplates(dir string) (map[string]dockerTemplate, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.xml"))
	if err != nil {
		return nil, err
	}
	if len(files) > 1024 {
		return nil, errors.New("too many docker templates")
	}
	templates := map[string]dockerTemplate{}
	for _, file := range files {
		f, e := os.Open(file)
		if e != nil {
			return nil, e
		}
		b, e := io.ReadAll(io.LimitReader(f, 262145))
		f.Close()
		if e != nil || len(b) > 262144 {
			return nil, errors.New("invalid docker template")
		}
		var t dockerTemplate
		if xml.Unmarshal(b, &t) != nil || t.Name == "" {
			continue
		}
		t.File = filepath.Base(file)
		if _, exists := templates[t.Name]; exists {
			templates[t.Name] = dockerTemplate{Name: t.Name}
			continue
		}
		templates[t.Name] = t
	}
	return templates, nil
}

func dockerSource(c dockerContainer, t dockerTemplate) (string, string) {
	l := c.Labels
	if l["com.docker.compose.project"] != "" && l["com.docker.compose.service"] != "" {
		number := l["com.docker.compose.container-number"]
		if number == "" {
			number = "1"
		}
		return "compose", strings.Join([]string{"compose", l["com.docker.compose.project"], l["com.docker.compose.service"], number}, "\x00")
	}
	if t.File != "" {
		return "unraid_template", "template\x00" + t.File
	}
	return "container_name", "name\x00" + strings.TrimPrefix(c.Names[0], "/")
}

func discoverDocker(containers []dockerContainer, templates map[string]dockerTemplate, manual []core.Target, old dockerCache) (dockerCache, error) {
	next := dockerCache{Bindings: append([]dockerBinding(nil), old.Bindings...), Report: core.DockerReport{Schema: 1, State: "ready", ScannedAt: time.Now().Unix(), Applications: []core.DockerApplication{}}, Targets: []core.Target{}}
	if len(old.Bindings) > 4096 {
		return old, errors.New("docker identity registry full")
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })
	sources := map[string]int{}
	for _, c := range containers {
		if !core.ContainerID.MatchString(c.ID) || len(c.Names) == 0 {
			return old, errors.New("invalid docker identity")
		}
		_, source := dockerSource(c, templates[strings.TrimPrefix(c.Names[0], "/")])
		sources[source]++
	}
	usedKeys := map[string]bool{}
	currentIDs := map[string]bool{}
	for _, c := range containers {
		currentIDs[c.ID] = true
	}
	for _, c := range containers {
		name := strings.TrimPrefix(c.Names[0], "/")
		t := templates[name]
		kind, source := dockerSource(c, t)
		binding := -1
		for i, b := range next.Bindings {
			if b.Container == c.ID {
				binding = i
				break
			}
		}
		ambiguous := false
		if binding < 0 && sources[source] == 1 {
			candidates := []int{}
			for i, b := range next.Bindings {
				if b.Source == source && !currentIDs[b.Container] {
					candidates = append(candidates, i)
				}
			}
			if len(candidates) == 1 {
				binding = candidates[0]
			} else if len(candidates) > 1 {
				ambiguous = true
			}
		}
		if binding < 0 {
			if len(next.Bindings) >= 4096 {
				return old, errors.New("docker identity registry full")
			}
			next.Bindings = append(next.Bindings, dockerBinding{ID: core.ID("app"), Source: source, Container: c.ID, Key: core.ID("docker"), Conflict: ambiguous})
			binding = len(next.Bindings) - 1
		}
		b := &next.Bindings[binding]
		b.Container = c.ID
		b.Source = source
		webui := c.Labels["net.unraid.docker.webui"]
		if webui == "" {
			webui = t.WebUI
		}
		origin, path, state := resolveDockerWebUI(c, containers, webui)
		running := c.State == "running"
		if !running && origin == "" && b.URL != "" {
			origin, path = b.URL, b.Path
		}
		if sources[source] > 1 || b.Conflict {
			state = "identity_conflict"
		} else if !running && origin != "" {
			state = "stopped"
		}
		// Adopt one existing exact destination/name match to preserve the user's
		// explicitly configured Jellyfin service and its established domain.
		if b.URL == "" && origin != "" {
			matches := []core.Target{}
			for _, m := range manual {
				if m.URL == origin && strings.EqualFold(m.Name, name) && !usedKeys[m.Key] {
					matches = append(matches, m)
				}
			}
			if len(matches) == 1 {
				b.Key = matches[0].Key
				b.AdoptedURL = matches[0].URL
			}
		}
		if usedKeys[b.Key] {
			return old, errors.New("ambiguous docker target binding")
		}
		usedKeys[b.Key] = true
		a := core.DockerApplication{ID: b.ID, ContainerID: c.ID, Name: name, IdentityKind: kind, State: state, Running: running, TargetKey: b.Key, TargetURL: origin, EntryPath: path}
		if origin != "" {
			target := core.Target{Key: b.Key, Name: name, URL: origin}
			for _, m := range manual {
				if m.Key == b.Key {
					if b.AdoptedURL != "" && m.URL == b.AdoptedURL {
						target.CertificateSHA256 = m.CertificateSHA256
						target.Name = m.Name
					} else {
						if m.URL != origin {
							a.State = "identity_conflict"
						}
						target = m
					}
					break
				}
			}
			next.Targets = append(next.Targets, target)
			b.URL = origin
			b.Path = path
		}
		next.Report.Applications = append(next.Report.Applications, a)
	}
	if e := next.Report.Validate(); e != nil {
		return old, e
	}
	return next, nil
}

var portPlaceholder = regexp.MustCompile(`\[PORT:([0-9]{1,5})\]`)

func resolveDockerWebUI(c dockerContainer, all []dockerContainer, raw string) (string, string, string) {
	if raw == "" {
		return "", "/", "no_webui"
	}
	if len(raw) > 4096 {
		return "", "/", "invalid_webui"
	}
	owner := c
	seen := map[string]bool{}
	for strings.HasPrefix(owner.HostConfig.NetworkMode, "container:") {
		if seen[owner.ID] {
			return "", "/", "network_unresolved"
		}
		seen[owner.ID] = true
		ref := strings.TrimPrefix(owner.HostConfig.NetworkMode, "container:")
		matches := []dockerContainer{}
		for _, peer := range all {
			if peer.ID == ref || (len(ref) >= 12 && strings.HasPrefix(peer.ID, ref)) || (len(peer.Names) > 0 && strings.TrimPrefix(peer.Names[0], "/") == ref) {
				matches = append(matches, peer)
			}
		}
		if len(matches) != 1 {
			return "", "/", "network_unresolved"
		}
		owner = matches[0]
	}
	host := ""
	replacement := ""
	ports := portPlaceholder.FindAllStringSubmatch(raw, -1)
	if len(ports) > 1 {
		return "", "/", "invalid_webui"
	}
	if len(ports) == 1 {
		p, _ := strconv.Atoi(ports[0][1])
		if p < 1 || p > 65535 {
			return "", "/", "invalid_webui"
		}
		replacement = strconv.Itoa(p)
		bindings := map[string]dockerPort{}
		for _, b := range owner.Ports {
			if b.Type == "tcp" && b.PrivatePort == p && b.PublicPort > 0 {
				if b.IP == "" || b.IP == "0.0.0.0" {
					b.IP = "127.0.0.1"
				}
				if b.IP == "::" {
					b.IP = "::1"
				}
				bindings[net.JoinHostPort(b.IP, strconv.Itoa(b.PublicPort))] = b
			}
		}
		if len(bindings) > 0 {
			keys := make([]string, 0, len(bindings))
			for k := range bindings {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			chosen := bindings[keys[0]]
			publicPorts := map[int]bool{}
			for _, b := range bindings {
				publicPorts[b.PublicPort] = true
			}
			if len(publicPorts) > 1 {
				return "", "/", "network_unresolved"
			}
			loopback := false
			for _, b := range bindings {
				if b.IP == "127.0.0.1" {
					chosen = b
					loopback = true
					break
				}
			}
			if !loopback && len(bindings) > 1 {
				return "", "/", "network_unresolved"
			}
			host = chosen.IP
			replacement = strconv.Itoa(chosen.PublicPort)
		}
	}
	if strings.Contains(raw, "[IP]") && host == "" {
		if owner.HostConfig.NetworkMode == "host" {
			host = "127.0.0.1"
		} else {
			addresses := map[string]bool{}
			for name, n := range owner.NetworkSettings.Networks {
				ip := n.IPAddress
				if ip == "" {
					ip = n.GlobalIPv6Address
				}
				if net.ParseIP(ip) == nil {
					continue
				}
				if name == owner.HostConfig.NetworkMode {
					host = ip
					break
				}
				addresses[ip] = true
			}
			if host == "" && len(addresses) == 1 {
				for ip := range addresses {
					host = ip
				}
			}
		}
		if host == "" {
			return "", "/", "network_unresolved"
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	raw = strings.ReplaceAll(raw, "[IP]", host)
	if replacement != "" {
		raw = portPlaceholder.ReplaceAllString(raw, replacement)
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(u.Host, "[]") && net.ParseIP(u.Hostname()) == nil {
		return "", "/", "invalid_webui"
	}
	origin := u.Scheme + "://" + u.Host
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	if (core.Target{Key: "docker", URL: origin}).Validate() != nil || !core.SafePath(path) || len(path) > 2048 {
		return "", "/", "invalid_webui"
	}
	return origin, path, "ready"
}

// A declaration alone is insufficient for automatic remote access. Probe the
// resolved HTTP(S) origin from this NAS; redirects are not followed and a 401,
// 403 or 405 still proves an HTTP service exists. TLS uses normal verification
// or an explicitly configured certificate pin.
func probeDockerTargets(ctx context.Context, cache *dockerCache) {
	targets := map[string]core.Target{}
	for _, t := range cache.Targets {
		targets[t.Key] = t
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, 8)
	for i := range cache.Report.Applications {
		a := &cache.Report.Applications[i]
		if a.State != "ready" {
			continue
		}
		wg.Add(1)
		go func(a *core.DockerApplication) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				a.State = "unreachable"
				return
			}
			target := targets[a.TargetKey]
			u, e := url.Parse(target.URL)
			if e != nil {
				a.State = "unreachable"
				return
			}
			transport := webproxy.Transport()
			defer transport.CloseIdleConnections()
			transport.TLSClientConfig = webproxy.TLS(u.Hostname(), target.CertificateSHA256)
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			request, e := http.NewRequestWithContext(probe, "HEAD", target.URL+a.EntryPath, nil)
			if e != nil {
				a.State = "unreachable"
				return
			}
			client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			response, e := client.Do(request)
			if e != nil {
				a.State = "unreachable"
				return
			}
			response.Body.Close()
			if response.StatusCode >= 500 {
				a.State = "unreachable"
			}
		}(a)
	}
	wg.Wait()
}
