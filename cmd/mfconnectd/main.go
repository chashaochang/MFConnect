package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mfconnect/internal/connector"
	"mfconnect/internal/core"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: mfconnectd init|enroll|run|check|version")
	}
	if os.Args[1] == "version" {
		fmt.Println(core.Version)
		return nil
	}
	if os.Args[1] == "capabilities" {
		fmt.Println(`{"endpoint_contracts":[1,2],"unified_entry":true,"docker_auto_publish":1,"browser_public_direct":1,"launch_navigation":1,"updates":1,"webdav":false}`)
		return nil
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	config := fs.String("config", "/boot/config/plugins/mfconnect/config.json", "configuration file")
	entry := fs.String("entry", "", "unified HTTPS entry origin (API is /api)")
	contract := fs.Int("endpoint-contract", 1, "local management endpoint contract: 1 or 2")
	api := fs.String("server", "", "HTTPS controller origin")
	name := fs.String("name", "Unraid", "device name")
	ca := fs.String("ca", "", "trusted CA bundle")
	invite := fs.String("enrollment-file", "", "one-use enrollment token file")
	runDir := fs.String("run-dir", "/var/run/mfconnect", "runtime directory independent of array")
	frpc := fs.String("frpc", "/usr/local/lib/mfconnect/current/frpc", "frpc binary")
	easyTier := fs.String("easytier", "/usr/local/lib/mfconnect/current/easytier-core", "pinned EasyTier core binary")
	transport := fs.String("transport", "frp", "init transport: frp (default) or legacy easytier")
	requiredTransport := fs.String("require-transport", "", "check: require this transport before installing a package")
	interval := fs.Duration("interval", 5*time.Second, "controller synchronization interval")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *entry != "" && *api != "" {
		return errors.New("use either --entry or legacy --server")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch os.Args[1] {
	case "discover":
		out, err := connector.InspectDocker(ctx, *config)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	case "provision":
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 129))
		if err != nil {
			return err
		}
		token := strings.TrimSpace(string(b))
		if len(token) < 32 || len(token) > 128 {
			return errors.New("invalid provisioning request")
		}
		if *entry != "" {
			return connector.ProvisionEntry(ctx, *config, *entry, token)
		}
		return connector.Provision(ctx, *config, *api, token)
	case "connect":
		c, err := connector.Load(*config)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
		if err != nil || len(b) > 65536 || !json.Valid(b) {
			return errors.New("invalid connect request")
		}
		out, err := connector.ConnectVersion(ctx, c, json.RawMessage(b), *contract)
		status, code := 200, ""
		if err != nil {
			status, code = 502, "cloud_unreachable"
			var he connector.HTTPError
			if errors.As(err, &he) {
				status, code = he.Status, he.Code
			}
			out = json.RawMessage(`{}`)
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"status": status, "error": code, "data": out})
	case "init":
		if *transport != "easytier" && *transport != "frp" {
			return errors.New("unsupported transport")
		}
		init := connector.Init
		if *entry != "" {
			init = connector.InitEntry
			*api = *entry
		}
		if err := init(*config, *api, *name, *ca); err != nil {
			return err
		}
		c, err := connector.Load(*config)
		if err != nil {
			return err
		}
		c.Transport = *transport
		return core.WriteJSON(*config, c)
	case "enroll":
		b, err := os.ReadFile(*invite)
		if err != nil {
			return err
		}
		return connector.Enroll(ctx, *config, strings.TrimSpace(string(b)))
	case "check":
		c, err := connector.Load(*config)
		if err == nil && *requiredTransport != "" && c.Transport != *requiredTransport {
			return errors.New("configuration transport is not supported by this package; keep the existing installation or migrate the configuration first")
		}
		return err
	case "run":
		return connector.Run(ctx, *config, *runDir, *frpc, *easyTier, *interval)
	default:
		return errors.New("unknown command")
	}
}
