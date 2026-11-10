package core

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// HTTPS: with a domain configured the server listens on :443 (and :80 for
// redirects + ACME HTTP-01 challenges). Certificates come from Let's Encrypt
// on demand - only for the panel domain, db.<domain> and <slug>.<domain> of
// projects that exist - or from cert/key files you provide (e.g. a wildcard
// or a Cloudflare origin certificate).

type certSource struct {
	app   *App
	mode  string
	auto  *autocert.Manager
	mu    sync.RWMutex
	files *tls.Certificate
	mtime time.Time
}

func newCertSource(a *App) *certSource {
	mode := a.Cfg.TLSMode()
	if mode == "proxy" {
		mode = "files" // certificate files for Postgres only
	}
	cs := &certSource{app: a, mode: mode}
	if cs.mode == "auto" {
		cs.auto = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(a.Cfg.Path("certs")),
			Email:      a.Cfg.TLS.Email,
			HostPolicy: a.hostPolicy,
		}
	}
	return cs
}

// hostPolicy limits certificate issuance to names we actually serve, so random
// Host headers can't make us request certificates (and hit rate limits).
func (a *App) hostPolicy(ctx context.Context, host string) error {
	host = strings.ToLower(host)
	d := a.Cfg.Domain
	if host == d || host == "db."+d {
		return nil
	}
	if label, ok := strings.CutSuffix(host, "."+d); ok && !strings.Contains(label, ".") {
		if _, err := a.project(slugFromLabel(label)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("ivarabase: no certificate for %q", host)
}

func (cs *certSource) loadFiles() error {
	c := cs.app.Cfg.TLS
	st, err := os.Stat(c.CertFile)
	if err != nil {
		return err
	}
	cs.mu.RLock()
	same := cs.files != nil && st.ModTime().Equal(cs.mtime)
	cs.mu.RUnlock()
	if same {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return fmt.Errorf("load %s / %s: %w", c.CertFile, c.KeyFile, err)
	}
	cs.mu.Lock()
	cs.files, cs.mtime = &cert, st.ModTime()
	cs.mu.Unlock()
	return nil
}

func (cs *certSource) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if cs.mode == "auto" {
		return cs.auto.GetCertificate(hello)
	}
	if err := cs.loadFiles(); err != nil { // picks up renewed files without a restart
		return nil, err
	}
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.files, nil
}

func (cs *certSource) tlsConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: cs.getCertificate, NextProtos: []string{"h2", "http/1.1"}}
	if cs.auto != nil {
		cfg.NextProtos = append(cfg.NextProtos, "acme-tls/1")
	}
	return cfg
}

// certFor returns the certificate for the panel domain (used for Postgres).
func (cs *certSource) certFor(host string) (*tls.Certificate, error) {
	return cs.getCertificate(&tls.ClientHelloInfo{ServerName: host, SupportedProtos: []string{"http/1.1"},
		CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}})
}

// ---------------------------------------------------------------- serving

