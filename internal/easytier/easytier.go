// Package easytier owns the pinned upstream process, never the application's
// authorization policy. Every device has a separate private network in this pilot.
package easytier

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mfconnect/internal/core"
)

const NASIP = "10.144.0.2"

type Node struct {
	Name, Network, Secret, IP string
	Listeners, Peers          []string
	GatewayPort               int
	ForwardPort               int
}

func (n Node) Config() ([]byte, error) {
	if !core.Identifier.MatchString(n.Name) || !core.Identifier.MatchString(n.Network) || len(n.Secret) < 32 || len(n.Secret) > 128 || net.ParseIP(n.IP) == nil {
		return nil, errors.New("invalid EasyTier network identity")
	}
	if n.GatewayPort < 1024 || n.GatewayPort > 65535 || n.ForwardPort < 0 || n.ForwardPort > 65535 {
		return nil, errors.New("invalid EasyTier gateway port")
	}
	q := strconv.Quote
	var b strings.Builder
	fmt.Fprintf(&b, "instance_name = %s\nhostname = %s\nipv4 = %s\nlisteners = [", q(n.Name), q(n.Name), q(n.IP))
	for i, listener := range n.Listeners {
		if err := endpoint(listener); err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(q(listener))
	}
	b.WriteString("]\nstun_servers = []\nstun_servers_v6 = []\n")
	for _, peer := range n.Peers {
		if err := endpoint(peer); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "[[peer]]\nuri = %s\n", q(peer))
	}
	if n.ForwardPort > 0 {
		fmt.Fprintf(&b, "[[port_forward]]\nbind_addr = %s\ndst_addr = %s\nproto = \"tcp\"\n", q(net.JoinHostPort("127.0.0.1", strconv.Itoa(n.ForwardPort))), q(net.JoinHostPort(NASIP, strconv.Itoa(n.GatewayPort))))
	}
	fmt.Fprintf(&b, "[network_identity]\nnetwork_name = %s\nnetwork_secret = %s\n", q(n.Network), q(n.Secret))
	// This adapter currently provides the cloud/NAS relay leg. Selection and NAT
	// traversal for mobile clients are deliberately not inferred from these flags.
	fmt.Fprintf(&b, `[flags]
no_tun = true
use_smoltcp = true
enable_encryption = true
private_mode = true
relay_network_whitelist = %s
disable_p2p = true
need_p2p = false
disable_tcp_hole_punching = true
disable_udp_hole_punching = true
disable_sym_hole_punching = true
disable_upnp = true
enable_ipv6 = false
accept_dns = false
bind_device = false
`, q(n.Network))
	// Deny host access and subnet forwarding except the NAS HTTPS gateway.
	// In no-TUN mode EasyTier otherwise maps virtual-IP ports to host loopback.
	for _, chain := range []int{1, 3} {
		fmt.Fprintf(&b, "[[acl.acl_v1.chains]]\nname = \"mf_gateway_%d\"\nchain_type = %d\nenabled = true\ndefault_action = 2\n", chain, chain)
		if n.IP == NASIP {
			fmt.Fprintf(&b, "[[acl.acl_v1.chains.rules]]\nname = \"gateway_only\"\npriority = 1000\nenabled = true\nprotocol = 1\nports = [%s]\ndestination_ips = [%s]\naction = 1\nstateful = true\n", q(strconv.Itoa(n.GatewayPort)), q(NASIP+"/32"))
		}
	}
	return []byte(b.String()), nil
}

func endpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "tcp" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid EasyTier TCP endpoint")
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || p < 1 || p > 65535 {
		return errors.New("invalid EasyTier TCP port")
	}
	return nil
}

func CheckBinary(ctx context.Context, binary string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return errors.New("cannot run EasyTier core")
	}
	v := strings.TrimSpace(strings.TrimPrefix(string(b), "easytier-core "))
	if v != core.EasyTierVersion && !strings.HasPrefix(v, core.EasyTierVersion+"-") {
		return errors.New("EasyTier version does not match pinned release")
	}
	return nil
}

type Process struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func Start(ctx context.Context, binary, config string, rpcPort int) (*Process, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := exec.CommandContext(verifyCtx, binary, "-c", config, "--disable-env-parsing", "--check-config").Run()
	cancel()
	if err != nil {
		return nil, errors.New("EasyTier rejected generated configuration")
	}
	cmd := exec.Command(binary, "-c", config, "--disable-env-parsing", "--rpc-portal", net.JoinHostPort("127.0.0.1", strconv.Itoa(rpcPort)), "--console-log-level", "error")
	// Upstream startup output can include its entire configuration and secret.
	// Keep it out of the product log; status reports sanitized process failures.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}
func (p *Process) Alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
func (p *Process) PID() int {
	if !p.Alive() {
		return 0
	}
	return p.cmd.Process.Pid
}
func (p *Process) Stop() {
	if p == nil || !p.Alive() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
