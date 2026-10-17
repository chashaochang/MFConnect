package updates

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSignedReleases(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	public := base64.StdEncoding.EncodeToString(pub)
	now := time.Now()
	valid := Release{ID: "plugin-0.4.0", Component: "plugin", Version: "0.4.0", Architecture: "linux/amd64", Protocol: 1, URL: "https://updates.example/mfconnect-0.4.0.plg", SHA256: strings.Repeat("a", 64), Size: 100, IssuedAt: now.Unix(), ExpiresAt: now.Unix() + 3600}
	sign := func(r Release) SignedRelease {
		b, _ := json.Marshal(r)
		return SignedRelease{base64.StdEncoding.EncodeToString(b), base64.StdEncoding.EncodeToString(ed25519.Sign(key, b))}
	}
	s := sign(valid)
	if _, e := s.Verify(public, now); e != nil {
		t.Fatal(e)
	}
	s.Payload = base64.StdEncoding.EncodeToString([]byte(`{"version":"9.9.9"}`))
	if _, e := s.Verify(public, now); e == nil {
		t.Fatal("tampered payload accepted")
	}
	for _, change := range []func(*Release){func(r *Release) { r.URL = "http://updates.example/app.plg" }, func(r *Release) { r.URL = "https://user:password@updates.example/app.plg" }, func(r *Release) { r.ExpiresAt = now.Unix() - 1 }, func(r *Release) { r.Size = 1 << 31 }, func(r *Release) { r.Version = "1;reboot" }, func(r *Release) { r.Architecture = "linux/arm64" }, func(r *Release) { r.SHA256 = "123" }} {
		r := valid
		change(&r)
		if _, e := sign(r).Verify(public, now); e == nil {
			t.Fatalf("invalid release accepted: %+v", r)
		}
	}
	if Compare("0.10.0", "0.9.99") != 1 || Compare("0.4.0", "0.4.0") != 0 || Compare("0.3.1", "0.4.0") != -1 {
		t.Fatal("version comparison")
	}
}
