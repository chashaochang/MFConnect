package standalone

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"sync"
	"time"
)

type pushReceipt struct {
	Protocol int   `json:"protocol"`
	Next     int64 `json:"next_offset"`
	EOF      bool  `json:"eof"`
}
type pullReceipt struct {
	Protocol int    `json:"protocol"`
	Offset   int64  `json:"offset"`
	Next     int64  `json:"next_offset"`
	Data     string `json:"data"`
	EOF      bool   `json:"eof"`
}

type stream struct {
	mu                        sync.Mutex
	id, application, revision string
	conn                      net.Conn
	in, out                   []byte
	sent, received            int64
	sendEOF, receiveEOF       bool
	closed                    *Fault
	lastActivity              time.Time
	notify                    chan struct{}
	done                      chan struct{}
	pushOffset                int64
	pushHash                  [32]byte
	pushEOF                   bool
	pushResult                *pushReceipt
	pullResult                *pullReceipt
	pulling                   bool
}

func newStream(id string, a Application, conn net.Conn, now time.Time) *stream {
	s := &stream{id: id, application: a.ID, revision: a.Revision, conn: conn, lastActivity: now, notify: make(chan struct{}), done: make(chan struct{})}
	go s.read()
	go s.write()
	return s
}

// All callers hold mu. A generation channel broadcasts to writer, reader and
// long-poll waiters; notifications are never consumed by the wrong waiter.
func (s *stream) changed() { close(s.notify); s.notify = make(chan struct{}) }
func (s *stream) close(e *Fault) {
	s.mu.Lock()
	if s.closed == nil {
		s.closed = e
		close(s.done)
		s.changed()
		_ = s.conn.Close()
		s.in = nil
		s.out = nil
		s.pullResult = nil
	}
	s.mu.Unlock()
}
func (s *stream) read() {
	for {
		s.mu.Lock()
		if s.closed != nil {
			s.mu.Unlock()
			return
		}
		space := BufferSize - len(s.out)
		if space == 0 {
			wait := s.notify
			s.mu.Unlock()
			<-wait
			continue
		}
		if space > 32768 {
			space = 32768
		}
		s.mu.Unlock()
		buf := make([]byte, space)
		n, err := s.conn.Read(buf)
		s.mu.Lock()
		if s.closed != nil {
			s.mu.Unlock()
			return
		}
		if n > 0 {
			s.out = append(s.out, buf[:n]...)
		}
		if err == io.EOF {
			s.receiveEOF = true
		}
		s.changed()
		s.mu.Unlock()
		if err != nil {
			if err != io.EOF {
				s.close(fault(410, "UPSTREAM_IO_ERROR"))
			}
			return
		}
	}
}
func (s *stream) write() {
	for {
		s.mu.Lock()
		if s.closed != nil {
			s.mu.Unlock()
			return
		}
		if len(s.in) == 0 {
			if s.sendEOF {
				s.mu.Unlock()
				if c, ok := s.conn.(interface{ CloseWrite() error }); ok {
					if c.CloseWrite() != nil {
						s.close(fault(410, "UPSTREAM_IO_ERROR"))
					}
				} else {
					s.close(fault(410, "UPSTREAM_IO_ERROR"))
				}
				return
			}
			wait := s.notify
			s.mu.Unlock()
			<-wait
			continue
		}
		n := len(s.in)
		if n > MaxChunk {
			n = MaxChunk
		}
		buf := append([]byte(nil), s.in[:n]...)
		s.in = s.in[n:]
		s.changed()
		s.mu.Unlock()
		for len(buf) > 0 {
			n, err := s.conn.Write(buf)
			if err != nil || n == 0 {
				s.close(fault(410, "UPSTREAM_IO_ERROR"))
				return
			}
			buf = buf[n:]
		}
	}
}
func (s *stream) push(offset int64, data []byte, eof bool, now time.Time) (*pushReceipt, *Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed != nil {
		return nil, s.closed
	}
	h := sha256.Sum256(data)
	if s.pushResult != nil && offset == s.pushOffset && h == s.pushHash && eof == s.pushEOF {
		s.lastActivity = now
		r := *s.pushResult
		return &r, nil
	}
	if offset != s.sent {
		return nil, offsetFault(s.sent)
	}
	if s.sendEOF {
		return nil, fault(410, "STREAM_CLOSED")
	}
	if offset > MaxOffset-int64(len(data)) {
		return nil, fault(400, "INVALID_REQUEST")
	}
	if len(s.in)+len(data) > BufferSize {
		e := fault(429, "BACKPRESSURE")
		e.RetryAfter = 100
		return nil, e
	}
	s.in = append(s.in, data...)
	s.sent += int64(len(data))
	s.sendEOF = eof
	s.lastActivity = now
	r := pushReceipt{1, s.sent, eof}
	s.pushResult = &r
	s.pushOffset = offset
	s.pushHash = h
	s.pushEOF = eof
	s.changed()
	return &r, nil
}
func (s *stream) pull(ctx context.Context, offset int64, max int, wait time.Duration, now time.Time) (*pullReceipt, *Fault) {
	s.mu.Lock()
	if s.pulling {
		s.mu.Unlock()
		return nil, fault(409, "STREAM_BUSY")
	}
	s.pulling = true
	defer func() { s.mu.Lock(); s.pulling = false; s.mu.Unlock() }()
	s.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		if s.closed != nil {
			e := s.closed
			s.mu.Unlock()
			return nil, e
		}
		if s.pullResult != nil && offset == s.pullResult.Offset {
			r := *s.pullResult
			s.lastActivity = now
			s.mu.Unlock()
			return &r, nil
		}
		if offset != s.received {
			e := offsetFault(s.received)
			s.mu.Unlock()
			return nil, e
		}
		s.pullResult = nil
		s.lastActivity = now
		if len(s.out) > 0 || s.receiveEOF {
			n := len(s.out)
			if n > max {
				n = max
			}
			r := pullReceipt{1, offset, offset + int64(n), base64.StdEncoding.EncodeToString(s.out[:n]), s.receiveEOF && n == len(s.out)}
			s.out = s.out[n:]
			s.received = r.Next
			s.pullResult = &r
			s.changed()
			s.mu.Unlock()
			return &r, nil
		}
		notify := s.notify
		s.mu.Unlock()
		select {
		case <-notify:
		case <-timer.C:
			return &pullReceipt{Protocol: 1, Offset: offset, Next: offset}, nil
		case <-ctx.Done():
			return nil, fault(409, "STREAM_BUSY")
		}
	}
}
