package connector

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/updates"
)

type Config struct {
	EntryOrigin     string `json:"entry_origin,omitempty"`
	APIBaseURL      string `json:"api_base_url,omitempty"`
	DockerDiscovery *bool  `json:"docker_discovery,omitempty"`
	docker          *core.DockerReport
	WebGUIURL       string `json:"webgui_url,omitempty"`
	path            string
	Transport       string        `json:"transport,omitempty"`
	GatewayPort     int           `json:"gateway_port,omitempty"`
	Server          string        `json:"server"`
	Name            string        `json:"name"`
	CAFile          string        `json:"ca_file"`
	DeviceID        string        `json:"device_id,omitempty"`
	PrivateKey      string        `json:"private_key"`
	TunnelToken     string        `json:"tunnel_token,omitempty"`
	Targets         []core.Target `json:"targets"`
}

func Init(path, server, name, ca string) error {
	if err := validateServer(server); err != nil {
		return err
	}
	return initConfig(path, Config{Server: strings.TrimRight(server, "/"), Name: name, CAFile: ca})
}
func initConfig(path string, c Config) error {
	if _, err := os.Stat(path); err == nil {
		return errors.New("configuration already exists")
	}
	if err := validateEndpoints(c); err != nil {
		return err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	c.Transport, c.GatewayPort, c.PrivateKey, c.Targets = "frp", 18743, base64.RawStdEncoding.EncodeToString(key), []core.Target{}
	return core.WriteJSON(path, c)
}
func validateServer(server string) error {
	u, e := url.Parse(server)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("server must be an HTTPS origin")
	}
	return nil
}
func Load(path string) (Config, error) {
	var c Config
	if err := core.ReadJSON(path, &c); err != nil {
		return c, err
	}
	c.path = path
	if c.WebGUIURL == "" {
		c.WebGUIURL = defaultWebGUIURL()
	}
	if c.WebGUIURL != "" && !validWebGUIURL(c.WebGUIURL) {
		return c, errors.New("WebGUI must be a loopback HTTP(S) origin")
	}
	if c.Transport == "" {
		c.Transport = "frp"
	}
	if c.Transport != "frp" && c.Transport != "easytier" {
		return c, errors.New("unsupported transport")
	}
	if (c.Transport == "easytier" || c.GatewayPort != 0) && (c.GatewayPort < 1024 || c.GatewayPort > 65535) {
		return c, errors.New("invalid gateway port")
	}
	if err := validateEndpoints(c); err != nil {
		return c, err
	}
	c.Server = strings.TrimRight(c.Server, "/")
	key, err := base64.RawStdEncoding.DecodeString(c.PrivateKey)
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return c, errors.New("invalid device key")
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if err = t.Validate(); err != nil {
			return c, err
		}
		if seen[t.Key] {
			return c, errors.New("duplicate target key")
		}
		seen[t.Key] = true
	}
	if c.CAFile == "" {
		return c, errors.New("CA file required for verified controller and frp TLS")
	}
	if err := loadDocker(&c); err != nil {
		return c, err
	}
	return c, nil
}

func client(c Config) (*http.Client, error) {
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid CA PEM")
	}
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}, nil
}

type HTTPError struct {
	Status int
	Code   string
}

func (e HTTPError) Error() string { return fmt.Sprintf("controller returned HTTP %d", e.Status) }

func request(ctx context.Context, c Config, path string, input, output any, sign bool) error {
	return requestContract(ctx, c, path, input, output, sign, 1)
}
func requestContract(ctx context.Context, c Config, path string, input, output any, sign bool, contract int) error {
	if err := validateEndpoints(c); err != nil {
		return err
	}

	b, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.APIBase()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if contract == 2 {
		req.Header.Set("X-MF-Endpoint-Contract", "2")
	}
	if sign {
		key, _ := base64.RawStdEncoding.DecodeString(c.PrivateKey)
		core.Sign(req, c.DeviceID, key, b)
	}
	httpClient, err := client(c)
	if err != nil {
		return err
	}
	defer httpClient.CloseIdleConnections()
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&failure)
		return HTTPError{Status: resp.StatusCode, Code: failure.Error}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output)
}

