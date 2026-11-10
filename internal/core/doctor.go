package core

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DoctorCheck is one line of `ivarabase doctor`.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | error
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Doctor checks a machine before (or while) running IvaraBase on it. It does
// not start anything.
func Doctor(dataDir string) ([]DoctorCheck, *Config) {
	var out []DoctorCheck
	add := func(name, status, detail, fix string) {
		out = append(out, DoctorCheck{name, status, detail, fix})
	}
	cfg, err := LoadConfig(dataDir)
	if err != nil {
		add("config", "error", err.Error(), "fix or remove "+filepath.Join(dataDir, "config.json"))
		return out, nil
	}
	add("data dir", "ok", cfg.DataDir, "")
	if os.Geteuid() == 0 && runtime.GOOS == "linux" {
		add("user", "warn", "running as root: PostgreSQL refuses to run as root", "use `sudo ivarabase service install` (runs as the ivarabase user) or run as a normal user")
	}

	pgInstall := "brew install postgresql@18"
	if runtime.GOOS == "linux" {
		pgInstall = "run scripts/install-deps-ubuntu.sh (installs postgresql-18 from apt.postgresql.org)"
	}
	want := 0
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "pg", "PG_VERSION")); err == nil {
		want, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if dir, _, ver, err := findPgBin(cfg.Postgres.BinDir, want, cfg.Postgres.ExternalDSN == ""); err != nil {
		add("postgres", "error", err.Error(), pgInstall)
	} else {
		add("postgres", "ok", "PostgreSQL "+ver+" in "+dir, "")
	}
	rr := &RestRuntime{app: &App{Cfg: cfg}}
	if b := rr.binary(); b == "" {
		add("postgrest", "warn", "not found - project REST APIs (/rest/v1) won't work", "macOS: brew install postgrest; Linux: scripts/install-deps-ubuntu.sh")
	} else {
		add("postgrest", "ok", rr.version+" ("+b+")", "")
	}
	fr := &FnRuntime{app: &App{Cfg: cfg}}
	if d := fr.denoPath(); d == "" {
		add("deno", "warn", "not found - functions won't run", "curl -fsSL https://deno.land/install.sh | sh")
	} else {
		add("deno", "ok", fr.denoVer, "")
	}

	// is a server already running here?
	running := false
	if resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + cfg.Listen + "/healthz"); err == nil {
		resp.Body.Close()
		running = resp.StatusCode == 200
		add("server", "ok", "running on "+cfg.Listen, "")
	} else if ln, err := net.Listen("tcp", cfg.Listen); err == nil {
		ln.Close()
		add("server", "ok", "not running; "+cfg.Listen+" is free", "")
	} else {
		add("server", "error", cfg.Listen+" is used by another program", "change it: ivarabase config set listen 127.0.0.1:8788")
	}

	if cfg.Domain == "" {
		if cfg.TLS.Mode == "proxy" {
			add("domain", "error", "tls.mode is proxy but no domain is set", "ivarabase config set domain base.example.com")
		} else {
			add("domain", "warn", "no domain: serving plain HTTP on "+cfg.Listen, "on a server: ivarabase config set domain base.example.com")
		}
		return out, cfg
	}
	add("https", "ok", "mode "+cfg.TLSMode()+", panel "+cfg.PublicURL+", projects "+(&App{Cfg: cfg}).projectURL("<slug>"), "")
	local := localIPs()
	for _, host := range []string{cfg.Domain, "ivb-check-" + randHex(3) + "." + cfg.Domain} {
		label := host
		if strings.HasPrefix(host, "ivb-check-") {
			label = "*." + cfg.Domain
		}
		ips, err := net.LookupHost(host)
		if err != nil || len(ips) == 0 {
			add("dns "+label, "error", "does not resolve", "add an A record for "+label+" pointing to this server")
			continue
		}
		mine := false
		for _, ip := range ips {
			if local[ip] {
				mine = true
			}
		}
		if mine {
			add("dns "+label, "ok", strings.Join(ips, ", ")+" (this machine)", "")
		} else {
			add("dns "+label, "warn", strings.Join(ips, ", ")+" - not an address of this machine (fine behind NAT or a proxy)", "point "+label+" at this server's public IP")
		}
	}
	if cfg.ServesTLS() && !running {
		for _, addr := range []string{cfg.httpsAddr(), cfg.httpAddr()} {
			if ln, err := net.Listen("tcp", addr); err != nil {
				fix := "free the port or change tls.https_addr / tls.http_addr"
				if strings.Contains(err.Error(), "permission") {
					fix = "ports below 1024 need root or CAP_NET_BIND_SERVICE: use `sudo ivarabase service install`"
				}
				add("port "+addr, "warn", err.Error(), fix)
			} else {
				ln.Close()
				add("port "+addr, "ok", "available", "")
			}
		}
	}
	if cfg.TLSMode() == "proxy" {
		if !isLoopbackAddr(cfg.Listen) {
			add("listen", "warn", cfg.Listen+" is reachable without the proxy: visitors could skip HTTPS", "ivarabase config set listen 127.0.0.1:8787")
		}
		add("proxy", "ok", "HTTPS ends at your proxy: forward "+cfg.Domain+" and *."+cfg.Domain+" to http://"+cfg.upstreamAddr()+
			" (ivarabase proxy-config prints a ready-made nginx config, --server caddy for Caddy)", "")
	}
	if cfg.TLSMode() == "files" || cfg.TLSMode() == "proxy" && cfg.hasCertFiles() {
		names := []string{cfg.Domain, "db." + cfg.Domain}
		if st, detail, fix := checkCertFiles(cfg.TLS.CertFile, cfg.TLS.KeyFile, names); st != "" {
			add("certificate", st, detail, fix)
		}
	}
	if cfg.TLSMode() == "proxy" && !cfg.hasCertFiles() && !isLoopbackAddr(cfg.Postgres.Listen) {
		add("postgres tls", "warn", "Postgres accepts remote connections but has no certificate",
			"point it at the proxy's certificate: ivarabase config set tls.cert_file /path/fullchain.pem; ivarabase config set tls.key_file /path/privkey.pem")
	}
	if cfg.TLSMode() == "auto" && cfg.TLS.Email == "" {
		add("tls email", "warn", "no Let's Encrypt contact email", "ivarabase config set tls.email you@example.com")
	}
	if _, err := exec.LookPath("ufw"); err == nil && runtime.GOOS == "linux" {
		if cfg.TLSMode() == "proxy" {
			add("firewall", "ok", "ufw found - your proxy owns 80/443; open the Postgres port only if clients connect directly",
				fmt.Sprintf("sudo ufw allow %d/tcp", cfg.Postgres.Port))
		} else {
			add("firewall", "ok", "ufw found - allow 80 and 443 (and the Postgres port if you open it)", "sudo ufw allow 80,443/tcp")
		}
	}
	return out, cfg
}

func isLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkCertFiles loads a certificate pair and checks it covers names and isn't
// about to expire. Returns "" when all is well.
func checkCertFiles(certFile, keyFile string, names []string) (status, detail, fix string) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		fix = "check tls.cert_file / tls.key_file"
		if os.IsPermission(err) || strings.Contains(err.Error(), "permission denied") {
			fix = "files only root can read (as most panels and certbot keep them): `sudo ivarabase service install ... --cert-file " + certFile + " --key-file " + keyFile +
				"` copies them for the service and keeps the copy current (or run doctor with sudo to check them)"
		}
		return "error", err.Error(), fix
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "error", "unreadable certificate: " + err.Error(), "check tls.cert_file"
	}
	var missing []string
	for _, n := range names {
		if leaf.VerifyHostname(n) != nil {
			missing = append(missing, n)
		}
	}
	left := time.Until(leaf.NotAfter)
	switch {
	case len(missing) > 0:
		return "warn", "certificate doesn't cover " + strings.Join(missing, ", ") + " (has " + strings.Join(leaf.DNSNames, ", ") + ")",
			"use a certificate for the domain and *.domain"
	case left <= 0:
		return "error", "certificate expired " + leaf.NotAfter.Format("2006-01-02"), "renew it"
	case left < 14*24*time.Hour:
		return "warn", fmt.Sprintf("certificate expires in %d days", int(left.Hours()/24)), "check that renewal works"
	}
	return "ok", "covers " + strings.Join(names, ", ") + ", valid until " + leaf.NotAfter.Format("2006-01-02"), ""
}

