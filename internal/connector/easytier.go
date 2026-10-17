package connector

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/easytier"
)

func Run(ctx context.Context, configPath, runDir, frpc, easytierBinary string, interval time.Duration) error {
	c, err := Load(configPath)
	if err != nil {
		return err
	}
	if c.Transport == "frp" {
		return runFRP(ctx, configPath, runDir, frpc, interval)
	}
	return runEasyTier(ctx, configPath, runDir, easytierBinary, interval)
}

func runEasyTier(ctx context.Context, configPath, runDir, binary string, interval time.Duration) error {
	if err := easytier.CheckBinary(ctx, binary); err != nil {
		return err
	}
	if interval < time.Second {
		interval = 5 * time.Second
	}
	var proc *easytier.Process
	var gateway *deviceGateway
	var localPolicy, networkPolicy string
	var lastSync int64
	stop := func() {
		proc.Stop()
		proc = nil
		if gateway != nil {
			gateway.Close()
			gateway = nil
		}
		networkPolicy = ""
	}
	defer func() {
		stop()
		_ = core.WriteJSON(filepath.Join(runDir, "status.json"), Status{Version: core.Version, Transport: "easytier", State: "stopped", Updated: time.Now().Unix(), LastSync: lastSync})
	}()
	for {
		status := Status{Version: core.Version, Transport: "easytier", State: "configuration_required", Updated: time.Now().Unix(), LastSync: lastSync}
		c, err := Load(configPath)
		if err == nil && c.Transport != "easytier" {
			return errors.New("transport changed; restart connector")
		}
		if err != nil {
			stop()
		} else {
			encoded, _ := json.Marshal(c)
			if hash := core.Hash(string(encoded)); hash != localPolicy {
				stop()
				localPolicy = hash
			}
			status.DeviceID = c.DeviceID
			out, syncErr := Sync(ctx, c)
			if syncErr != nil {
				status.State = "controller_unreachable"
				var he HTTPError
				if errors.As(syncErr, &he) && he.Status == 401 {
					stop()
					status.State = "authorization_required"
				}
			} else {
				lastSync = time.Now().Unix()
				status.LastSync = lastSync
				status.Services = out.Assignments
				status.State = "supervising_tunnel"
				if out.Transport != "easytier" || out.EasyTier == nil {
					err = errors.New("missing EasyTier assignment")
				} else {
					encoded, err = (easytier.Node{Name: "nas", Network: out.EasyTier.Network, Secret: out.EasyTier.Secret, IP: easytier.NASIP, Peers: []string{out.EasyTier.Peer}, GatewayPort: c.GatewayPort}).Config()
				}
				if err == nil && (!proc.Alive() || networkPolicy != core.Hash(string(encoded))) {
					stop()
					gateway, err = startGateway(configPath, c)
					if err == nil {
						path := filepath.Join(runDir, "easytier.toml")
						err = core.WritePrivate(path, encoded)
						if err == nil {
							proc, err = easytier.Start(ctx, binary, path, 0)
						}
						if err == nil {
							networkPolicy = core.Hash(string(encoded))
						}
					}
				}
				if err != nil {
					stop()
					status.State = "tunnel_start_failed"
				}
			}
			if proc.Alive() {
				status.ProcessID = proc.PID()
				status.GatewayPort = c.GatewayPort
			}
		}
		if err := core.WriteJSON(filepath.Join(runDir, "status.json"), status); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