func Enroll(ctx context.Context, path, token string) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	if c.DeviceID != "" {
		return errors.New("device is already enrolled; revoke the old binding before creating a new configuration")
	}
	key, _ := base64.RawStdEncoding.DecodeString(c.PrivateKey)
	pub := ed25519.PrivateKey(key).Public().(ed25519.PublicKey)
	proof := ed25519.Sign(key, []byte("mfconnect-enroll\n"+token+"\n"+c.Name))
	input := map[string]string{"enrollment_token": token, "name": c.Name, "public_key": base64.RawStdEncoding.EncodeToString(pub), "proof": base64.RawStdEncoding.EncodeToString(proof)}
	var out struct {
		ID    string `json:"device_id"`
		Token string `json:"tunnel_token"`
	}
	if err = request(ctx, c, "/v1/devices/enroll", input, &out, false); err != nil {
		return err
	}
	if !core.Identifier.MatchString(out.ID) || out.Token == "" {
		return errors.New("invalid enrollment response")
	}
	c.DeviceID = out.ID
	c.TunnelToken = out.Token
	// Persist identity only; discovered targets belong to the private sidecar.
	var saved Config
	if err = core.ReadJSON(path, &saved); err != nil {
		return err
	}
	saved.DeviceID, saved.TunnelToken = c.DeviceID, c.TunnelToken
	return core.WriteJSON(path, saved)
}

func Sync(ctx context.Context, c Config) (core.SyncResponse, error) {
	var out core.SyncResponse
	if c.DeviceID == "" || c.TunnelToken == "" {
		return out, errors.New("device has not been enrolled")
	}
	in := core.SyncRequest{Targets: c.Targets, Docker: c.docker, Transport: c.Transport, PortalSupported: c.WebGUIURL != "" && c.GatewayPort > 0}
	if c.path != "" {
		in.Software, in.UpdateReport = updates.PluginSoftware(c.path)
		if in.Software != nil {
			in.Software.Connector = core.Version
		}
	}
	// Address collection must not interrupt the existing authenticated tunnel.
	// An omitted report preserves the last successful snapshot and its timestamp.
	if report, err := networkReport(); err == nil {
		in.Network = report
	}
	if c.Transport == "easytier" || c.GatewayPort != 0 {
		_, cert, err := gatewayCertificate(c)
		if err != nil {
			return out, err
		}
		in.GatewayPort, in.GatewayCertificate = c.GatewayPort, cert
	}
	err := request(ctx, c, "/v1/device/sync", in, &out, true)
	if err == nil && c.path != "" && out.UpdateJob != nil && out.UpdateJob.DeviceID == c.DeviceID {
		_ = updates.AcceptPluginJob(c.path, out.UpdateJob)
	}
	return out, err
}

func FRPConfig(c Config, out core.SyncResponse) ([]byte, error) {
	if out.Transport != "" && out.Transport != "frp" {
		return nil, errors.New("controller returned another transport")
	}
	if (c.GatewayPort > 0) != out.Gateway {
		return nil, errors.New("controller and connector disagree about NAS gateway; refusing downgrade")
	}
	if out.TunnelHost == "" || out.TunnelPort < 1 || out.TunnelPort > 65535 {
		return nil, errors.New("invalid tunnel endpoint")
	}
	targets := map[string]core.Target{}
	for _, t := range c.Targets {
		targets[t.Key] = t
	}
	assignments := append([]core.Assignment(nil), out.Assignments...)
	sort.Slice(assignments, func(i, j int) bool { return assignments[i].ServiceID < assignments[j].ServiceID })
	proxies := []map[string]any{}
	if p := out.Portal; p != nil && p.Enabled {
		if c.WebGUIURL == "" || c.GatewayPort == 0 || !core.ValidConnectID(p.ID) || p.RemotePort < 1024 || p.RemotePort > 65535 {
			return nil, errors.New("invalid portal assignment")
		}
		proxies = append(proxies, map[string]any{"name": "portal", "type": "tcp", "localIP": "127.0.0.1", "localPort": c.GatewayPort, "remotePort": p.RemotePort})
	}
	seen := map[string]bool{}
	for _, a := range assignments {
		t, ok := targets[a.Key]
		if !ok || !core.Identifier.MatchString(a.ServiceID) || a.RemotePort < 1024 || a.RemotePort > 65535 || seen[a.ServiceID] {
			return nil, errors.New("controller assigned an unconfigured or invalid target")
		}
		seen[a.ServiceID] = true
		if !a.Published {
			continue
		}
		u, err := url.Parse(t.URL)
		if err != nil {
			return nil, err
		}
		port := 80
		if u.Scheme == "https" {
			port = 443
		}
		if u.Port() != "" {
			port, _ = strconv.Atoi(u.Port())
		}
		localIP := u.Hostname()
		if c.GatewayPort > 0 {
			localIP, port = "127.0.0.1", c.GatewayPort
		}
		proxies = append(proxies, map[string]any{"name": a.ServiceID, "type": "tcp", "localIP": localIP, "localPort": port, "remotePort": a.RemotePort})
	}
	return json.MarshalIndent(map[string]any{
		"serverAddr": out.TunnelHost, "serverPort": out.TunnelPort, "user": c.DeviceID, "metadatas": map[string]string{"mf_token": c.TunnelToken}, "loginFailExit": false,
		"transport": map[string]any{"tls": map[string]any{"enable": true, "trustedCaFile": c.CAFile, "serverName": out.TunnelHost}},
		"log":       map[string]any{"to": "console", "level": "warn", "disablePrintColor": true}, "proxies": proxies,
	}, "", "  ")
}

