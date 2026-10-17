package standalone

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"mfconnect/internal/core"
)

func Run(ctx context.Context, configDir, runDir string) error {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	var identity struct {
		NASID string `json:"nas_id"`
	}
	path := filepath.Join(configDir, "standalone-identity.json")
	err := core.ReadJSON(path, &identity)
	if os.IsNotExist(err) {
		identity.NASID = core.ID("nas")
		err = core.WriteJSON(path, identity)
	}
	if err != nil {
		return err
	}
	if !core.Identifier.MatchString(identity.NASID) {
		return errors.New("invalid standalone identity")
	}
	c := &DockerCatalog{CachePath: filepath.Join(configDir, "standalone-docker.json"), DockerSocket: "/var/run/docker.sock", TemplateDir: "/boot/config/plugins/dockerMan/templates-user", IdentPath: "/boot/config/ident.cfg"}
	s := New(Options{NASID: identity.NASID, Auth: NewNASAuthenticator("/var/run/unraid-api.sock"), Catalog: c})
	socket := filepath.Join(runDir, "standalone.sock")
	if st, e := os.Lstat(socket); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return errors.New("standalone socket path is not a socket")
		}
		if conn, e := net.DialTimeout("unix", socket, time.Second); e == nil {
			conn.Close()
			return errors.New("standalone already running")
		}
		if e := os.Remove(socket); e != nil {
			return e
		}
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	go c.Run(ctx)
	go s.Run(ctx)
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	go func() { <-ctx.Done(); _ = server.Close() }()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
