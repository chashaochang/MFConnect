package standalone

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"mfconnect/internal/core"
)

var requestID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

type opened struct {
	Application, Revision, ID string
	Stream                    *stream
}
type session struct {
	mu                sync.Mutex
	token, principal  string
	credential        Credential
	expires, verified time.Time
	dead              bool
	opens             map[string]opened
	streams           map[string]*stream
}
type Server struct {
	opts       Options
	mu         sync.Mutex
	sessions   map[string]*session
	authWindow time.Time
	authCount  int
	dialing    int
}

func New(opts Options) *Server {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Dial == nil {
		opts.Dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return &Server{opts: opts, sessions: map[string]*session{}}
}
func (s *Server) expire(sess *session) {
	sess.mu.Lock()
	if !sess.dead {
		sess.dead = true
		for _, st := range sess.streams {
			st.close(fault(401, "SESSION_EXPIRED"))
		}
		sess.credential = Credential{}
	}
	sess.mu.Unlock()
	s.mu.Lock()
	delete(s.sessions, sess.token)
	s.mu.Unlock()
}
func (s *Server) verify(ctx context.Context, sess *session, force bool) *Fault {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	now := s.opts.Now()
	if sess.dead || !now.Before(sess.expires) {
		return fault(401, "SESSION_EXPIRED")
	}
	if !force && now.Sub(sess.verified) < 5*time.Second {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	id, e := s.opts.Auth.Verify(ctx, sess.credential)
	if e != nil {
		return e
	}
	if id != sess.principal {
		return fault(401, "SESSION_EXPIRED")
	}
	sess.verified = now
	return nil
}
func (s *Server) getSession(r *http.Request) (*session, *Fault) {
	a := r.Header.Get("Authorization")
	if !strings.HasPrefix(a, "Bearer ") || len(a) > 200 {
		return nil, fault(401, "AUTHENTICATION_REQUIRED")
	}
	s.mu.Lock()
	sess := s.sessions[strings.TrimPrefix(a, "Bearer ")]
	s.mu.Unlock()
	if sess == nil {
		return nil, fault(401, "SESSION_EXPIRED")
	}
	if e := s.verify(r.Context(), sess, false); e != nil {
		s.expire(sess)
		return nil, e
	}
	return sess, nil
}
func (s *Server) Snapshot() Catalog {
	c := s.opts.Catalog.Snapshot()
	if c.State != "ready" || c.ScannedAt == 0 || s.opts.Now().Unix()-c.ScannedAt > 30 {
		c.State = "unavailable"
	}
	return c
}

// Run continuously invalidates active streams, including sessions without new
// requests. Network identity checks run independently to avoid a slow key
// validation postponing revocation of every other session.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			list := make([]*session, 0, len(s.sessions))
			for _, v := range s.sessions {
				list = append(list, v)
			}
			s.mu.Unlock()
			for _, v := range list {
				s.expire(v)
			}
			return
		case <-t.C:
			s.mu.Lock()
			list := make([]*session, 0, len(s.sessions))
			for _, v := range s.sessions {
				list = append(list, v)
			}
			s.mu.Unlock()
			catalog := s.Snapshot()
			revisions := map[string]string{}
			if catalog.State == "ready" {
				for _, a := range catalog.Applications {
					if a.State == "ready" {
						revisions[a.ID] = a.Revision
					}
				}
			}
			for _, sess := range list {
				// TryLock bounds background goroutines even during an API outage.
				if !sess.mu.TryLock() {
					continue
				}
				check := s.opts.Now().Sub(sess.verified) >= 5*time.Second || !s.opts.Now().Before(sess.expires)
				for _, st := range sess.streams {
					st.mu.Lock()
					idle := s.opts.Now().Sub(st.lastActivity) > 90*time.Second
					st.mu.Unlock()
					if idle {
						st.close(fault(410, "STREAM_EXPIRED"))
					} else if revisions[st.application] != st.revision {
						st.close(fault(409, "TARGET_CHANGED"))
					}
				}
				sess.mu.Unlock()
				if check {
					go func(v *session) {
						if s.verify(ctx, v, false) != nil {
							s.expire(v)
						}
					}(sess)
				}
			}
		}
	}
}
func decode(r *http.Request, v any) *Fault {
	if r.ContentLength > MaxRequest {
		return fault(413, "REQUEST_TOO_LARGE")
	}
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		return fault(400, "INVALID_REQUEST")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxRequest+1))
	if len(b) > MaxRequest {
		return fault(413, "REQUEST_TOO_LARGE")
	}
	if err != nil {
		return fault(400, "INVALID_REQUEST")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return fault(400, "INVALID_REQUEST")
	}
	return nil
}
func protocol(n int) *Fault {
	if n != 1 {
		return fault(400, "UNSUPPORTED_PROTOCOL")
	}
	return nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		failure(w, fault(403, "ORIGIN_FORBIDDEN"))
		return
	}
	if r.URL.RawQuery != "" || !strings.HasPrefix(r.URL.Path, BasePath+"/") {
		failure(w, fault(404, "NOT_FOUND"))
		return
	}
	op := strings.TrimPrefix(r.URL.Path, BasePath+"/")
	method := "POST"
	if op == "capability" || op == "targets" {
		method = "GET"
	}
	if r.Method != method {
		failure(w, fault(400, "INVALID_REQUEST"))
		return
	}
	if op == "capability" {
		respond(w, 200, map[string]any{"protocol": 1, "capability": "existing_origin_webui", "transport": "http_push_pull", "auth_types": []string{"unraid_api_key", "unraid_webgui_session"}, "authorization_policy": "nas_admin_only", "wire_protocol": "tcp", "limits": map[string]int{"max_request_bytes": MaxRequest, "max_chunk_bytes": MaxChunk, "buffer_bytes_per_direction": BufferSize, "max_streams_per_session": 8, "max_streams_total": 64, "max_poll_ms": 20000, "stream_idle_seconds": 90, "session_ttl_seconds": 1800, "auth_recheck_seconds": 5}})
		return
	}
	if op == "auth" {
		s.auth(w, r)
		return
	}
	sess, e := s.getSession(r)
	if e != nil {
		failure(w, e)
		return
	}
	if r.Header.Get("X-API-Key") != "" || r.Header.Get("Cookie") != "" {
		failure(w, fault(400, "INVALID_REQUEST"))
		return
	}
	switch op {
	case "targets":
		c := s.Snapshot()
		respond(w, 200, map[string]any{"protocol": 1, "nas_id": s.opts.NASID, "state": c.State, "scanned_at": c.ScannedAt, "targets": c.Applications})
	case "open":
		s.open(w, r, sess)
	case "push":
		s.push(w, r, sess)
	case "pull":
		s.pull(w, r, sess)
	case "close":
		s.close(w, r, sess)
	default:
		failure(w, fault(404, "NOT_FOUND"))
	}
}
func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Protocol int    `json:"protocol"`
		Type     string `json:"auth_type"`
	}
	if e := decode(r, &in); e != nil {
		failure(w, e)
		return
	}
	if e := protocol(in.Protocol); e != nil {
		failure(w, e)
		return
	}
	if in.Type == "session" {
		if r.Header.Get("X-API-Key") != "" || r.Header.Get("Cookie") != "" {
			failure(w, fault(400, "INVALID_REQUEST"))
			return
		}
		sess, e := s.getSession(r)
		if e != nil {
			failure(w, e)
			return
		}
		if e = s.verify(r.Context(), sess, true); e != nil {
			s.expire(sess)
			failure(w, e)
			return
		}
		sess.mu.Lock()
		sess.expires = s.opts.Now().Add(1800 * time.Second)
		s.authResponse(w, sess)
		sess.mu.Unlock()
		return
	}
	c := Credential{Type: in.Type, Key: r.Header.Get("X-API-Key"), Cookie: r.Header.Get("Cookie"), CSRF: r.Header.Get("X-CSRF-Token")}
	if r.Header.Get("Authorization") != "" || (c.Type == "unraid_api_key" && (len(c.Key) < 16 || len(c.Key) > 512 || c.Cookie != "" || c.CSRF != "")) ||
		(c.Type == "unraid_webgui_session" && (c.Key != "" || c.Cookie == "" || len(c.Cookie) > 8192 || c.CSRF == "" || len(c.CSRF) > 512)) || (c.Type != "unraid_api_key" && c.Type != "unraid_webgui_session") {
		failure(w, fault(400, "INVALID_REQUEST"))
		return
	}
	s.mu.Lock()
	now := s.opts.Now()
	if now.Sub(s.authWindow) > time.Minute {
		s.authWindow = now
		s.authCount = 0
	}
	s.authCount++
	limited := s.authCount > 30
	s.mu.Unlock()
	if limited {
		failure(w, fault(429, "AUTH_RATE_LIMIT"))
		return
	}
	id, e := s.opts.Auth.Verify(r.Context(), c)
	if e != nil {
		failure(w, e)
		return
	}
	sess := &session{token: core.Random(), principal: id, credential: c, expires: now.Add(1800 * time.Second), verified: now, opens: map[string]opened{}, streams: map[string]*stream{}}
	s.mu.Lock()
	if len(s.sessions) >= 16 {
		s.mu.Unlock()
		failure(w, fault(429, "SESSION_LIMIT"))
		return
	}
	s.sessions[sess.token] = sess
	s.mu.Unlock()
	sess.mu.Lock()
	s.authResponse(w, sess)
	sess.mu.Unlock()
}
func (s *Server) authResponse(w http.ResponseWriter, sess *session) {
	respond(w, 200, map[string]any{"protocol": 1, "nas_id": s.opts.NASID, "principal_id": core.Hash(sess.principal), "access_token": sess.token, "expires_at": sess.expires.Unix(), "authorization_policy": "nas_admin_only"})
}
func active(sess *session) int {
	n := 0
	for _, st := range sess.streams {
		st.mu.Lock()
		if st.closed == nil {
			n++
		}
		st.mu.Unlock()
	}
	return n
}

