package standalone

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testAuth struct{ revoked atomic.Bool }

func (a *testAuth) Verify(_ context.Context, c Credential) (string, *Fault) {
	if a.revoked.Load() || c.Key != "valid-existing-nas-key" {
		return "", fault(401, "AUTHENTICATION_REQUIRED")
	}
	return "administrator", nil
}

type testCatalog struct {
	mu sync.Mutex
	c  Catalog
}

func (c *testCatalog) Snapshot() Catalog {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.c
	out.Applications = append([]Application(nil), c.c.Applications...)
	return out
}

type harness struct {
	t       *testing.T
	s       *Server
	auth    *testAuth
	catalog *testCatalog
	outer   *httptest.Server
	token   string
	target  Application
}

func newHarness(t *testing.T, address string) *harness {
	t.Helper()
	a := Application{ID: "application_a", ContainerID: strings.Repeat("a", 64), Name: "Fixture", State: "ready", Revision: Revision(address), DialAddresses: []string{address}}
	c := &testCatalog{c: Catalog{State: "ready", ScannedAt: time.Now().Unix(), Applications: []Application{a}}}
	auth := &testAuth{}
	s := New(Options{NASID: "nas_fixture", Auth: auth, Catalog: c})
	outer := httptest.NewServer(s)
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	t.Cleanup(func() { cancel(); outer.Close() })
	h := &harness{t: t, s: s, auth: auth, catalog: c, outer: outer, target: a}
	code, res := h.call("auth", map[string]any{"protocol": 1, "auth_type": "unraid_api_key"}, "", map[string]string{"X-API-Key": "valid-existing-nas-key"})
	if code != 200 {
		t.Fatalf("auth %d %v", code, res)
	}
	h.token = res["access_token"].(string)
	return h
}
func (h *harness) call(op string, body any, token string, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	method := "POST"
	if op == "targets" || op == "capability" {
		method = "GET"
		b = nil
	}
	r, _ := http.NewRequest(method, h.outer.URL+BasePath+"/"+op, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 25 * time.Second}
	res, err := client.Do(r)
	if err != nil {
		h.t.Errorf("request: %v", err)
		return 0, nil
	}
	defer res.Body.Close()
	var out map[string]any
	if json.NewDecoder(res.Body).Decode(&out) != nil {
		h.t.Errorf("invalid response")
	}
	return res.StatusCode, out
}
func (h *harness) open() string {
	h.t.Helper()
	code, out := h.call("open", map[string]any{"protocol": 1, "request_id": fmt.Sprintf("request_%016d", time.Now().UnixNano()), "application_id": h.target.ID, "target_revision": h.target.Revision}, h.token, nil)
	if code != 200 {
		h.t.Fatalf("open %d %v", code, out)
	}
	return out["stream_id"].(string)
}
func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

type rpcConn struct {
	h              *harness
	id             string
	sent, received int64
	buf            []byte
	eof            bool
}

func (c *rpcConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > MaxChunk {
			n = MaxChunk
		}
		code, out := c.h.call("push", map[string]any{"protocol": 1, "stream_id": c.id, "offset": c.sent, "data": base64.StdEncoding.EncodeToString(p[:n]), "eof": false}, c.h.token, nil)
		if code != 200 {
			return total, fmt.Errorf("push: %d %v", code, out)
		}
		c.sent += int64(n)
		p = p[n:]
		total += n
	}
	return total, nil
}
func (c *rpcConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.eof {
			return 0, io.EOF
		}
		code, out := c.h.call("pull", map[string]any{"protocol": 1, "stream_id": c.id, "offset": c.received, "max_bytes": MaxChunk, "wait_ms": 1000}, c.h.token, nil)
		if code != 200 {
			return 0, fmt.Errorf("pull: %d %v", code, out)
		}
		c.buf, _ = base64.StdEncoding.DecodeString(out["data"].(string))
		c.received = int64(out["next_offset"].(float64))
		c.eof = out["eof"].(bool)
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}
func (c *rpcConn) Close() error {
	c.h.call("close", map[string]any{"protocol": 1, "stream_id": c.id}, c.h.token, nil)
	return nil
}
func (c *rpcConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *rpcConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *rpcConn) SetDeadline(time.Time) error      { return nil }
func (c *rpcConn) SetReadDeadline(time.Time) error  { return nil }
func (c *rpcConn) SetWriteDeadline(time.Time) error { return nil }

