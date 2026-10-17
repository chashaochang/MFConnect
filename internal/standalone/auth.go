// Package standalone implements the NAS-only, existing-origin TCP transport.
// It never uses cloud credentials or accepts client-selected dial addresses.
package standalone

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"
)

type Credential struct {
	Type   string
	Key    string
	Cookie string
	CSRF   string
}

type Authenticator interface {
	Verify(context.Context, Credential) (string, *Fault)
}

type NASAuthenticator struct{ client *http.Client }

func NewNASAuthenticator(socket string) *NASAuthenticator {
	return &NASAuthenticator{client: &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
	}}
}

func (a *NASAuthenticator) Verify(ctx context.Context, c Credential) (string, *Fault) {
	// me is permission-protected by Unraid. A successful HTTP status alone is
	// deliberately insufficient (GraphQL returns 200 for many auth failures).
	r, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/graphql", bytes.NewBufferString(`{"query":"query MFConnectIdentity { me { id roles } }"}`))
	r.Header.Set("Content-Type", "application/json")
	switch c.Type {
	case "unraid_api_key":
		r.Header.Set("X-API-Key", c.Key)
	case "unraid_webgui_session":
		r.Header.Set("Cookie", c.Cookie)
		r.Header.Set("X-CSRF-Token", c.CSRF)
	default:
		return "", fault(401, "AUTHENTICATION_REQUIRED")
	}
	res, err := a.client.Do(r)
	if err != nil {
		return "", fault(503, "AUTH_BACKEND_UNAVAILABLE")
	}
	defer res.Body.Close()
	if res.StatusCode == 401 {
		return "", fault(401, "AUTHENTICATION_REQUIRED")
	}
	if res.StatusCode == 403 {
		return "", fault(403, "PERMISSION_DENIED")
	}
	if res.StatusCode != 200 {
		return "", fault(503, "AUTH_BACKEND_UNAVAILABLE")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(b) > 65536 {
		return "", fault(503, "AUTH_BACKEND_UNAVAILABLE")
	}
	var response struct {
		Data struct {
			Me *struct {
				ID    string   `json:"id"`
				Roles []string `json:"roles"`
			} `json:"me"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(b, &response) != nil {
		return "", fault(503, "AUTH_BACKEND_UNAVAILABLE")
	}
	if len(response.Errors) > 0 || response.Data.Me == nil || response.Data.Me.ID == "" {
		return "", fault(401, "AUTHENTICATION_REQUIRED")
	}
	for _, role := range response.Data.Me.Roles {
		if role == "ADMIN" {
			return response.Data.Me.ID, nil
		}
	}
	return "", fault(403, "PERMISSION_DENIED")
}
