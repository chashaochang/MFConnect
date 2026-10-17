package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/store"
)

func installFixture(t *testing.T) *fixture {
	f := connectFixture(t)
	f.app.Config.InstallMode = "self_service"
	f.app.Config.PluginManifest = filepath.Join(t.TempDir(), "mfconnect.plg")
	os.WriteFile(f.app.Config.PluginManifest, []byte(`<PLUGIN name="mfconnect"></PLUGIN>`), 0600)
	return f
}
func installEnrollment(token string) map[string]string {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	name := "test NAS"
	return map[string]string{"enrollment_token": token, "name": name, "public_key": base64.RawStdEncoding.EncodeToString(pub), "proof": base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, []byte("mfconnect-enroll\n"+token+"\n"+name)))}
}
func TestSelfInstallationScopeIdempotencyAndConsumption(t *testing.T) {
	f := installFixture(t)
	body := map[string]string{"request_id": core.RandomHex()}
	if invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", map[string]string{"request_id": body["request_id"], "owner": "alice"}).Code != 400 {
		t.Fatal("caller controlled owner")
	}
	w := invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", body)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out struct {
		URL     string `json:"install_url"`
		Expires int64  `json:"expires_at"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	u, _ := url.Parse(out.URL)
	enrollment := strings.Split(u.Path, "/")[3]
	if out.Expires > time.Now().Unix()+1800 {
		t.Fatal("unbounded expiry")
	}
	retry := invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", body)
	var again struct {
		URL      string `json:"install_url"`
		Replayed bool   `json:"replayed"`
	}
	json.Unmarshal(retry.Body.Bytes(), &again)
	if retry.Code != 200 || again.URL != out.URL || !again.Replayed {
		t.Fatal("unsafe claim retry")
	}
	for _, token := range []string{enrollment, body["request_id"]} {
		for _, path := range []string{"/v1/devices", "/v1/services", "/v1/updates/server"} {
			if invoke(f.app, "GET", "api.example.test", path, token, nil).Code != 401 {
				t.Fatal("installation credential became API principal")
			}
		}
	}
	if invoke(f.app, "GET", "api.example.test", u.Path, "", nil).Code != 200 {
		t.Fatal("manifest unavailable")
	}
	enroll := installEnrollment(enrollment)
	w = invoke(f.app, "POST", "api.example.test", "/v1/devices/enroll", "", enroll)
	if w.Code != 201 {
		t.Fatal("cannot enroll", w.Body.String())
	}
	var device struct {
		ID string `json:"device_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &device)
	st := f.st.View()
	owner := st.Devices[device.ID].Owner
	if owner == "alice" || owner == "bob" || st.Principals[owner].Admin || store.DeviceAuthorized(&st, st.Principals[owner], "dev_one") {
		t.Fatal("self install gained existing NAS authority")
	}
	if invoke(f.app, "GET", "api.example.test", u.Path, "", nil).Code != 404 || invoke(f.app, "POST", "api.example.test", "/v1/devices/enroll", "", enroll).Code != 401 || invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", body).Code != 409 {
		t.Fatal("consumed installation reused")
	}
}

func TestSelfInstallationAtomicRetriesAndLimits(t *testing.T) {
	f := installFixture(t)
	body := map[string]string{"request_id": core.RandomHex()}
	var success atomic.Int32
	var retries atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", body)
			if w.Code == 201 {
				success.Add(1)
			} else if w.Code == 200 {
				retries.Add(1)
			} else {
				t.Errorf("unexpected claim: %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 || retries.Load() != 7 || len(f.st.View().Enrollments) != 1 {
		t.Fatal("multiple installers minted")
	}
	f.st.Update(func(st *store.State) error {
		i := st.Installations[core.Hash(body["request_id"])]
		i.Expires = 1
		st.Installations[core.Hash(body["request_id"])] = i
		return nil
	})
	// Keep the expired tombstone within the retry retention period.
	f.st.Update(func(st *store.State) error {
		st.Installations[core.Hash(body["request_id"])] = store.Installation{Expires: time.Now().Unix() - 1}
		return nil
	})
	if invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", body).Code != 410 {
		t.Fatal("expired request minted another installer")
	}
	f.app.Config.InstallDeviceLimit = 2
	if invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", map[string]string{"request_id": core.RandomHex()}).Code != 409 {
		t.Fatal("device limit ignored")
	}
	f.app.Config.InstallMode = "disabled"
	if invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", body).Code != 403 {
		t.Fatal("disabled self installation allowed")
	}
	if invoke(f.app, "GET", "api.example.test", "/v1/installation-policy", "", nil).Code != 200 {
		t.Fatal("public policy unavailable")
	}
}

func TestSelfInstallationDeviceLimitAtConcurrentEnrollment(t *testing.T) {
	f := installFixture(t)
	f.app.Config.InstallDeviceLimit = 3
	tokens := []string{}
	for i := 0; i < 2; i++ {
		w := invoke(f.app, "POST", "api.example.test", "/v1/installations/claim", "", map[string]string{"request_id": core.RandomHex()})
		var out struct {
			URL string `json:"install_url"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		u, _ := url.Parse(out.URL)
		if w.Code != 201 {
			t.Fatal("claim failed")
		}
		tokens = append(tokens, strings.Split(u.Path, "/")[3])
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for _, token := range tokens {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			w := invoke(f.app, "POST", "api.example.test", "/v1/devices/enroll", "", installEnrollment(token))
			if w.Code == 201 {
				accepted.Add(1)
			} else if w.Code != 409 {
				t.Errorf("enroll: %d", w.Code)
			}
		}(token)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal("concurrent registrations exceeded limit")
	}
}