func TestTransportHTTPUploadRangeAndCredentials(t *testing.T) {
	payload := bytes.Repeat([]byte("range-upload-data"), 32768)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" || strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("outer credentials leaked")
		}
		if r.Method == "POST" {
			b, e := io.ReadAll(r.Body)
			if e != nil || !bytes.Equal(b, payload) {
				t.Error("upload differs")
			}
			w.Write(b)
			return
		}
		http.ServeContent(w, r, "data", time.Time{}, bytes.NewReader(payload))
	}))
	defer backend.Close()
	h := newHarness(t, strings.TrimPrefix(backend.URL, "http://"))
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return &rpcConn{h: h, id: h.open()}, nil }}}
	defer client.CloseIdleConnections()
	r, _ := http.NewRequest("GET", backend.URL, nil)
	r.Header.Set("Range", "bytes=123-4567")
	res, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 206 || !bytes.Equal(b, payload[123:4568]) {
		t.Fatal("range mismatch")
	}
	res, e = client.Post(backend.URL, "application/octet-stream", bytes.NewReader(payload))
	if e != nil {
		t.Fatal(e)
	}
	b, e = io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || !bytes.Equal(b, payload) {
		t.Fatal("echo upload differs", e)
	}
}
func TestEndToEndTLS(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "encrypted app body") }))
	defer backend.Close()
	h := newHarness(t, strings.TrimPrefix(backend.URL, "https://"))
	roots := x509.NewCertPool()
	roots.AddCert(backend.Certificate())
	for _, trust := range []bool{true, false} {
		c := &rpcConn{h: h, id: h.open()}
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "example.com"}
		if trust {
			cfg.RootCAs = roots
		}
		conn := tls.Client(c, cfg)
		err := conn.Handshake()
		if trust {
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
			res, e := http.ReadResponse(bufio.NewReader(conn), nil)
			if e != nil {
				t.Fatal(e)
			}
			b, _ := io.ReadAll(res.Body)
			if string(b) != "encrypted app body" {
				t.Fatal(string(b))
			}
		} else if err == nil {
			t.Fatal("untrusted container certificate accepted")
		}
		c.Close()
	}
}
func TestReplayOrderingHalfCloseAndCrossSession(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	go func() {
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		c.Write(append([]byte("after-fin:"), b...))
	}()
	h := newHarness(t, l.Addr().String())
	id := h.open()
	body := map[string]any{"protocol": 1, "stream_id": id, "offset": 0, "data": base64.StdEncoding.EncodeToString([]byte("once")), "eof": true}
	for i := 0; i < 2; i++ {
		status, out := h.call("push", body, h.token, nil)
		if status != 200 || out["next_offset"] != float64(4) {
			t.Fatal(status, out)
		}
	}
	body["data"] = base64.StdEncoding.EncodeToString([]byte("evil"))
	status, out := h.call("push", body, h.token, nil)
	if status != 409 || errCode(out) != "OFFSET_MISMATCH" {
		t.Fatal(status, out)
	}
	_, auth := h.call("auth", map[string]any{"protocol": 1, "auth_type": "unraid_api_key"}, "", map[string]string{"X-API-Key": "valid-existing-nas-key"})
	other := auth["access_token"].(string)
	status, _ = h.call("pull", map[string]any{"protocol": 1, "stream_id": id, "offset": 0, "max_bytes": 64, "wait_ms": 1000}, other, nil)
	if status != 410 {
		t.Fatal("cross-session stream allowed")
	}
	pull := map[string]any{"protocol": 1, "stream_id": id, "offset": 0, "max_bytes": 64, "wait_ms": 1000}
	status, out = h.call("pull", pull, h.token, nil)
	if status != 200 {
		t.Fatal(status, out)
	}
	data, _ := base64.StdEncoding.DecodeString(out["data"].(string))
	if string(data) != "after-fin:once" {
		t.Fatal(string(data))
	}
	_, again := h.call("pull", pull, h.token, nil)
	if out["data"] != again["data"] || out["next_offset"] != again["next_offset"] {
		t.Fatal("pull replay changed")
	}
	h.call("close", map[string]any{"protocol": 1, "stream_id": id}, h.token, nil)
}
func TestCancellationLimitsAndRevocation(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	var conns sync.WaitGroup
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			conns.Add(1)
			go func() { defer conns.Done(); defer c.Close(); io.Copy(io.Discard, c) }()
		}
	}()
	h := newHarness(t, l.Addr().String())
	id := h.open()
	done := make(chan string, 1)
	go func() {
		_, r := h.call("pull", map[string]any{"protocol": 1, "stream_id": id, "offset": 0, "max_bytes": 64, "wait_ms": 20000}, h.token, nil)
		done <- errCode(r)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.s.mu.Lock()
		sess := h.s.sessions[h.token]
		h.s.mu.Unlock()
		st, _ := getStream(sess, id)
		st.mu.Lock()
		busy := st.pulling
		st.mu.Unlock()
		if busy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	status, out := h.call("pull", map[string]any{"protocol": 1, "stream_id": id, "offset": 0, "max_bytes": 64, "wait_ms": 0}, h.token, nil)
	if status != 409 || errCode(out) != "STREAM_BUSY" {
		t.Fatal(status, out)
	}
	h.call("close", map[string]any{"protocol": 1, "stream_id": id}, h.token, nil)
	select {
	case code := <-done:
		if code != "STREAM_CLOSED" {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release long poll")
	}
	for i := 0; i < 8; i++ {
		h.open()
	}
	status, out = h.call("open", map[string]any{"protocol": 1, "request_id": "over_limit_request", "application_id": h.target.ID, "target_revision": h.target.Revision}, h.token, nil)
	if status != 429 {
		t.Fatal(status, out)
	}
	h.auth.revoked.Store(true)
	h.s.mu.Lock()
	sess := h.s.sessions[h.token]
	h.s.mu.Unlock()
	sess.mu.Lock()
	sess.verified = time.Now().Add(-6 * time.Second)
	sess.mu.Unlock()
	status, _ = h.call("targets", nil, h.token, nil)
	if status != 401 {
		t.Fatal("revoked authorization accepted")
	}
	l.Close()
	conns.Wait()
}
func TestBoundedBackpressure(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	s := newStream("test", Application{}, a, time.Now())
	defer s.close(fault(410, "STREAM_CLOSED"))
	data := bytes.Repeat([]byte{7}, MaxChunk)
	offset := int64(0)
	blocked := false
	for i := 0; i < 10; i++ {
		r, e := s.push(offset, data, false, time.Now())
		if e != nil {
			if e.Code != "BACKPRESSURE" {
				t.Fatal(e)
			}
			blocked = true
			break
		}
		offset = r.Next
	}
	if !blocked || offset > BufferSize+MaxChunk {
		t.Fatal("unbounded input queue", offset)
	}
}
func TestWebSocketInsideOrdinaryHTTP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		rw.Flush()
		frame := make([]byte, 8)
		if _, e := io.ReadFull(rw, frame); e != nil {
			return
		}
		if frame[0] != 0x81 || frame[1] != 0x82 {
			t.Error("not masked WebSocket frame")
			return
		}
		c.Write([]byte{0x81, 2, frame[6] ^ frame[2], frame[7] ^ frame[3]})
	}))
	defer backend.Close()
	h := newHarness(t, strings.TrimPrefix(backend.URL, "http://"))
	c := &rpcConn{h: h, id: h.open()}
	defer c.Close()
	fmt.Fprint(c, "GET /socket HTTP/1.1\r\nHost: fixture\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	r := bufio.NewReader(c)
	res, e := http.ReadResponse(r, nil)
	if e != nil || res.StatusCode != 101 {
		t.Fatal(res, e)
	}
	c.Write([]byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2})
	b := make([]byte, 4)
	if _, e := io.ReadFull(r, b); e != nil || !bytes.Equal(b, []byte{0x81, 2, 'h', 'i'}) {
		t.Fatal(b, e)
	}
}

