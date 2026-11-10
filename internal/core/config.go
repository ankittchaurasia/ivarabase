package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Version is overridden at build time with -ldflags "-X ivarabase/internal/core.Version=...".
var Version = "0.2.0-dev"

// RepoURL is the project's home: docs, releases, issues.
const RepoURL = "https://github.com/ankittchaurasia/ivarabase"

// Config is persisted as <data>/config.json. Everything has a working default
// so a fresh `ivarabase serve` on a laptop needs no flags.
type Config struct {
	DataDir   string `json:"-"`
	Listen    string `json:"listen"`     // HTTP listen address for API + UI + MCP
	PublicURL string `json:"public_url"` // how clients reach the server (shown in UI / MCP snippets)

	// Domain turns on name-based hosting: the panel at https://<domain> and
	// each project at https://<slug>.<domain> (DNS: domain + *.domain -> server).
	Domain string    `json:"domain"`
	TLS    TLSConfig `json:"tls"`

	// TrustedProxies are the addresses (IPs or CIDRs) whose X-Forwarded-For /
	// X-Real-IP / X-Forwarded-Proto headers we believe. Empty = loopback only,
	// which is right for nginx/Caddy on the same machine.
	TrustedProxies []string     `json:"trusted_proxies,omitempty"`
	trusted        []*net.IPNet // parsed TrustedProxies

	Postgres      PostgresConfig `json:"postgres"`
	DenoPath      string         `json:"deno_path"`      // empty = auto-detect
	PostgrestPath string         `json:"postgrest_path"` // empty = auto-detect

	// WebhooksAllowPrivate lets database webhooks call private, loopback and
	// link-local addresses (refused by default: SSRF). Admin-only, per server.
	WebhooksAllowPrivate bool `json:"webhooks_allow_private_targets,omitempty"`

	envSet     []envSetting // IVARA_* variables applied on load (never saved)
	fileValues *Config      // config.json values before the environment was applied
	loadedURL  string       // PublicURL as derived on load
}

type TLSConfig struct {
	Mode      string `json:"mode"`      // "" = auto when a domain is set | "auto" (Let's Encrypt) | "files" | "proxy" (HTTPS ends at nginx/Caddy in front) | "off"
	Email     string `json:"email"`     // Let's Encrypt account contact
	CertFile  string `json:"cert_file"` // bring your own certificate (PEM, may be a wildcard); in proxy mode used for Postgres only
	KeyFile   string `json:"key_file"`
	HTTPSAddr string `json:"https_addr"` // default :443
	HTTPAddr  string `json:"http_addr"`  // default :80 (redirects + ACME challenges)

	// When the certificate lives where the service user can't read it (hosting
	// keeps them root-only), `service install` copies it into the data
	// directory and a root timer (ivarabase-tls-sync) keeps the copy current.
	// cert_file/key_file then point at the copies; these are the originals.
	SyncCertFrom string `json:"sync_cert_from,omitempty"`
	SyncKeyFrom  string `json:"sync_key_from,omitempty"`
}

// TLSMode is the effective mode: off, auto, files or proxy.
func (c *Config) TLSMode() string {
	if c.Domain == "" || c.TLS.Mode == "off" {
		return "off"
	}
	if c.TLS.Mode == "proxy" {
		return "proxy"
	}
	if c.TLS.CertFile != "" && c.TLS.KeyFile != "" {
		return "files"
	}
	if c.TLS.Mode == "files" {
		return "off" // files mode without files: refuse to guess
	}
	return "auto"
}

// ServesTLS reports whether IvaraBase itself listens for HTTPS (auto/files).
func (c *Config) ServesTLS() bool {
	m := c.TLSMode()
	return m == "auto" || m == "files"
}

// hasCertFiles: a certificate pair is configured (files mode, or Postgres TLS
// behind a proxy).
func (c *Config) hasCertFiles() bool { return c.TLS.CertFile != "" && c.TLS.KeyFile != "" }

// parseTrusted validates TrustedProxies; an empty list means loopback.
func parseTrusted(list []string) ([]*net.IPNet, error) {
	if len(list) == 0 {
		list = []string{"127.0.0.0/8", "::1/128"}
	}
	var out []*net.IPNet
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil {
				if ip.To4() != nil {
					s += "/32"
				} else {
					s += "/128"
				}
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %q is not an IP or CIDR", s)
		}
		out = append(out, n)
	}
	return out, nil
}

// isTrustedProxy reports whether ip may set forwarding headers.
func (c *Config) isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	nets := c.trusted
	if nets == nil { // config not built by LoadConfig (tests)
		nets, _ = parseTrusted(c.TrustedProxies)
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *Config) httpsAddr() string {
	if c.TLS.HTTPSAddr != "" {
		return c.TLS.HTTPSAddr
	}
	return ":443"
}

