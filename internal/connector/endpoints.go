package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"mfconnect/internal/core"
)

func (c Config) APIBase() string {
	if c.APIBaseURL != "" {
		return c.APIBaseURL
	}
	return strings.TrimRight(c.Server, "/")
}
func validateEndpoints(c Config) error {
	if err := validateServer(c.Server); err != nil {
		return err
	}
	origin := strings.TrimRight(c.Server, "/")
	if c.APIBaseURL != "" && c.APIBaseURL != origin && c.APIBaseURL != origin+"/api" {
		return errors.New("api_base_url must be the configured HTTPS origin or its /api base")
	}
	if c.EntryOrigin != "" && (c.EntryOrigin != origin || c.APIBaseURL != origin+"/api") {
		return errors.New("entry_origin must match the explicitly configured unified API origin")
	}
	return nil
}
func InitEntry(path, entry, name, ca string) error {
	if err := validateServer(entry); err != nil {
		return err
	}
	return initConfig(path, Config{Server: strings.TrimRight(entry, "/"), EntryOrigin: strings.TrimRight(entry, "/"), APIBaseURL: strings.TrimRight(entry, "/") + "/api", Name: name, CAFile: ca})
}

// An endpoint descriptor is obtained over the configured, certificate-verified
// connection. It never changes that connection or forwards credentials elsewhere.
func validateDescriptor(c Config, raw json.RawMessage) error {
	var out struct {
		Contract     int               `json:"endpoint_contract"`
		APIOrigin    string            `json:"api_origin"`
		APIBase      string            `json:"api_base_url"`
		Entry        string            `json:"entry_origin"`
		Canonical    string            `json:"canonical_api_base_url"`
		Browser      string            `json:"browser_domain"`
		Apps         string            `json:"apps_domain"`
		Connect      *core.ConnectInfo `json:"connect"`
		Capabilities struct {
			Unified bool `json:"unified_entry"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Contract != 2 || out.APIOrigin != c.Server || out.APIBase != c.APIBase() {
		return errors.New("controller returned an unconfirmed API endpoint")
	}
	if out.Capabilities.Unified {
		if validateServer(out.Entry) != nil || out.Canonical != out.Entry+"/api" {
			return errors.New("invalid unified endpoint descriptor")
		}
		u, _ := url.Parse(out.Entry)
		if out.Browser != u.Hostname() || out.Apps != u.Hostname() {
			return errors.New("invalid unified domain scope")
		}
	}
	if out.Connect == nil {
		return errors.New("missing NAS endpoint descriptor")
	}
	if out.Connect.ID == "" {
		if out.Connect.Origin != "" || out.Connect.EntryURL != "" {
			return errors.New("unexpected unconfigured NAS address")
		}
	} else if !core.ValidConnectID(out.Connect.ID) || out.Browser == "" || out.Connect.Origin != "https://"+out.Connect.ID+"."+out.Browser || out.Connect.EntryURL != "https://"+out.Browser+"/"+out.Connect.ID {
		return errors.New("NAS addresses do not match the confirmed browser domain")
	}
	return nil
}
func ConnectVersion(ctx context.Context, c Config, input json.RawMessage, contract int) (json.RawMessage, error) {
	if contract != 1 && contract != 2 {
		return nil, errors.New("unsupported endpoint contract")
	}
	var action struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(input, &action) != nil {
		return nil, errors.New("invalid connect request")
	}
	var out json.RawMessage
	err := requestContract(ctx, c, "/v1/device/connect", input, &out, true, contract)
	if err == nil && contract == 2 && (action.Action == "endpoints" || action.Action == "session") {
		err = validateDescriptor(c, out)
	}
	return out, err
}
