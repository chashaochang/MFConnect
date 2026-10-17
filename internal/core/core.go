package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mfconnect/internal/updates"
)

const Version = "0.7.0"
const FRPVersion = "0.71.1"
const EasyTierVersion = "2.6.4"

var Identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func Random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func RandomHex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func ID(prefix string) string { return prefix + "_" + RandomHex() }
func Hash(s string) string    { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func WriteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivate(path, append(b, '\n'))
}

func WritePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".mfconnect-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func ReadJSON(path string, value any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

type Target struct {
	Key               string `json:"key"`
	Name              string `json:"name"`
	URL               string `json:"url"`
	CertificateSHA256 string `json:"certificate_sha256,omitempty"`
}

func (t Target) Validate() error {
	if !Identifier.MatchString(t.Key) {
		return errors.New("invalid target key")
	}
	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("target must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid target port")
		}
	}
	if t.CertificateSHA256 != "" {
		b, err := hex.DecodeString(t.CertificateSHA256)
		if err != nil || len(b) != 32 || u.Scheme != "https" {
			return errors.New("certificate pin must be a SHA-256 hex digest for HTTPS")
		}
	}
	return nil
}

func SafePath(p string) bool {
	if p == "" {
		return true
	}
	u, err := url.Parse(p)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return false
	}
	decoded, err := url.PathUnescape(u.EscapedPath())
	return err == nil && strings.HasPrefix(decoded, "/") && !strings.HasPrefix(decoded, "//") && !strings.ContainsAny(decoded, "\\\r\n\x00") && !strings.HasPrefix(decoded, "/_mf/")
}

func SignatureMessage(method, path, stamp, nonce string, body []byte) []byte {
	return []byte(strings.Join([]string{method, path, stamp, nonce, Hash(string(body))}, "\n"))
}

func Sign(req *http.Request, deviceID string, key ed25519.PrivateKey, body []byte) {
	stamp, nonce := fmt.Sprint(time.Now().Unix()), Random()
	req.Header.Set("X-MF-Device", deviceID)
	req.Header.Set("X-MF-Time", stamp)
	req.Header.Set("X-MF-Nonce", nonce)
	sig := ed25519.Sign(key, SignatureMessage(req.Method, req.URL.RequestURI(), stamp, nonce, body))
	req.Header.Set("X-MF-Signature", base64.RawStdEncoding.EncodeToString(sig))
}

type Assignment struct {
	ServiceID  string `json:"service_id"`
	Key        string `json:"key"`
	RemotePort int    `json:"remote_port"`
	Published  bool   `json:"published"`
}

type SyncRequest struct {
	Docker             *DockerReport     `json:"docker,omitempty"`
	PortalSupported    bool              `json:"portal_supported,omitempty"`
	Software           *updates.Software `json:"software,omitempty"`
	UpdateReport       *updates.Report   `json:"update_report,omitempty"`
	Targets            []Target          `json:"targets"`
	Network            *NetworkReport    `json:"network,omitempty"`
	Transport          string            `json:"transport,omitempty"`
	GatewayPort        int               `json:"gateway_port,omitempty"`
	GatewayCertificate string            `json:"gateway_certificate,omitempty"`
}
type SyncResponse struct {
	Portal      *PortalAssignment   `json:"portal,omitempty"`
	UpdateJob   *updates.Job        `json:"update_job,omitempty"`
	Gateway     bool                `json:"gateway,omitempty"`
	TunnelHost  string              `json:"tunnel_host"`
	TunnelPort  int                 `json:"tunnel_port"`
	Assignments []Assignment        `json:"assignments"`
	Transport   string              `json:"transport,omitempty"`
	EasyTier    *EasyTierAssignment `json:"easytier,omitempty"`
}

type EasyTierAssignment struct {
	Network string `json:"network"`
	Secret  string `json:"secret"`
	Peer    string `json:"peer"`
}

type AccessRequest struct {
	Host    string `json:"host"`
	Session string `json:"session"`
}

type AccessResponse struct {
	ServiceID string `json:"service_id"`
	Target    Target `json:"target"`
	Revision  int    `json:"revision"`
	Expires   int64  `json:"expires"`
}
