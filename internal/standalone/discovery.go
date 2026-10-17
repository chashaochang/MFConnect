package standalone

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"mfconnect/internal/connector"
)

type DockerCatalog struct {
	mu                                              sync.RWMutex
	state                                           Catalog
	CachePath, DockerSocket, TemplateDir, IdentPath string
}

func (c *DockerCatalog) Snapshot() Catalog { c.mu.RLock(); defer c.mu.RUnlock(); return c.state }
func (c *DockerCatalog) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	apps, err := connector.ScanLocalDocker(ctx, c.CachePath, c.DockerSocket, c.TemplateDir, connector.LocalManagementPorts(c.IdentPath))
	out := Catalog{State: "unavailable", Applications: []Application{}}
	if err == nil {
		out.State = "ready"
		out.ScannedAt = time.Now().Unix()
		for _, a := range apps {
			x := a.Application
			t := Application{ID: x.ID, ContainerID: x.ContainerID, Name: x.Name, State: x.State, DialAddresses: a.Addresses}
			if x.TargetURL != "" {
				u, e := url.Parse(x.TargetURL)
				if e == nil {
					port := 80
					if u.Scheme == "https" {
						port = 443
					}
					if u.Port() != "" {
						port, _ = strconv.Atoi(u.Port())
					}
					t.Target = &Target{Origin: x.TargetURL, Scheme: u.Scheme, Host: u.Hostname(), Port: port, Path: x.EntryPath, Wire: "tcp", TLS: "client_end_to_end"}
				}
			}
			t.Revision = Revision(x.ID, x.ContainerID, x.TargetURL, x.EntryPath, strings.Join(a.Addresses, ","))
			out.Applications = append(out.Applications, t)
		}
	}
	c.mu.Lock()
	c.state = out
	c.mu.Unlock()
	return err
}
func (c *DockerCatalog) Run(ctx context.Context) {
	_ = c.Refresh(ctx)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.Refresh(ctx)
		}
	}
}
