package server

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mfconnect/internal/core"
)

type FRPConfig struct {
	Binary     string `json:"binary"`
	RunDir     string `json:"run_dir"`
	ListenHost string `json:"listen_host"`
}

// FRPServerConfig binds every service forward to loopback. Only the shared
// authenticated client connection is published outside the server/container.
func FRPServerConfig(c Config) map[string]any {
	bind := "0.0.0.0"
	if c.FRP != nil {
		bind = c.FRP.ListenHost
	}
	return map[string]any{
		"bindAddr": bind, "bindPort": c.TunnelPort, "proxyBindAddr": c.ProxyHost,
		"allowPorts":  []map[string]int{{"start": c.PortStart, "end": c.PortEnd}},
		"transport":   map[string]any{"tls": map[string]any{"force": true, "certFile": c.Certificate, "keyFile": c.PrivateKey}},
		"httpPlugins": []map[string]any{{"name": "mfconnect", "addr": c.HookListen, "path": "/frp", "ops": []string{"Login", "NewProxy", "Ping", "NewWorkConn", "NewUserConn"}}},
		"log":         map[string]any{"to": "console", "level": "warn", "disablePrintColor": true},
	}
}

// StartFRP supervises a single frps for all NAS devices. Device identity and
// service grants are checked by the private hook and again by the NAS gateway.
func (s *Server) StartFRP(ctx context.Context) (func(), error) {
	c := s.Config.FRP
	if c == nil {
		return func() {}, nil
	}
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	version, err := exec.CommandContext(check, c.Binary, "--version").Output()
	cancel()
	if err != nil || strings.TrimSpace(string(version)) != core.FRPVersion {
		return nil, errors.New("frps version does not match pinned release")
	}
	// Detect an occupied port before starting supervision instead of silently
	// claiming a working relay while another service owns its listener.
	listener, err := net.Listen("tcp", net.JoinHostPort(c.ListenHost, strconv.Itoa(s.Config.TunnelPort)))
	if err != nil {
		return nil, errors.New("FRP listener is already in use or unavailable")
	}
	_ = listener.Close()
	path := filepath.Join(c.RunDir, "frps.json")
	if err = core.WriteJSON(path, FRPServerConfig(s.Config)); err != nil {
		return nil, err
	}
	check, cancel = context.WithTimeout(ctx, 10*time.Second)
	err = exec.CommandContext(check, c.Binary, "verify", "-c", path).Run()
	cancel()
	if err != nil {
		return nil, errors.New("frps rejected generated configuration")
	}
	ctx, cancel = context.WithCancel(ctx)
	done := make(chan struct{})
	statusPath := filepath.Join(c.RunDir, "frps-status.json")
	go func() {
		defer close(done)
		defer func() {
			_ = core.WriteJSON(statusPath, map[string]any{"state": "stopped", "updated_at": time.Now().Unix()})
		}()
		for ctx.Err() == nil {
			cmd := exec.Command(c.Binary, "-c", path)
			cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
			if err := cmd.Start(); err == nil {
				_ = core.WriteJSON(statusPath, map[string]any{"state": "running", "process_id": cmd.Process.Pid, "updated_at": time.Now().Unix()})
				exited := make(chan error, 1)
				go func() { exited <- cmd.Wait() }()
				select {
				case <-exited:
				case <-ctx.Done():
					_ = cmd.Process.Signal(syscall.SIGTERM)
					select {
					case <-exited:
					case <-time.After(5 * time.Second):
						_ = cmd.Process.Kill()
						<-exited
					}
				}
			}
			if ctx.Err() != nil {
				return
			}
			_ = core.WriteJSON(statusPath, map[string]any{"state": "restarting", "updated_at": time.Now().Unix()})
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}
