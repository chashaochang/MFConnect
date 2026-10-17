package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mfconnect/internal/core"
	"mfconnect/internal/pki"
	"mfconnect/internal/server"
	"mfconnect/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: mfconnect-server init|serve|issue-user|version")
	}
	if os.Args[1] == "version" {
		fmt.Println(core.Version)
		return nil
	}
	if os.Args[1] == "capabilities" {
		fmt.Println(`{"endpoint_contracts":[1,2],"unified_entry":true}`)
		return nil
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	dir := fs.String("dir", "state", "private state directory")
	entry := fs.String("entry", "", "unified HTTPS entry origin")
	host := fs.String("api-host", "localhost", "API host, no port")
	connect := fs.String("connect-host", "", "legacy browser entry alias (optional)")
	domain := fs.String("base-domain", "apps.localhost", "service DNS suffix")
	tunnelHost := fs.String("tunnel-host", "localhost", "hostname verified by frpc")
	transport := fs.String("transport", "frp", "init transport: frp (default) or legacy easytier")
	frpsBinary := fs.String("frps", "dist/tools/frps", "pinned FRP server binary for init")
	easyTierBinary := fs.String("easytier", "dist/tools/easytier-core", "pinned EasyTier binary for init")
	user := fs.String("user", "", "principal to issue (operator provisioning, not RaidMate login)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	configPath := filepath.Join(abs, "server.json")
	switch os.Args[1] {
	case "installation-link":
		var c server.Config
		if err = core.ReadJSON(configPath, &c); err != nil {
			return err
		}
		st, err := store.Open(c.Database)
		if err != nil {
			return err
		}
		defer st.Close()
		owner := *user
		token := core.Random()
		expires := time.Now().Unix() + 1800
		err = st.Update(func(state *store.State) error {
			if owner == "" {
				owner = core.ID("owner")
				state.Principals[owner] = store.Principal{ID: owner, TokenHash: core.Hash(core.Random())}
			}
			if _, ok := state.Principals[owner]; !ok {
				return errors.New("unknown owner")
			}
			state.Enrollments[core.Hash(token)] = store.Enrollment{Owner: owner, Expires: expires, Install: true}
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Printf("%s/v1/install/%s/mfconnect.plg\n", c.APIBaseURL(), token)
		return nil
	case "init":
		if *transport != "easytier" && *transport != "frp" {
			return errors.New("unsupported transport")
		}
		if _, err := os.Stat(configPath); err == nil {
			return errors.New("state already initialized")
		}
		if strings.ContainsAny(*host+*domain+*tunnelHost, "/ :\n") {
			return errors.New("invalid hostname")
		}
		names := []string{*host, "*." + *domain, *tunnelHost, "127.0.0.1"}
		connectHost := *connect
		if *entry != "" {
			h, err := server.EntryHost(*entry)
			if err != nil {
				return err
			}
			explicit := map[string]bool{}
			fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
			if !explicit["api-host"] {
				*host = h
			}
			if !explicit["base-domain"] {
				*domain = h
			}
			if !explicit["tunnel-host"] {
				*tunnelHost = h
			}
			if connectHost == "" {
				connectHost = h
			}
			names = []string{h, "*." + h, *host, *domain, "*." + *domain, *tunnelHost, "127.0.0.1"}
		}
		if connectHost != "" {
			names = append(names, connectHost, "*."+connectHost)
		}
		if err := pki.Development(abs, names); err != nil {
			return err
		}
		c := server.Config{EntryOrigin: strings.TrimRight(*entry, "/"), ConnectHost: connectHost, Listen: "127.0.0.1:8443", HookListen: "127.0.0.1:9091", APIHost: *host, BaseDomain: *domain, Database: filepath.Join(abs, "state.db"), Certificate: filepath.Join(abs, "server.crt"), PrivateKey: filepath.Join(abs, "server.key"), TunnelHost: *tunnelHost, TunnelPort: 17000, ProxyHost: "127.0.0.1", PortStart: 20000, PortEnd: 20999, SessionSeconds: 3600}
		if *transport == "easytier" {
			binary, err := filepath.Abs(*easyTierBinary)
			if err != nil {
				return err
			}
			c.EasyTier = &server.EasyTierConfig{Binary: binary, RunDir: filepath.Join(abs, "run"), ListenHost: "0.0.0.0", PortStart: 21000, PortEnd: 21999}
		} else {
			binary, err := filepath.Abs(*frpsBinary)
			if err != nil {
				return err
			}
			c.FRP = &server.FRPConfig{Binary: binary, RunDir: filepath.Join(abs, "run"), ListenHost: "0.0.0.0"}
		}
		st, err := store.Open(c.Database)
		if err != nil {
			return err
		}
		defer st.Close()
		token := core.Random()
		if err = st.Update(func(state *store.State) error {
			state.Principals["admin"] = store.Principal{ID: "admin", TokenHash: core.Hash(token), Admin: true}
			return nil
		}); err != nil {
			return err
		}
		if err = core.WritePrivate(filepath.Join(abs, "admin-token"), []byte(token+"\n")); err != nil {
			return err
		}
		if err = core.WriteJSON(configPath, c); err != nil {
			return err
		}
		frps := server.FRPServerConfig(c)
		if err = core.WriteJSON(filepath.Join(abs, "frps.json"), frps); err != nil {
			return err
		}
		fmt.Printf("Initialized %s with development TLS. Operator token saved to admin-token (not printed).\n", abs)
		return nil
	case "issue-user":
		if !core.Identifier.MatchString(*user) || *user == "admin" {
			return errors.New("valid non-admin --user required")
		}
		var c server.Config
		if err = core.ReadJSON(configPath, &c); err != nil {
			return err
		}
		st, err := store.Open(c.Database)
		if err != nil {
			return err
		}
		defer st.Close()
		token := core.Random()
		if err = st.Update(func(state *store.State) error {
			if _, exists := state.Principals[*user]; exists {
				return errors.New("principal already exists")
			}
			state.Principals[*user] = store.Principal{ID: *user, TokenHash: core.Hash(token)}
			return nil
		}); err != nil {
			return err
		}
		return core.WritePrivate(filepath.Join(abs, *user+"-token"), []byte(token+"\n"))
	case "serve", "check":
		var c server.Config
		if err = core.ReadJSON(configPath, &c); err != nil {
			return err
		}
		if dir := os.Getenv("MF_UPDATE_DIR"); dir != "" {
			c.Updates = &server.UpdateConfig{ControlDir: dir, Catalog: filepath.Join(dir, "catalog.json"), Owner: os.Getenv("MF_UPDATE_OWNER"), DeviceID: os.Getenv("MF_UPDATE_DEVICE")}
		}
		if entry := os.Getenv("MF_ENTRY_ORIGIN"); entry != "" {
			c.EntryOrigin = entry
		}
		if host := os.Getenv("MF_CONNECT_HOST"); host != "" {
			c.ConnectHost = host
		}
		if mode := os.Getenv("MF_INSTALL_MODE"); mode != "" {
			c.InstallMode = mode
		}
		if limit := os.Getenv("MF_INSTALL_DEVICE_LIMIT"); limit != "" {
			n, e := strconv.Atoi(limit)
			if e != nil || n < 1 || n > 1000 {
				return errors.New("invalid MF_INSTALL_DEVICE_LIMIT")
			}
			c.InstallDeviceLimit = n
		}
		if manifest := os.Getenv("MF_PLUGIN_MANIFEST"); manifest != "" {
			c.PluginManifest = manifest
		}
		if os.Args[1] == "check" {
			if _, err := os.Stat(c.Database); err != nil {
				return err
			}
		}
		st, err := store.Open(c.Database)
		if err != nil {
			return err
		}
		defer st.Close()
		app, err := server.New(c, st)
		if err != nil {
			return err
		}
		c = app.Config
		if err = app.CheckConfig(); err != nil {
			return err
		}
		if os.Args[1] == "check" {
			fmt.Println("Configuration, hostname ownership and TLS coverage verified")
			return nil
		}
		api := &http.Server{Addr: c.Listen, Handler: app, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
		hook := &http.Server{Addr: c.HookListen, Handler: http.HandlerFunc(app.Hook), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		stopEasyTier, err := app.StartEasyTier(ctx)
		if err != nil {
			return err
		}
		defer stopEasyTier()
		stopFRP, err := app.StartFRP(ctx)
		if err != nil {
			return err
		}
		defer stopFRP()
		fail := make(chan error, 2)
		go func() { fail <- api.ListenAndServeTLS(c.Certificate, c.PrivateKey) }()
		go func() { fail <- hook.ListenAndServe() }()
		log.Printf("mfconnect %s API %s; private authorization %s", core.Version, c.Listen, c.HookListen)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case err = <-fail:
				cancel()
			case <-ctx.Done():
			case <-ticker.C:
				app.CloseInvalidConnections()
				continue
			}
			break
		}
		shutCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = api.Shutdown(shutCtx)
		_ = hook.Shutdown(shutCtx)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	default:
		return errors.New("unknown command")
	}
}
