package webproxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Keep the external Host so Unraid creates its own host-specific session and
// CSRF/login flow. The only upstream is the configured local WebGUI listener.
func ServeWebGUI(w http.ResponseWriter, r *http.Request, target, host string, transport *http.Transport) {
	u, err := url.Parse(target)
	if err != nil {
		http.Error(w, "invalid_webgui", 503)
		return
	}
	transport.TLSClientConfig = TLS(u.Hostname(), "")
	proxy := httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(u)
		pr.Out.Host = host
		for name := range pr.Out.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-mf-") || strings.HasPrefix(strings.ToLower(name), "x-forwarded-") || strings.EqualFold(name, "Forwarded") {
				pr.Out.Header.Del(name)
			}
		}
		pr.Out.Header.Set("X-Forwarded-Host", host)
		pr.Out.Header.Set("X-Forwarded-Proto", "https")
		cookies := pr.Out.Cookies()
		pr.Out.Header.Del("Cookie")
		for _, c := range cookies {
			if !strings.HasPrefix(c.Name, "__Host-mf_") {
				pr.Out.AddCookie(c)
			}
		}
	}, ModifyResponse: func(resp *http.Response) error {
		cookies := resp.Cookies()
		resp.Header.Del("Set-Cookie")
		for _, c := range cookies {
			if strings.HasPrefix(c.Name, "__Host-mf_") {
				continue
			}
			c.Domain = ""
			c.Secure = true
			resp.Header.Add("Set-Cookie", c.String())
		}
		if redirect, e := url.Parse(resp.Header.Get("Location")); e == nil && (redirect.Host == host || redirect.Host == u.Host) {
			redirect.Scheme = "https"
			redirect.Host = host
			resp.Header.Set("Location", redirect.String())
		}
		resp.Header.Set("Cache-Control", "private, no-store")
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { http.Error(w, "webgui_unavailable", 503) }}
	proxy.ServeHTTP(w, r)
	transport.CloseIdleConnections()
}
