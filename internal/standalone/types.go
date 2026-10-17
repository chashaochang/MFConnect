package standalone

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

const BasePath = "/graphql/api/mfconnect/v1"
const MaxChunk = 65536
const BufferSize = 262144
const MaxRequest = 98304
const MaxOffset int64 = 9007199254740991

type Fault struct {
	Status     int    `json:"-"`
	Code       string `json:"code"`
	Retryable  bool   `json:"retryable"`
	Expected   *int64 `json:"expected_offset,omitempty"`
	RetryAfter int    `json:"retry_after_ms,omitempty"`
}

func fault(status int, code string) *Fault {
	return &Fault{Status: status, Code: code, Retryable: status == 503 || status == 429 || code == "STREAM_BUSY"}
}
func offsetFault(n int64) *Fault { e := fault(409, "OFFSET_MISMATCH"); e.Expected = &n; return e }
func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, e *Fault) {
	respond(w, e.Status, map[string]any{"protocol": 1, "error": e})
}

type Target struct {
	Origin string `json:"upstream_origin"`
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Path   string `json:"entry_path"`
	Wire   string `json:"wire_protocol"`
	TLS    string `json:"tls_mode"`
}
type Application struct {
	ID          string  `json:"application_id"`
	ContainerID string  `json:"container_id"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Revision    string  `json:"target_revision"`
	Target      *Target `json:"target,omitempty"`
	// Numeric IP:port only. Never supplied by a remote request or DNS at dial time.
	DialAddresses []string `json:"-"`
}
type Catalog struct {
	State        string
	ScannedAt    int64
	Applications []Application
}
type CatalogProvider interface{ Snapshot() Catalog }
type DialFunc func(context.Context, string, string) (net.Conn, error)

type Options struct {
	NASID   string
	Auth    Authenticator
	Catalog CatalogProvider
	Dial    DialFunc
	Now     func() time.Time
}
