package connector

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mfconnect/internal/core"
)

func TestEndpointConfigurationAndDescriptorTrust(t *testing.T) {
	c := Config{Server: "https://example.test", EntryOrigin: "https://example.test", APIBaseURL: "https://example.test/api"}
	if err := validateEndpoints(c); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"https://evil.test/api", "https://example.test/api/../v1", "https://example.test/api/", "https://example.test/api?x=1"} {
		bad := c
		bad.APIBaseURL = base
		if validateEndpoints(bad) == nil {
			t.Fatal("untrusted API base", base)
		}
	}
	legacy := Config{Server: "https://api.legacy.test"}
	if legacy.APIBase() != legacy.Server || validateEndpoints(legacy) != nil {
		t.Fatal("legacy config changed")
	}
	descriptor := map[string]any{"endpoint_contract": 2, "connect": core.ConnectInfo{}, "api_origin": c.Server, "api_base_url": c.APIBaseURL, "entry_origin": c.EntryOrigin, "canonical_api_base_url": c.APIBaseURL, "browser_domain": "example.test", "apps_domain": "example.test", "capabilities": map[string]bool{"unified_entry": true}}
	encode := func() json.RawMessage { b, _ := json.Marshal(descriptor); return b }
	if err := validateDescriptor(c, encode()); err != nil {
		t.Fatal(err)
	}
	descriptor["api_base_url"] = "https://evil.test/api"
	if validateDescriptor(c, encode()) == nil {
		t.Fatal("external active endpoint accepted")
	}
	descriptor["api_base_url"] = c.APIBaseURL
	descriptor["apps_domain"] = "other.test"
	if validateDescriptor(c, encode()) == nil {
		t.Fatal("external application scope accepted")
	}
	for _, origin := range []string{"http://example.test", "https://example.test/api", "https://example.test?", "https://u@example.test"} {
		if validateServer(origin) == nil {
			t.Fatal("invalid origin", origin)
		}
	}
}

func TestUnifiedConnectorSignsPhysicalAPIPathAndNeverFollowsRedirect(t *testing.T) {
	var c Config
	redirect := false
	foreignCalls := 0
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls++; w.WriteHeader(200) }))
	defer foreign.Close()
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/api/v1/device/connect" || r.Header.Get("X-MF-Endpoint-Contract") != "2" {
			t.Error("wrong base or contract", r.URL.RequestURI())
		}
		b, _ := io.ReadAll(r.Body)
		key, _ := base64.RawStdEncoding.DecodeString(c.PrivateKey)
		sig, _ := base64.RawStdEncoding.DecodeString(r.Header.Get("X-MF-Signature"))
		msg := core.SignatureMessage(r.Method, r.URL.RequestURI(), r.Header.Get("X-MF-Time"), r.Header.Get("X-MF-Nonce"), b)
		if !ed25519.Verify(ed25519.PrivateKey(key).Public().(ed25519.PublicKey), msg, sig) {
			t.Error("API prefix excluded from signature")
		}
		if redirect {
			http.Redirect(w, r, foreign.URL+"/api/v1/device/connect", 307)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"endpoint_contract": 2, "connect": core.ConnectInfo{}, "api_origin": c.Server, "api_base_url": c.APIBaseURL, "entry_origin": c.EntryOrigin, "canonical_api_base_url": c.APIBaseURL, "browser_domain": "127.0.0.1", "apps_domain": "127.0.0.1", "capabilities": map[string]bool{"unified_entry": true}})
	}))
	defer cloud.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cloud.Certificate().Raw}), 0600)
	file := filepath.Join(dir, "config.json")
	if err := InitEntry(file, cloud.URL, "NAS", ca); err != nil {
		t.Fatal(err)
	}
	var err error
	c, err = Load(file)
	if err != nil {
		t.Fatal(err)
	}
	c.DeviceID = "dev_test"
	if _, err := ConnectVersion(context.Background(), c, json.RawMessage(`{"action":"endpoints"}`), 2); err != nil {
		t.Fatal(err)
	}
	redirect = true
	if _, err := ConnectVersion(context.Background(), c, json.RawMessage(`{"action":"session"}`), 2); err == nil {
		t.Fatal("redirect accepted")
	}
	if foreignCalls != 0 {
		t.Fatal("credentials followed cross-domain redirect")
	}
	before, _ := os.ReadFile(file)
	if err := Provision(context.Background(), file, cloud.URL, "unused"); err == nil {
		t.Fatal("provision silently changed API mode")
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatal("existing configuration overwritten")
	}
}