// serveTLS runs the HTTPS listener, the :80 redirect/ACME listener and the
// plain loopback listener (for the CLI, MCP and local tools).
func (s *Server) serveTLS(ctx context.Context) error {
	cs := newCertSource(s.app)
	s.certs = cs
	if cs.mode == "files" {
		if err := cs.loadFiles(); err != nil {
			return err
		}
	}
	httpsAddr, httpAddr := s.app.Cfg.httpsAddr(), s.app.Cfg.httpAddr()
	httpsLn, err := net.Listen("tcp", httpsAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w (ports below 1024 need root or CAP_NET_BIND_SERVICE; `ivarabase service install` sets that up)", httpsAddr, err)
	}
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		http.Redirect(w, r, "https://"+host+portSuffix(httpsAddr, "443")+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	var plain http.Handler = redirect
	if cs.auto != nil {
		plain = cs.auto.HTTPHandler(redirect)
	}
	servers := []*http.Server{
		{Handler: s, TLSConfig: cs.tlsConfig(), ReadHeaderTimeout: 15 * time.Second},
		{Addr: httpAddr, Handler: plain, ReadHeaderTimeout: 10 * time.Second},
		{Addr: s.app.Cfg.Listen, Handler: s, ReadHeaderTimeout: 15 * time.Second},
	}
	errc := make(chan error, 3)
	go func() { errc <- servers[0].Serve(tls.NewListener(httpsLn, servers[0].TLSConfig)) }()
	go func() {
		if err := servers[1].ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http listener %s: %v (Let's Encrypt HTTP challenges and redirects need it)", httpAddr, err)
		}
	}()
	go func() {
		if err := servers[2].ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	log.Printf("IvaraBase %s serving https://%s%s (%s certificates), projects at https://<slug>.%s, local http://%s",
		Version, s.app.Cfg.Domain, portSuffix(httpsAddr, "443"), cs.mode, s.app.Cfg.Domain, s.app.Cfg.Listen)
	go s.app.syncPostgresTLS(ctx, cs)
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		srv.Shutdown(sctx)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// projectFromHost maps <slug>.<domain> to a project slug.
func (s *Server) projectFromHost(r *http.Request) (string, bool) {
	d := s.app.Cfg.Domain
	if d == "" {
		return "", false
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, ok := strings.CutSuffix(host, "."+d)
	if !ok || label == "" || label == "db" || strings.Contains(label, ".") {
		return "", false
	}
	return slugFromLabel(label), true
}

// Hostnames can't contain underscores (and Let's Encrypt refuses them), so a
// project my_app is served at my-app.<domain>. Slugs never contain hyphens,
// which keeps the mapping unambiguous.
func hostLabel(slug string) string      { return strings.ReplaceAll(slug, "_", "-") }
func slugFromLabel(label string) string { return strings.ReplaceAll(label, "-", "_") }

// projectURL is the base URL clients (supabase-js) use for a project.
func (a *App) projectURL(slug string) string {
	if a.Cfg.Domain != "" {
		u, err := url.Parse(a.Cfg.PublicURL)
		if err == nil && u.Host != "" {
			port := ""
			if p := u.Port(); p != "" {
				port = ":" + p
			}
			return u.Scheme + "://" + hostLabel(slug) + "." + a.Cfg.Domain + port
		}
	}
	return a.Cfg.PublicURL + "/proj/" + slug
}

// ---------------------------------------------------------------- Postgres TLS

func (p *Postgres) tlsFiles() (string, string) {
	return filepath.Join(p.dataDir(), "server.crt"), filepath.Join(p.dataDir(), "server.key")
}

func (p *Postgres) tlsReady() bool {
	if p.External {
		return false
	}
	c, k := p.tlsFiles()
	return fileExists(c) && fileExists(k)
}

// syncPostgresTLS copies the domain's certificate into the cluster so direct
// database connections are encrypted (sslmode=require / verify-full against
// the domain), and refreshes it when the certificate renews.
func (a *App) syncPostgresTLS(ctx context.Context, cs *certSource) {
	if a.PG.External {
		return
	}
	for {
		if err := a.writePostgresCert(cs); err != nil {
			log.Printf("postgres TLS: %v (retrying in 10 minutes)", err)
		}
		wait := 12 * time.Hour
		if cs.mode == "files" {
			wait = time.Hour // cheap stat; picks up a proxy's renewed certificate soon
		}
		if !a.PG.tlsReady() {
			wait = 10 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// InContainer reports whether IvaraBase runs inside a container image.
func InContainer() bool {
	return os.Getenv("IVARA_IN_CONTAINER") == "1" || fileExists("/.dockerenv") || fileExists("/run/.containerenv")
}

// ensureSelfSignedPostgresTLS gives Postgres a certificate when it accepts
// remote connections but no certificate files are configured (platforms
// with Traefik keep theirs inside the proxy). sslmode=require then still encrypts;
// verify-full needs the real certificate (tls.cert_file / IVARA_TLS_CERT_FILE).
func (a *App) ensureSelfSignedPostgresTLS() error {
	if a.PG.External || a.PG.tlsReady() {
		return nil
	}
	l := a.Cfg.Postgres.Listen
	if l == "127.0.0.1" || l == "localhost" || l == "::1" {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	name := a.Cfg.Domain
	if name == "" {
		name = "ivarabase"
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name, Organization: []string{"IvaraBase self-signed"}},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kd, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	crt, kf := a.PG.tlsFiles()
	if err := writeFileAtomic(kf, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return err
	}
	if err := a.PG.writeConf(); err != nil {
		return err
	}
	if err := a.PG.cmd("pg_ctl", "reload", "-D", a.PG.dataDir()).Run(); err != nil {
		return fmt.Errorf("reload postgres: %w", err)
	}
	log.Printf("postgres TLS: no certificate files configured, using a self-signed certificate (sslmode=require works; set tls.cert_file/key_file for verify-full)")
	return nil
}

func (a *App) writePostgresCert(cs *certSource) error {
	cert, err := cs.certFor(a.Cfg.Domain)
	if err != nil {
		return err
	}
	var chain bytes.Buffer
	for _, der := range cert.Certificate {
		pem.Encode(&chain, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	crt, key := a.PG.tlsFiles()
	if old, err := os.ReadFile(crt); err == nil && bytes.Equal(old, chain.Bytes()) {
		return nil // unchanged
	}
	if err := writeFileAtomic(key, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(crt, chain.Bytes(), 0o600); err != nil {
		return err
	}
	if err := a.PG.writeConf(); err != nil {
		return err
	}
	if err := a.PG.cmd("pg_ctl", "reload", "-D", a.PG.dataDir()).Run(); err != nil {
		return fmt.Errorf("reload postgres: %w", err)
	}
	log.Printf("postgres TLS certificate installed for %s", a.Cfg.Domain)
	return nil
}
