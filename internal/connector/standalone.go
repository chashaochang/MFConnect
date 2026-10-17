package connector

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mfconnect/internal/core"
)

// LocalDockerApplication has a server-derived address allowlist. A template's
// literal URL alone never authorizes a dial outside its container.
type LocalDockerApplication struct {
	Application core.DockerApplication
	Addresses   []string
}

func ScanLocalDocker(ctx context.Context, cachePath, socket, templateDir string, managementPorts map[int]bool) ([]LocalDockerApplication, error) {
	var old dockerCache
	_ = core.ReadJSON(cachePath, &old)
	containers, err := readDocker(ctx, socket)
	if err != nil {
		return nil, err
	}
	templates, err := readTemplates(templateDir)
	if err != nil {
		return nil, err
	}
	next, err := discoverDocker(containers, templates, nil, old)
	if err != nil {
		return nil, err
	}
	previous, _ := json.Marshal(old.Bindings)
	updated, _ := json.Marshal(next.Bindings)
	if string(previous) != string(updated) {
		// Persist only identity/target changes, not every scan to the flash drive.
		if err := core.WriteJSON(cachePath, next); err != nil {
			return nil, err
		}
	}
	hostIPs := map[string]bool{"127.0.0.1": true, "::1": true}
	if addresses, e := net.InterfaceAddrs(); e == nil {
		for _, a := range addresses {
			if ip, _, e := net.ParseCIDR(a.String()); e == nil {
				hostIPs[ip.String()] = true
			}
		}
	}
	byID := map[string]dockerContainer{}
	for _, c := range containers {
		byID[c.ID] = c
	}
	result := make([]LocalDockerApplication, len(next.Report.Applications))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, a := range next.Report.Applications {
		result[i].Application = a
		if a.State != "ready" {
			continue
		}
		wg.Add(1)
		go func(i int, a core.DockerApplication) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				result[i].Application.State = "unreachable"
				return
			}
			addresses := localAddresses(ctx, byID[a.ContainerID], containers, a.TargetURL, hostIPs, managementPorts)
			if len(addresses) == 0 {
				result[i].Application.State = "target_forbidden"
				return
			}
			for _, address := range addresses {
				conn, e := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", address)
				if e == nil {
					_ = conn.Close()
					result[i].Addresses = append(result[i].Addresses, address)
				}
			}
			if len(result[i].Addresses) == 0 {
				result[i].Application.State = "unreachable"
			}
		}(i, a)
	}
	wg.Wait()
	return result, nil
}

func localAddresses(ctx context.Context, c dockerContainer, all []dockerContainer, raw string, hostIPs map[string]bool, managementPorts map[int]bool) []string {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		port, e = strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return nil
		}
	}
	seen := map[string]bool{}
	for strings.HasPrefix(c.HostConfig.NetworkMode, "container:") {
		if seen[c.ID] {
			return nil
		}
		seen[c.ID] = true
		ref := strings.TrimPrefix(c.HostConfig.NetworkMode, "container:")
		matches := []dockerContainer{}
		for _, other := range all {
			if other.ID == ref || (len(ref) >= 12 && strings.HasPrefix(other.ID, ref)) || (len(other.Names) > 0 && strings.TrimPrefix(other.Names[0], "/") == ref) {
				matches = append(matches, other)
			}
		}
		if len(matches) != 1 {
			return nil
		}
		c = matches[0]
	}
	allowed := map[string]bool{}
	for _, n := range c.NetworkSettings.Networks {
		for _, ip := range []string{n.IPAddress, n.GlobalIPv6Address} {
			if p := net.ParseIP(ip); p != nil {
				allowed[p.String()] = true
			}
		}
	}
	for _, p := range c.Ports {
		if p.Type != "tcp" || p.PublicPort != port {
			continue
		}
		if p.IP == "" || p.IP == "0.0.0.0" || p.IP == "::" {
			for ip := range hostIPs {
				allowed[ip] = true
			}
		} else if ip := net.ParseIP(p.IP); ip != nil {
			allowed[ip.String()] = true
		}
	}
	if c.HostConfig.NetworkMode == "host" {
		for ip := range hostIPs {
			allowed[ip] = true
		}
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return nil
	}
	result := []string{}
	for _, a := range ips {
		ip := a.IP.String()
		// Every DNS answer must belong to this container. Pin numeric addresses
		// in the snapshot: never resolve client input or re-resolve during dial.
		if a.Zone != "" || !allowed[ip] || a.IP.IsUnspecified() || a.IP.IsMulticast() || (hostIPs[ip] && (managementPorts[port] || port == 22 || port == 2375 || port == 2376)) {
			return nil
		}
		result = append(result, net.JoinHostPort(ip, strconv.Itoa(port)))
	}
	sort.Strings(result)
	return result
}

func LocalManagementPorts(identPath string) map[int]bool {
	ports := map[int]bool{80: true, 443: true}
	b, _ := os.ReadFile(identPath)
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || (strings.TrimSpace(k) != "PORT" && strings.TrimSpace(k) != "PORTSSL") {
			continue
		}
		if p, e := strconv.Atoi(strings.Trim(strings.TrimSpace(v), "\"'")); e == nil && p > 0 && p <= 65535 {
			ports[p] = true
		}
	}
	return ports
}