type Status struct {
	Connect     *core.PortalAssignment `json:"connect,omitempty"`
	Transport   string                 `json:"transport,omitempty"`
	GatewayPort int                    `json:"gateway_port,omitempty"`
	Version     string                 `json:"version"`
	State       string                 `json:"state"`
	DeviceID    string                 `json:"device_id,omitempty"`
	Updated     int64                  `json:"updated"`
	LastSync    int64                  `json:"last_sync,omitempty"`
	ProcessID   int                    `json:"process_id,omitempty"`
	Services    []core.Assignment      `json:"services,omitempty"`
}
type child struct {
	cmd  *exec.Cmd
	done chan error
}

func stop(c *child) {
	if c == nil {
		return
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

// Run retains an established tunnel during control-plane outages. Revocation
// stops it; frps also checks revocation independently on new work/connections.
func runFRP(ctx context.Context, configPath, runDir, frpc string, interval time.Duration) error {
	if interval < time.Second {
		interval = 5 * time.Second
	}
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	var proc *child
	var gateway *deviceGateway
	var lastConfig string
	var localPolicy string
	lastSync := int64(0)
	lastDiscovery := time.Time{}
	stopAll := func() {
		stop(proc)
		proc = nil
		gateway.Close()
		gateway = nil
		lastConfig = ""
	}
	defer func() {
		stopAll()
		_ = core.WriteJSON(filepath.Join(runDir, "status.json"), Status{Version: core.Version, State: "stopped", Updated: time.Now().Unix(), LastSync: lastSync})
	}()
	for {
		if time.Since(lastDiscovery) >= 30*time.Second {
			_ = refreshDocker(ctx, configPath)
			lastDiscovery = time.Now()
		}
		status := Status{Version: core.Version, Transport: "frp", State: "configuration_required", Updated: time.Now().Unix(), LastSync: lastSync}
		c, err := Load(configPath)
		if err == nil && c.Transport != "frp" {
			return errors.New("transport changed; restart connector")
		}
		if err != nil {
			stopAll()
		}
		if err == nil {
			policy, _ := json.Marshal(c)
			if localPolicy != "" && localPolicy != core.Hash(string(policy)) {
				stopAll()
			}
			localPolicy = core.Hash(string(policy))
			status.DeviceID = c.DeviceID
			var out core.SyncResponse
			out, err = Sync(ctx, c)
			if err == nil {
				lastSync = time.Now().Unix()
				status.LastSync = lastSync
				status.Services = out.Assignments
				status.Connect = out.Portal
				status.State = "supervising_tunnel"
				var encoded []byte
				encoded, err = FRPConfig(c, out)
				if proc != nil {
					select {
					case <-proc.done:
						proc = nil
						lastConfig = ""
					default:
					}
				}
				if err == nil && (proc == nil || lastConfig != core.Hash(string(encoded))) {
					candidate := filepath.Join(runDir, "frpc-next.json")
					err = core.WritePrivate(candidate, encoded)
					if err == nil {
						verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
						verify := exec.CommandContext(verifyCtx, frpc, "verify", "-c", candidate)
						err = verify.Run()
						cancel()
					}
					if err == nil {
						stopAll()
						if c.GatewayPort > 0 {
							gateway, err = startGateway(configPath, c)
						}
					}
					if err == nil {
						active := filepath.Join(runDir, "frpc.json")
						err = os.Rename(candidate, active)
						if err == nil {
							cmd := exec.Command(frpc, "-c", active)
							// Do not expose transport metadata or credentials through logs.
							cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
							err = cmd.Start()
							if err == nil {
								proc = &child{cmd: cmd, done: make(chan error, 1)}
								done := proc.done
								go func() { done <- cmd.Wait() }()
								lastConfig = core.Hash(string(encoded))
							}
						}
					}
				}
			} else {
				status.State = "controller_unreachable"
				var he HTTPError
				if errors.As(err, &he) && he.Status == 401 {
					stopAll()
					status.State = "authorization_required"
				}
			}
			if proc != nil {
				status.ProcessID = proc.cmd.Process.Pid
				status.GatewayPort = c.GatewayPort
			}
			if err != nil && status.State == "supervising_tunnel" {
				stopAll()
				status.ProcessID, status.GatewayPort = 0, 0
				status.State = "tunnel_start_failed"
			}
		}
		if err = core.WriteJSON(filepath.Join(runDir, "status.json"), status); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
