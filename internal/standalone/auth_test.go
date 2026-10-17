package standalone

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealAuthAdapterRequiresProtectedIdentityAndAdmin(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "api.sock")
	l, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "me { id roles }") || strings.Contains(string(b), "__typename") {
			t.Error("unprotected auth query")
		}
		switch r.Header.Get("X-API-Key") {
		case "admin":
			io.WriteString(w, `{"data":{"me":{"id":"key-admin","roles":["ADMIN"]}}}`)
		case "readonly":
			io.WriteString(w, `{"data":{"me":{"id":"key-read","roles":["VIEWER"]}}}`)
		case "partial":
			io.WriteString(w, `{"data":{"me":{"id":"key-admin","roles":["ADMIN"]}},"errors":[{"message":"unauthorized"}]}`)
		default:
			io.WriteString(w, `{"errors":[{"message":"unauthorized"}]}`)
		}
	})}
	go s.Serve(l)
	defer s.Close()
	auth := NewNASAuthenticator(socket)
	for _, tc := range []struct{ key, code string }{{"admin", ""}, {"readonly", "PERMISSION_DENIED"}, {"partial", "AUTHENTICATION_REQUIRED"}, {"invalid", "AUTHENTICATION_REQUIRED"}} {
		id, e := auth.Verify(context.Background(), Credential{Type: "unraid_api_key", Key: tc.key})
		if tc.code == "" {
			if e != nil || id != "key-admin" {
				t.Fatal(id, e)
			}
		} else if e == nil || e.Code != tc.code {
			t.Fatal(tc, e)
		}
	}
}
