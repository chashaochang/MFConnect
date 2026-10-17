package updates

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Protocol = 1

var versionPattern = regexp.MustCompile(`^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}$`)
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)
var JobIDPattern = regexp.MustCompile(`^upd_[a-f0-9]{32}$`)

type Release struct {
	ID           string `json:"id"`
	Component    string `json:"component"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Protocol     int    `json:"protocol"`
	URL          string `json:"url"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Image        string `json:"image,omitempty"`
	ImageID      string `json:"image_id,omitempty"`
	Notes        string `json:"notes"`
	IssuedAt     int64  `json:"issued_at"`
	ExpiresAt    int64  `json:"expires_at"`
}
type SignedRelease struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}
type Catalog struct {
	Releases []SignedRelease `json:"releases"`
}
type Software struct {
	Connector      string `json:"connector"`
	Plugin         string `json:"plugin"`
	UpdateProtocol int    `json:"update_protocol"`
}
type Job struct {
	ID              string        `json:"id"`
	Component       string        `json:"component"`
	DeviceID        string        `json:"device_id,omitempty"`
	RequestedBy     string        `json:"requested_by"`
	RequestID       string        `json:"request_id"`
	ReleaseID       string        `json:"release_id"`
	Version         string        `json:"version"`
	PreviousVersion string        `json:"previous_version"`
	State           string        `json:"state"`
	Error           string        `json:"error,omitempty"`
	CreatedAt       int64         `json:"created_at"`
	UpdatedAt       int64         `json:"updated_at"`
	ExpiresAt       int64         `json:"expires_at"`
	Release         SignedRelease `json:"release"`
}
type Report struct {
	JobID            string `json:"job_id"`
	State            string `json:"state"`
	Error            string `json:"error,omitempty"`
	InstalledVersion string `json:"installed_version,omitempty"`
	UpdatedAt        int64  `json:"updated_at"`
}
type Heartbeat struct {
	Protocol  int   `json:"protocol"`
	UpdatedAt int64 `json:"updated_at"`
}

func ValidVersion(v string) bool { return versionPattern.MatchString(v) }
func Compare(a, b string) int {
	if !ValidVersion(a) || !ValidVersion(b) {
		return 0
	}
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range aa {
		x, _ := strconv.Atoi(aa[i])
		y, _ := strconv.Atoi(bb[i])
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}
func Terminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "rolled_back"
}
func ValidReport(r Report) bool {
	switch r.State {
	case "queued", "downloading", "installing", "verifying", "rolling_back", "succeeded", "failed", "rolled_back":
	default:
		return false
	}
	return JobIDPattern.MatchString(r.JobID) && len(r.Error) <= 80 && (r.Error == "" || idPattern.MatchString(r.Error)) && (r.InstalledVersion == "" || ValidVersion(r.InstalledVersion))
}
func (s SignedRelease) Verify(key string, now time.Time) (Release, error) {
	var r Release
	pub, e := base64.StdEncoding.DecodeString(key)
	if e != nil || len(pub) != ed25519.PublicKeySize {
		return r, errors.New("invalid_release_key")
	}
	payload, e := base64.StdEncoding.DecodeString(s.Payload)
	if e != nil || len(payload) > 16384 {
		return r, errors.New("invalid_release")
	}
	sig, e := base64.StdEncoding.DecodeString(s.Signature)
	if e != nil || !ed25519.Verify(pub, payload, sig) {
		return r, errors.New("invalid_release_signature")
	}
	if json.Unmarshal(payload, &r) != nil {
		return r, errors.New("invalid_release")
	}
	u, e := url.Parse(r.URL)
	hash, he := hex.DecodeString(r.SHA256)
	if !idPattern.MatchString(r.ID) || !ValidVersion(r.Version) || r.Architecture != "linux/amd64" || r.Protocol != Protocol || e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || he != nil || len(hash) != 32 || strings.ToLower(r.SHA256) != r.SHA256 || r.Size < 1 || r.Size > 1<<30 || len(r.Notes) > 4000 || r.IssuedAt > now.Unix()+300 || r.ExpiresAt <= now.Unix() {
		return r, errors.New("invalid_or_expired_release")
	}
	switch r.Component {
	case "plugin":
		if !strings.HasSuffix(u.Path, ".plg") || r.Size > 1<<20 {
			return r, errors.New("invalid_plugin_release")
		}
	case "server":
		if r.Image != "mfconnect-server:"+r.Version+"-docker.1" || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(r.ImageID) {
			return r, errors.New("invalid_server_release")
		}
	default:
		return r, errors.New("invalid_component")
	}
	return r, nil
}
func Read(path string, out any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || info.Size() > 1<<20 {
		return errors.New("file_too_large")
	}
	return json.NewDecoder(f).Decode(out)
}
func Write(path string, value any) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".update-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(b, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	if e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func Latest(path, component string) (SignedRelease, Release, error) {
	return LatestWithKey(path, component, PublicKey)
}
func LatestWithKey(path, component, key string) (SignedRelease, Release, error) {
	var c Catalog
	if e := Read(path, &c); e != nil {
		return SignedRelease{}, Release{}, errors.New("release_catalog_unavailable")
	}
	if len(c.Releases) > 64 {
		return SignedRelease{}, Release{}, errors.New("release_catalog_invalid")
	}
	var signed SignedRelease
	var best Release
	for _, entry := range c.Releases {
		r, e := entry.Verify(key, time.Now())
		if e != nil {
			continue
		}
		if r.Component == component && (best.ID == "" || Compare(r.Version, best.Version) > 0) {
			signed, best = entry, r
		}
	}
	if best.ID == "" {
		return signed, best, errors.New("no_verified_release")
	}
	return signed, best, nil
}