func (c *Config) httpAddr() string {
	if c.TLS.HTTPAddr != "" {
		return c.TLS.HTTPAddr
	}
	return ":80"
}

// portSuffix is ":8443" for non-default ports, "" for :443/:80.
func portSuffix(addr, def string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if p := addr[i+1:]; p != "" && p != def {
			return ":" + p
		}
	}
	return ""
}

// derivePublicURL makes the public URL follow the domain when HTTPS is on.
// Behind a proxy it is https://<domain>, unless public_url already names the
// domain over https (e.g. a proxy on a non-standard port).
func (c *Config) derivePublicURL() {
	switch {
	case c.TLSMode() == "proxy":
		if u, err := url.Parse(c.PublicURL); err != nil || u.Scheme != "https" || u.Hostname() != c.Domain || u.Path != "" && u.Path != "/" {
			c.PublicURL = "https://" + c.Domain
		}
	case c.Domain != "" && c.ServesTLS():
		c.PublicURL = "https://" + c.Domain + portSuffix(c.httpsAddr(), "443")
	case c.PublicURL == "" && c.Domain != "":
		c.PublicURL = "http://" + c.Domain
	case c.PublicURL == "":
		c.PublicURL = "http://" + c.Listen
		if host, port, err := net.SplitHostPort(c.Listen); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
			c.PublicURL = "http://127.0.0.1:" + port // listening everywhere (containers): show a reachable address
		}
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
}

type PostgresConfig struct {
	BinDir      string `json:"bin_dir"`          // empty = auto-detect newest local install
	Port        int    `json:"port"`             // managed cluster port
	Listen      string `json:"listen_addresses"` // managed cluster listen_addresses ("127.0.0.1" or "*")
	PublicHost  string `json:"public_host"`      // host printed in project connection strings
	ExternalDSN string `json:"external_dsn"`     // use an existing cluster (superuser DSN) instead of managing one

	// Settings override the automatic tuning (shared_buffers, work_mem, ...) and
	// can set any other postgresql.conf parameter. IVARA_PG_* variables win.
	Settings map[string]string `json:"settings,omitempty"`
}

func DefaultDataDir() string {
	if v := os.Getenv("IVARA_DATA"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/var/lib/ivarabase"
	}
	return filepath.Join(home, ".ivarabase")
}

func defaultConfig(dataDir string) *Config {
	return &Config{
		DataDir:   dataDir,
		Listen:    "127.0.0.1:8787",
		PublicURL: "http://127.0.0.1:8787",
		Postgres: PostgresConfig{
			Port:       54329,
			Listen:     "127.0.0.1",
			PublicHost: "127.0.0.1",
		},
	}
}

// Paths inside the data directory.
func (c *Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.DataDir}, parts...)...)
}

func (c *Config) configFile() string { return c.Path("config.json") }

// LoadConfig reads <data>/config.json, falling back to defaults when absent.
func LoadConfig(dataDir string) (*Config, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	cfg := defaultConfig(abs)
	cfg.PublicURL = "" // derived from listen unless config.json or IVARA_PUBLIC_URL sets it
	b, err := os.ReadFile(cfg.configFile())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", cfg.configFile(), err)
		}
	}
	cfg.DataDir = abs
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if cfg.Postgres.Port == 0 {
		cfg.Postgres.Port = 54329
	}
	if cfg.Postgres.Listen == "" {
		cfg.Postgres.Listen = "127.0.0.1"
	}
	if cfg.Postgres.PublicHost == "" {
		cfg.Postgres.PublicHost = "127.0.0.1"
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8787"
	}
	cfg.Domain = strings.ToLower(strings.TrimSpace(cfg.Domain))
	if cfg.trusted, err = parseTrusted(cfg.TrustedProxies); err != nil {
		return nil, err
	}
	cfg.derivePublicURL()
	cfg.loadedURL = cfg.PublicURL
	return cfg, nil
}

func (c *Config) Save() error {
	b, err := json.MarshalIndent(c.forSave(), "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.configFile(), append(b, '\n'), 0o600)
}

func (c *Config) ensureDirs() error {
	for _, d := range []string{"", "secrets", "run", "logs", "functions", "backups", "tmp"} {
		mode := os.FileMode(0o755)
		if d == "secrets" || d == "backups" || d == "tmp" || d == "" {
			mode = 0o700
		}
		if err := os.MkdirAll(c.Path(d), mode); err != nil {
			return err
		}
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
