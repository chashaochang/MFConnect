package webproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebGUIHostCookieAndRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "home.example.test" || r.Header.Get("X-MF-Portal-Proof") != "" || r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Error("unsafe upstream headers")
		}
		if _, err := r.Cookie(SessionCookie); err == nil {
			t.Error("cloud credential leaked")
		}
		if c, err := r.Cookie("unraid_host"); err != nil || c.Value != "native" {
			t.Error("NAS login cookie lost")
		}
		http.SetCookie(w, &http.Cookie{Name: "unraid_host", Value: "fresh", Path: "/", HttpOnly: true, Domain: "localhost"})
		w.Header().Set("Location", "http://home.example.test/Main")
		w.WriteHeader(302)
	}))
	defer upstream.Close()
	r := httptest.NewRequest("GET", "https://home.example.test/", nil)
	r.AddCookie(&http.Cookie{Name: "unraid_host", Value: "native"})
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "private"})
	r.Header.Set("X-MF-Portal-Proof", "private")
	w := httptest.NewRecorder()
	ServeWebGUI(w, r, upstream.URL, "home.example.test", Transport())
	if w.Code != 302 || w.Header().Get("Location") != "https://home.example.test/Main" {
		t.Fatal("redirect not rewritten")
	}
	c := w.Result().Cookies()
	if len(c) != 1 || !c[0].Secure || !c[0].HttpOnly || c[0].Domain != "" {
		t.Fatal("NAS cookie scope changed")
	}
}
