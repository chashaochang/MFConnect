package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var connectIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

func NormalizeConnectID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
func ValidConnectID(id string) bool {
	if !connectIDPattern.MatchString(id) {
		return false
	}
	switch id {
	case "www", "api", "admin", "login", "logout", "auth", "connect", "mfconnect", "relay", "apps", "downloads", "healthz", "static", "mail", "support":
		return false
	}
	return true
}

type ConnectInfo struct {
	ID       string `json:"id"`
	Enabled  bool   `json:"enabled"`
	EntryURL string `json:"entry_url"`
	Origin   string `json:"origin"`
	Online   bool   `json:"online"`
}
type PortalAssignment struct {
	ConnectInfo
	RemotePort int `json:"remote_port"`
}

const PortalProofHeader = "X-MF-Portal-Proof"

// This proof authenticates the cloud hop to the pinned NAS TLS gateway.
// It is never forwarded to WebGUI; WebGUI independently authenticates the user.
func PortalProof(r *http.Request, device, secretHash string, now time.Time) string {
	stamp, nonce := fmt.Sprint(now.Unix()), Random()
	return stamp + "." + nonce + "." + portalMAC(r, device, secretHash, stamp, nonce)
}
func portalMAC(r *http.Request, device, secretHash, stamp, nonce string) string {
	h := hmac.New(sha256.New, []byte(secretHash))
	h.Write([]byte(strings.Join([]string{"mfconnect-portal-v1", device, r.Method, r.Host, r.URL.RequestURI(), stamp, nonce}, "\n")))
	return hex.EncodeToString(h.Sum(nil))
}
func ValidPortalProof(r *http.Request, device, secretHash string, now time.Time) bool {
	p := strings.Split(r.Header.Get(PortalProofHeader), ".")
	if len(p) != 3 || len(p[1]) != 43 {
		return false
	}
	stamp, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil || stamp < now.Unix()-30 || stamp > now.Unix()+30 {
		return false
	}
	return hmac.Equal([]byte(p[2]), []byte(portalMAC(r, device, secretHash, p[0], p[1])))
}