// The reservation counter imposes a global cap without holding the server lock
// during a dial. Per-session serialization also makes open request IDs atomic.
func (s *Server) reserve() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dialing >= 64 {
		return false
	}
	s.dialing++
	return true
}
func (s *Server) release() { s.mu.Lock(); s.dialing--; s.mu.Unlock() }
func (s *Server) open(w http.ResponseWriter, r *http.Request, sess *session) {
	var in struct {
		Protocol    int    `json:"protocol"`
		RequestID   string `json:"request_id"`
		Application string `json:"application_id"`
		Revision    string `json:"target_revision"`
	}
	if e := decode(r, &in); e != nil {
		failure(w, e)
		return
	}
	if e := protocol(in.Protocol); e != nil {
		failure(w, e)
		return
	}
	if !requestID.MatchString(in.RequestID) || !core.Identifier.MatchString(in.Application) || len(in.Revision) != 64 {
		failure(w, fault(400, "INVALID_REQUEST"))
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.dead {
		failure(w, fault(401, "SESSION_EXPIRED"))
		return
	}
	if prev, ok := sess.opens[in.RequestID]; ok {
		if prev.Application != in.Application || prev.Revision != in.Revision {
			failure(w, fault(409, "REQUEST_ID_CONFLICT"))
			return
		}
		if prev.Stream == nil {
			failure(w, fault(410, "STREAM_CLOSED"))
			return
		}
		prev.Stream.mu.Lock()
		e := prev.Stream.closed
		prev.Stream.mu.Unlock()
		if e != nil {
			failure(w, e)
			return
		}
		s.openResponse(w, prev)
		return
	}
	if len(sess.opens) >= 256 {
		failure(w, fault(429, "SESSION_REQUEST_LIMIT"))
		return
	}
	if active(sess) >= 8 {
		failure(w, fault(429, "STREAM_LIMIT"))
		return
	}
	c := s.Snapshot()
	if c.State != "ready" {
		failure(w, fault(503, "DISCOVERY_UNAVAILABLE"))
		return
	}
	var app *Application
	for i := range c.Applications {
		if c.Applications[i].ID == in.Application {
			app = &c.Applications[i]
			break
		}
	}
	if app == nil {
		failure(w, fault(404, "APPLICATION_NOT_FOUND"))
		return
	}
	if app.State == "target_forbidden" {
		failure(w, fault(403, "TARGET_FORBIDDEN"))
		return
	}
	if app.State != "ready" || len(app.DialAddresses) == 0 {
		failure(w, fault(502, "TARGET_UNREACHABLE"))
		return
	}
	if app.Revision != in.Revision {
		failure(w, fault(409, "TARGET_CHANGED"))
		return
	}
	if !s.reserve() {
		failure(w, fault(429, "STREAM_LIMIT"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var conn net.Conn
	var err error
	for _, address := range app.DialAddresses {
		host, _, e := net.SplitHostPort(address)
		if e != nil || net.ParseIP(host) == nil {
			continue
		}
		conn, err = s.opts.Dial(ctx, "tcp", address)
		if err == nil {
			break
		}
	}
	if conn == nil {
		s.release()
		failure(w, fault(502, "TARGET_UNREACHABLE"))
		return
	}
	// Re-read after dialing so a changed/disappeared Docker target cannot be
	// authorized from a snapshot which became stale during the connect.
	c = s.Snapshot()
	valid := false
	for _, a := range c.Applications {
		if c.State == "ready" && a.ID == app.ID && a.State == "ready" && a.Revision == app.Revision {
			valid = true
		}
	}
	if !valid {
		_ = conn.Close()
		s.release()
		failure(w, fault(409, "TARGET_CHANGED"))
		return
	}
	id := core.Random()
	st := newStream(id, *app, conn, s.opts.Now())
	sess.streams[id] = st
	prev := opened{in.Application, in.Revision, id, st}
	sess.opens[in.RequestID] = prev
	go func() { <-st.done; s.release() }()
	s.openResponse(w, prev)
}
func (s *Server) openResponse(w http.ResponseWriter, o opened) {
	respond(w, 200, map[string]any{"protocol": 1, "stream_id": o.ID, "application_id": o.Application, "target_revision": o.Revision, "send_offset": 0, "receive_offset": 0})
}
func getStream(sess *session, id string) (*stream, *Fault) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.dead {
		return nil, fault(401, "SESSION_EXPIRED")
	}
	st := sess.streams[id]
	if st == nil {
		return nil, fault(410, "STREAM_CLOSED")
	}
	return st, nil
}
func (s *Server) push(w http.ResponseWriter, r *http.Request, sess *session) {
	var in struct {
		Protocol int    `json:"protocol"`
		StreamID string `json:"stream_id"`
		Offset   int64  `json:"offset"`
		Data     string `json:"data"`
		EOF      bool   `json:"eof"`
	}
	if e := decode(r, &in); e != nil {
		failure(w, e)
		return
	}
	if e := protocol(in.Protocol); e != nil {
		failure(w, e)
		return
	}
	b, err := base64.StdEncoding.Strict().DecodeString(in.Data)
	if len(b) > MaxChunk {
		failure(w, fault(413, "REQUEST_TOO_LARGE"))
		return
	}
	if err != nil || base64.StdEncoding.EncodeToString(b) != in.Data || (len(b) == 0 && !in.EOF) || in.Offset < 0 || in.Offset > MaxOffset {
		failure(w, fault(400, "INVALID_REQUEST"))
		return
	}
	st, e := getStream(sess, in.StreamID)
	if e != nil {
		failure(w, e)
		return
	}
	out, e := st.push(in.Offset, b, in.EOF, s.opts.Now())
	if e != nil {
		failure(w, e)
		return
	}
	respond(w, 200, out)
}
func (s *Server) pull(w http.ResponseWriter, r *http.Request, sess *session) {
	var in struct {
		Protocol int    `json:"protocol"`
		StreamID string `json:"stream_id"`
		Offset   int64  `json:"offset"`
		Max      int    `json:"max_bytes"`
		Wait     int    `json:"wait_ms"`
	}
	if e := decode(r, &in); e != nil {
		failure(w, e)
		return
	}
	if e := protocol(in.Protocol); e != nil {
		failure(w, e)
		return
	}
	if in.Offset < 0 || in.Offset > MaxOffset || in.Max < 1 || in.Max > MaxChunk || in.Wait < 0 || in.Wait > 20000 {
		failure(w, fault(400, "INVALID_REQUEST"))
		return
	}
	st, e := getStream(sess, in.StreamID)
	if e != nil {
		failure(w, e)
		return
	}
	out, e := st.pull(r.Context(), in.Offset, in.Max, time.Duration(in.Wait)*time.Millisecond, s.opts.Now())
	if e != nil {
		failure(w, e)
		return
	}
	respond(w, 200, out)
}
func (s *Server) close(w http.ResponseWriter, r *http.Request, sess *session) {
	var in struct {
		Protocol int    `json:"protocol"`
		StreamID string `json:"stream_id"`
		All      bool   `json:"all"`
	}
	if e := decode(r, &in); e != nil {
		failure(w, e)
		return
	}
	if e := protocol(in.Protocol); e != nil {
		failure(w, e)
		return
	}
	if (in.All && in.StreamID != "") || (!in.All && in.StreamID == "") {
		failure(w, fault(400, "INVALID_REQUEST"))
		return
	}
	if in.All {
		s.expire(sess)
	} else {
		if st, _ := getStream(sess, in.StreamID); st != nil {
			st.close(fault(410, "STREAM_CLOSED"))
		}
	}
	respond(w, 200, map[string]any{"protocol": 1, "closed": true})
}

func Revision(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