func TestOpenIdempotencyTargetChangeAndSessionRenewal(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(io.Discard, c) }()
		}
	}()
	h := newHarness(t, l.Addr().String())
	body := map[string]any{"protocol": 1, "request_id": "same_open_request_id", "application_id": h.target.ID, "target_revision": h.target.Revision}
	_, first := h.call("open", body, h.token, nil)
	_, second := h.call("open", body, h.token, nil)
	if first["stream_id"] != second["stream_id"] {
		t.Fatal("open retry created another connection")
	}
	body["target_revision"] = strings.Repeat("b", 64)
	code, out := h.call("open", body, h.token, nil)
	if code != 409 || errCode(out) != "REQUEST_ID_CONFLICT" {
		t.Fatal(code, out)
	}
	code, out = h.call("auth", map[string]any{"protocol": 1, "auth_type": "session"}, h.token, nil)
	if code != 200 || out["access_token"] != h.token {
		t.Fatal("renew rotated token", code, out)
	}
	h.catalog.mu.Lock()
	h.catalog.c.Applications[0].Revision = "changed"
	h.catalog.mu.Unlock()
	time.Sleep(1100 * time.Millisecond)
	code, out = h.call("push", map[string]any{"protocol": 1, "stream_id": first["stream_id"], "offset": 0, "data": "eA==", "eof": false}, h.token, nil)
	if code != 409 || errCode(out) != "TARGET_CHANGED" {
		t.Fatal("stale target remained live", code, out)
	}
	h.s.mu.Lock()
	sess := h.s.sessions[h.token]
	h.s.mu.Unlock()
	sess.mu.Lock()
	sess.expires = time.Now().Add(-time.Second)
	sess.mu.Unlock()
	code, _ = h.call("targets", nil, h.token, nil)
	if code != 401 {
		t.Fatal("expired session accepted")
	}
}

func TestIPv6TCPAndBrowserOriginRejected(t *testing.T) {
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer l.Close()
	go func() {
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("ipv6"))
	}()
	h := newHarness(t, l.Addr().String())
	c := &rpcConn{h: h, id: h.open()}
	defer c.Close()
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "ipv6" {
		t.Fatal(string(b), err)
	}
	code, out := h.call("capability", nil, "", map[string]string{"Origin": "http://application.invalid"})
	if code != 403 || errCode(out) != "ORIGIN_FORBIDDEN" {
		t.Fatal(code, out)
	}
}
