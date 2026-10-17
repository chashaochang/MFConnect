package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/updates"
	_ "modernc.org/sqlite"
)

type Principal struct {
	OwnerID        string `json:"owner_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	Expires        int64  `json:"expires,omitempty"`
	RefreshHash    string `json:"refresh_hash,omitempty"`
	RefreshExpires int64  `json:"refresh_expires,omitempty"`
	ID             string `json:"id"`
	TokenHash      string `json:"token_hash"`
	Admin          bool   `json:"admin"`
}
type Enrollment struct {
	InstallRequest string `json:"install_request,omitempty"`
	Install        bool   `json:"install,omitempty"`
	Owner          string `json:"owner"`
	Expires        int64  `json:"expires"`
}
type Installation struct {
	Owner          string `json:"owner"`
	Expires        int64  `json:"expires"`
	EnrollmentHash string `json:"enrollment_hash"`
	Consumed       bool   `json:"consumed,omitempty"`
}
type Device struct {
	Docker          *core.DockerReport  `json:"docker,omitempty"`
	ConnectID       string              `json:"connect_id,omitempty"`
	ConnectEnabled  bool                `json:"connect_enabled,omitempty"`
	PortalSupported bool                `json:"portal_supported,omitempty"`
	PortalPort      int                 `json:"portal_port,omitempty"`
	Software        *updates.Software   `json:"software,omitempty"`
	Transport       string              `json:"transport,omitempty"`
	EasyTierSecret  string              `json:"easytier_secret,omitempty"`
	EasyTierPort    int                 `json:"easytier_port,omitempty"`
	GatewayPort     int                 `json:"gateway_port,omitempty"`
	GatewayPin      string              `json:"gateway_pin,omitempty"`
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	Owner           string              `json:"owner"`
	PublicKey       string              `json:"public_key"`
	TunnelTokenHash string              `json:"tunnel_token_hash"`
	Revoked         bool                `json:"revoked"`
	LastSeen        int64               `json:"last_seen"`
	Network         *core.DeviceNetwork `json:"network,omitempty"`
}
type Service struct {
	DockerManaged   bool        `json:"docker_managed,omitempty"`
	DockerAvailable bool        `json:"docker_available,omitempty"`
	ID              string      `json:"id"`
	DeviceID        string      `json:"device_id"`
	HostKey         string      `json:"host_key"`
	Target          core.Target `json:"target"`
	RemotePort      int         `json:"remote_port"`
	Published       bool        `json:"published"`
	Revision        int         `json:"revision"`
}
type Ticket struct {
	UserID    string `json:"user_id"`
	ServiceID string `json:"service_id"`
	Revision  int    `json:"revision"`
	Host      string `json:"host"`
	Path      string `json:"path"`
	NonceHash string `json:"nonce_hash"`
	Expires   int64  `json:"expires"`
}
type Session struct {
	Host      string `json:"host,omitempty"`
	UserID    string `json:"user_id"`
	ServiceID string `json:"service_id"`
	Revision  int    `json:"revision"`
	Expires   int64  `json:"expires"`
}
type State struct {
	Installations map[string]Installation `json:"installations"`
	InstallKey    string                  `json:"install_key,omitempty"`
	UpdateJobs    map[string]updates.Job  `json:"update_jobs"`
	Principals    map[string]Principal    `json:"principals"`
	Enrollments   map[string]Enrollment   `json:"enrollments"`
	Devices       map[string]Device       `json:"devices"`
	Services      map[string]Service      `json:"services"`
	Tickets       map[string]Ticket       `json:"tickets"`
	Sessions      map[string]Session      `json:"sessions"`
	Nonces        map[string]int64        `json:"nonces"`
}

func Empty() State {
	return State{Installations: map[string]Installation{}, Principals: map[string]Principal{}, Enrollments: map[string]Enrollment{}, Devices: map[string]Device{}, Services: map[string]Service{}, Tickets: map[string]Ticket{}, Sessions: map[string]Session{}, Nonces: map[string]int64{}, UpdateJobs: map[string]updates.Job{}}
}

type Store struct {
	mu    sync.Mutex
	db    *sql.DB
	state State
}

// A transactional SQLite snapshot is intentionally used for the single-instance
// pilot. State is assigned only after persistence succeeds; ticket consumption,
// nonce checks and port allocation share the same serializable transaction.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), payload BLOB NOT NULL)"} {
		if _, err = db.Exec(stmt); err != nil {
			return fail(err)
		}
	}
	_ = os.Chmod(path, 0600)
	st := &Store{db: db, state: Empty()}
	var b []byte
	err = db.QueryRow("SELECT payload FROM state WHERE id=1").Scan(&b)
	if err == nil {
		if err = json.Unmarshal(b, &st.state); err != nil {
			return fail(err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fail(err)
	}
	return st, nil
}
func (s *Store) Close() error { return s.db.Close() }
func clone(st State) State {
	b, _ := json.Marshal(st)
	cp := Empty()
	if err := json.Unmarshal(b, &cp); err != nil {
		panic(err)
	}
	return cp
}
func (s *Store) View() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b []byte
	err := s.db.QueryRow("SELECT payload FROM state WHERE id=1").Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return Empty()
	}
	if err != nil {
		panic(err)
	}
	st := Empty()
	if err = json.Unmarshal(b, &st); err != nil {
		panic(err)
	}
	return st
}
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	next := Empty()
	var previous []byte
	err = tx.QueryRow("SELECT payload FROM state WHERE id=1").Scan(&previous)
	if err == nil {
		if err = json.Unmarshal(previous, &next); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := fn(&next); err != nil {
		return err
	}
	now := time.Now().Unix()
	for k, v := range next.Tickets {
		if v.Expires <= now {
			delete(next.Tickets, k)
		}
	}
	for k, v := range next.Sessions {
		if v.Expires <= now {
			delete(next.Sessions, k)
		}
	}
	for k, v := range next.Enrollments {
		if v.Expires <= now {
			delete(next.Enrollments, k)
		}
	}
	for k, v := range next.Installations {
		if v.Expires+86400 <= now {
			delete(next.Installations, k)
			if !v.Consumed {
				delete(next.Principals, v.Owner)
			}
		}
	}
	for k, v := range next.Nonces {
		if v <= now {
			delete(next.Nonces, k)
		}
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO state(id,payload) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload", b); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.state = next
	return nil
}
func Authorized(st *State, p Principal, service Service) bool {
	d := st.Devices[service.DeviceID]
	return DeviceAuthorized(st, p, service.DeviceID) && (d.ConnectID == "" || d.ConnectEnabled) && DockerAvailable(d, service)
}

func DockerAvailable(d Device, svc Service) bool {
	return !svc.DockerManaged || (svc.DockerAvailable && time.Now().Unix()-d.LastSeen < 45 && d.Docker.Available(svc.Target.Key))
}

func DeviceAuthorized(st *State, p Principal, id string) bool {
	d, ok := st.Devices[id]
	if !ok || d.Revoked || (p.Expires != 0 && p.Expires <= time.Now().Unix()) {
		return false
	}
	if p.DeviceID != "" {
		return p.DeviceID == id && p.OwnerID == d.Owner
	}
	return p.Admin || d.Owner == p.ID
}
