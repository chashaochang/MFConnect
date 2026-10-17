package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/easytier"
	"mfconnect/internal/store"
)

type EasyTierConfig struct {
	Binary     string `json:"binary"`
	RunDir     string `json:"run_dir"`
	ListenHost string `json:"listen_host"`
	PortStart  int    `json:"port_start"`
	PortEnd    int    `json:"port_end"`
}

func validateGateway(d store.Device, in core.SyncRequest) (string, error) {
	if in.GatewayPort < 1024 || in.GatewayPort > 65535 {
		return "", errors.New("invalid gateway port")
	}
	der, err := base64.RawStdEncoding.DecodeString(in.GatewayCertificate)
	if err != nil || len(der) > 4096 {
		return "", errors.New("invalid gateway certificate")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	registered, _ := base64.RawStdEncoding.DecodeString(d.PublicKey)
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(pub, registered) || time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
		return "", errors.New("certificate does not identify enrolled device")
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return "", err
	}
	hash := sha256.Sum256(der)
	return hex.EncodeToString(hash[:]), nil
}

func (s *Server) deviceAccess(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8193))
	if err != nil || len(b) > 8192 {
		reject(w, 400, "invalid_request")
		return
	}
	d, err := s.authenticateDevice(r, b)
	if err != nil {
		reject(w, 401, "invalid_device_signature")
		return
	}
	var in core.AccessRequest
	if json.Unmarshal(b, &in) != nil || len(in.Session) < 32 || len(in.Session) > 256 {
		reject(w, 400, "invalid_request")
		return
	}
	st := s.Store.View()
	session, ok := st.Sessions[core.Hash(in.Session)]
	svc, found := st.Services[session.ServiceID]
	p, present := st.Principals[session.UserID]
	if !ok || !found || !present || session.Expires <= time.Now().Unix() || svc.DeviceID != d.ID || !svc.Published || session.Revision != svc.Revision || (!s.serviceHost(svc, in.Host) || !s.sessionHost(session, svc, in.Host)) || !store.Authorized(&st, p, svc) || (st.Devices[d.ID].Transport != "easytier" && st.Devices[d.ID].GatewayPin == "") {
		reject(w, 401, "invalid_session")
		return
	}
	respond(w, 200, core.AccessResponse{ServiceID: svc.ID, Target: svc.Target, Revision: svc.Revision, Expires: session.Expires})
}

type easyTierRoute struct {
	process *easytier.Process
	port    int
	policy  string
}
type easyTierManager struct {
	mu     sync.Mutex
	routes map[string]*easyTierRoute
	done   chan struct{}
}

// StartEasyTier supervises one isolated upstream process per enrolled NAS.
// This is a pilot deployment model; no credentials are shared across devices.
func (s *Server) StartEasyTier(ctx context.Context) (func(), error) {
	c := s.Config.EasyTier
	if c == nil {
		return func() {}, nil
	}
	if err := easytier.CheckBinary(ctx, c.Binary); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &easyTierManager{routes: map[string]*easyTierRoute{}, done: make(chan struct{})}
	s.easy = m
	go func() {
		defer close(m.done)
		defer func() {
			for _, r := range m.routes {
				r.process.Stop()
			}
		}()
		for {
			state := s.Store.View()
			m.mu.Lock()
			for id, r := range m.routes {
				d, ok := state.Devices[id]
				if !ok || d.Revoked || d.Transport != "easytier" {
					r.process.Stop()
					delete(m.routes, id)
				}
			}
			for id, d := range state.Devices {
				if d.Revoked || d.Transport != "easytier" || d.EasyTierPort == 0 {
					continue
				}
				policy := core.Hash(d.EasyTierSecret + ":" + strconv.Itoa(d.EasyTierPort) + ":" + strconv.Itoa(d.GatewayPort))
				r := m.routes[id]
				if r != nil && r.process.Alive() && r.policy == policy {
					continue
				}
				if r != nil {
					r.process.Stop()
				}
				delete(m.routes, id)
				port, err := easytier.FreePort()
				if err != nil {
					continue
				}
				n := easytier.Node{Name: "cloud", Network: id, Secret: d.EasyTierSecret, IP: "10.144.0.1", Listeners: []string{"tcp://" + net.JoinHostPort(c.ListenHost, strconv.Itoa(d.EasyTierPort))}, GatewayPort: d.GatewayPort, ForwardPort: port}
				encoded, err := n.Config()
				if err != nil {
					continue
				}
				path := filepath.Join(c.RunDir, id+".toml")
				if err = core.WritePrivate(path, encoded); err != nil {
					continue
				}
				proc, err := easytier.Start(ctx, c.Binary, path, 0)
				if err == nil {
					m.routes[id] = &easyTierRoute{process: proc, port: port, policy: policy}
				}
			}
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return func() { cancel(); <-m.done }, nil
}

func (s *Server) easyTierEndpoint(id string) (string, error) {
	if s.easy == nil {
		return "", errors.New("EasyTier is not running")
	}
	s.easy.mu.Lock()
	defer s.easy.mu.Unlock()
	r := s.easy.routes[id]
	if r == nil || !r.process.Alive() {
		return "", errors.New("EasyTier device route unavailable")
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port)), nil
}
