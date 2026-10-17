package core

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestPortalProofBoundToRequest(t *testing.T) {
	r := httptest.NewRequest("POST", "https://home.example.test/login", nil)
	now := time.Now()
	secret := Hash(Random())
	r.Header.Set(PortalProofHeader, PortalProof(r, "device", secret, now))
	if !ValidPortalProof(r, "device", secret, now) {
		t.Fatal("valid proof rejected")
	}
	if ValidPortalProof(r, "other", secret, now) || ValidPortalProof(r, "device", Hash("wrong"), now) || ValidPortalProof(r, "device", secret, now.Add(time.Minute)) {
		t.Fatal("bad identity/time accepted")
	}
	r.URL.Path = "/Main"
	if ValidPortalProof(r, "device", secret, now) {
		t.Fatal("path substitution accepted")
	}
	r.URL.Path = "/login"
	r.Host = "other.example.test"
	if ValidPortalProof(r, "device", secret, now) {
		t.Fatal("host substitution accepted")
	}
}
