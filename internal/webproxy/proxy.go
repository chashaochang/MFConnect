// Package webproxy is the shared final hop to locally approved applications.
package webproxy

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"mfconnect/internal/core"
)

const SessionCookie = "__Host-mf_session"

func TLS(host, pin string) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if pin != "" {
		// Exact device-approved certificate pin, never an unchecked TLS bypass.
		c.InsecureSkipVerify = true
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("missing upstream certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != strings.ToLower(pin) {
				return errors.New("upstream certificate pin mismatch")
			}
			return nil
		}
	}
	return c
}

func Transport() *http.Transport {
	return &http.Transport{Proxy: nil, DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
}

func Serve(w http.ResponseWriter, r *http.Request, target core.Target, host string, transport *http.Transport) {
	u, err := url.Parse(target.URL)
	if err != nil {
		http.Error(w, "invalid_upstream", 502)
		return
	}
	transport.TLSClientConfig = TLS(u.Hostname(), target.CertificateSHA256)
	external := "https://" + host
	proxy := httputil.ReverseProxy{Transport: transport, FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				pr.Out.Header.Del(name)
			}
			for name := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-mf-") {
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
			if pr.Out.Header.Get("Origin") == external {
				pr.Out.Header.Set("Origin", u.Scheme+"://"+u.Host)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
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
			if loc := resp.Header.Get("Location"); loc != "" {
				if redirect, e := url.Parse(loc); e == nil && redirect.Host == u.Host && (redirect.Scheme == u.Scheme || redirect.Scheme == "") {
					redirect.Scheme = "https"
					redirect.Host = host
					resp.Header.Set("Location", redirect.String())
				}
			}
			resp.Header.Set("Cache-Control", "private, no-store")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { http.Error(w, "upstream_unavailable", 503) },
	}
	proxy.ServeHTTP(w, r)
	transport.CloseIdleConnections()
}
