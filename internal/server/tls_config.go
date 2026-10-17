package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"
)

func (s *Server) checkUnifiedCertificate() error {
	if s.Config.EntryOrigin == "" {
		return nil
	}
	pair, err := tls.LoadX509KeyPair(s.Config.Certificate, s.Config.PrivateKey)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
		return errors.New("entry TLS certificate is not currently valid")
	}
	for _, host := range []string{s.Config.EntryHost(), s.Config.APIHost, s.Config.ConnectHost, s.Config.TunnelHost} {
		if host != "" {
			if err := cert.VerifyHostname(host); err != nil {
				return fmt.Errorf("TLS does not cover configured hostname %s: %w", host, err)
			}
		}
	}
	for _, domain := range []string{s.Config.EntryHost(), s.Config.BaseDomain, s.Config.ConnectHost} {
		if domain == "" {
			continue
		}
		found := false
		for _, name := range cert.DNSNames {
			if name == "*."+domain {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("TLS certificate must include wildcard *.%s", domain)
		}
	}
	return nil
}