func localIPs() map[string]bool {
	out := map[string]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

// ConfigKeys are the settings `ivarabase config set` accepts.
var ConfigKeys = map[string]string{
	"domain":                    "panel at https://<domain>, projects at https://<slug>.<domain>",
	"public_url":                "override the public base URL (when TLS is off; in proxy mode it defaults to https://<domain>)",
	"trusted_proxies":           "comma-separated IPs/CIDRs allowed to set X-Forwarded-For (default: loopback); empty resets",
	"listen":                    "local HTTP address, default 127.0.0.1:8787",
	"tls.mode":                  "auto (Let's Encrypt) | files | proxy (nginx/Caddy/Traefik in front does HTTPS) | off",
	"tls.email":                 "Let's Encrypt contact email",
	"tls.cert_file":             "certificate PEM (files mode; in proxy mode used for Postgres TLS)",
	"tls.key_file":              "private key PEM (files mode; in proxy mode used for Postgres TLS)",
	"tls.https_addr":            "default :443",
	"tls.http_addr":             "default :80",
	"postgres.port":             "managed Postgres port, default 54329",
	"postgres.listen_addresses": "127.0.0.1 (default) or * to allow remote clients (TLS enforced once a certificate exists)",
	"postgres.public_host":      "host shown in connection strings",
	"postgres.bin_dir":          "PostgreSQL binaries directory",
	"deno_path":                 "path to deno",
	"postgrest_path":            "path to postgrest",
}

// SetConfig changes one setting in <data>/config.json.
func SetConfig(dataDir, key, value string) (*Config, error) {
	if _, ok := ConfigKeys[key]; !ok {
		return nil, fmt.Errorf("unknown setting %q", key)
	}
	cfg, err := LoadConfig(dataDir)
	if err != nil {
		return nil, err
	}
	switch key {
	case "domain":
		cfg.Domain = strings.ToLower(strings.TrimSpace(value))
	case "public_url":
		cfg.PublicURL = strings.TrimRight(value, "/")
	case "listen":
		cfg.Listen = value
	case "trusted_proxies":
		var list []string
		for _, p := range strings.Split(value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				list = append(list, p)
			}
		}
		if _, err := parseTrusted(list); err != nil {
			return nil, err
		}
		cfg.TrustedProxies = list
	case "tls.mode":
		if value != "" && value != "auto" && value != "files" && value != "proxy" && value != "off" {
			return nil, fmt.Errorf("tls.mode must be auto, files, proxy or off")
		}
		cfg.TLS.Mode = value
	case "tls.email":
		cfg.TLS.Email = value
	case "tls.cert_file":
		cfg.TLS.CertFile = value
	case "tls.key_file":
		cfg.TLS.KeyFile = value
	case "tls.https_addr":
		cfg.TLS.HTTPSAddr = value
	case "tls.http_addr":
		cfg.TLS.HTTPAddr = value
	case "postgres.port":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("postgres.port must be a port number")
		}
		cfg.Postgres.Port = n
	case "postgres.listen_addresses":
		cfg.Postgres.Listen = value
	case "postgres.public_host":
		cfg.Postgres.PublicHost = value
	case "postgres.bin_dir":
		cfg.Postgres.BinDir = value
	case "deno_path":
		cfg.DenoPath = value
	case "postgrest_path":
		cfg.PostgrestPath = value
	}
	if key == "domain" || key == "tls.mode" || key == "listen" || key == "tls.https_addr" {
		cfg.PublicURL = "" // re-derive from the new setting
	}
	cfg.derivePublicURL()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	return cfg, cfg.Save()
}
