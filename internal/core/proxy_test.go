package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProxyModeConfig(t *testing.T) {
	c := defaultConfig("/tmp/x")
	c.Domain = "base.example.com"
	c.TLS.Mode = "proxy"
	c.TLS.CertFile, c.TLS.KeyFile = "/c.pem", "/k.pem" // Postgres only: must not switch to files mode
	c.derivePublicURL()
	if c.TLSMode() != "proxy" || c.ServesTLS() {
		t.Fatalf("proxy mode must not serve TLS itself: mode %s", c.TLSMode())
	}
	if c.PublicURL != "https://base.example.com" {
		t.Fatalf("public URL behind a proxy is https://<domain>: %s", c.PublicURL)
	}
	if u := (&App{Cfg: c}).projectURL("my_app"); u != "https://my-app.base.example.com" {
		t.Fatalf("project URL: %s", u)
	}
	c.PublicURL = "https://base.example.com:8443"
	c.derivePublicURL()
	if c.PublicURL != "https://base.example.com:8443" {
		t.Fatalf("an https public_url on the domain (proxy on another port) is kept: %s", c.PublicURL)
	}
	c.PublicURL = "http://127.0.0.1:8787" // stale value saved before proxy mode
	c.derivePublicURL()
	if c.PublicURL != "https://base.example.com" {
		t.Fatalf("stale public_url replaced: %s", c.PublicURL)
	}
	c.Domain = ""
	if c.TLSMode() != "off" {
		t.Fatal("proxy mode without a domain is off")
	}
	if !(&Config{TLS: TLSConfig{Mode: "files", CertFile: "a", KeyFile: "b"}, Domain: "d"}).ServesTLS() {
		t.Fatal("files mode serves TLS")
	}
}

func TestSetConfigProxy(t *testing.T) {
	dir := t.TempDir()
	if _, err := SetConfig(dir, "domain", "base.example.com"); err != nil {
		t.Fatal(err)
	}
	cfg, err := SetConfig(dir, "tls.mode", "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://base.example.com" || cfg.TLSMode() != "proxy" {
		t.Fatalf("%s %s", cfg.PublicURL, cfg.TLSMode())
	}
	if _, err := SetConfig(dir, "trusted_proxies", "10.0.0.0/8, 172.17.0.1"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = LoadConfig(dir)
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("trusted_proxies: %v", cfg.TrustedProxies)
	}
	if _, err := SetConfig(dir, "trusted_proxies", "not-an-ip"); err == nil {
		t.Fatal("bad trusted_proxies must be refused")
	}
	if _, err := SetConfig(dir, "trusted_proxies", ""); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = LoadConfig(dir); len(cfg.TrustedProxies) != 0 {
		t.Fatal("empty value resets to loopback")
	}
}

func TestClientIP(t *testing.T) {
	a := &App{Cfg: defaultConfig("/tmp/x")}
	req := func(remote string, h map[string]string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return a.clientIP(r)
	}
	cases := []struct {
		name, remote string
		h            map[string]string
		want         string
	}{
		{"direct client", "203.0.113.9:5000", nil, "203.0.113.9"},
		{"direct client can't spoof", "203.0.113.9:5000", map[string]string{"X-Forwarded-For": "1.1.1.1", "X-Real-IP": "1.1.1.1"}, "203.0.113.9"},
		{"local nginx", "127.0.0.1:40000", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7"},
		{"nginx appends to a forged header", "127.0.0.1:40000", map[string]string{"X-Forwarded-For": "1.1.1.1, 198.51.100.7"}, "198.51.100.7"},
		{"X-Real-IP only", "127.0.0.1:40000", map[string]string{"X-Real-IP": "198.51.100.7"}, "198.51.100.7"},
		{"ipv6 client", "[::1]:40000", map[string]string{"X-Forwarded-For": "2001:db8::5"}, "2001:db8::5"},
		{"garbage stops the chain", "127.0.0.1:40000", map[string]string{"X-Forwarded-For": "1.1.1.1, nonsense", "X-Real-IP": "198.51.100.7"}, "198.51.100.7"},
		{"no headers", "127.0.0.1:40000", nil, "127.0.0.1"},
		{"all hops local", "127.0.0.1:40000", map[string]string{"X-Forwarded-For": "127.0.0.1"}, "127.0.0.1"},
	}
	for _, c := range cases {
		if got := req(c.remote, c.h); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}

	// Docker: nginx on the host reaches the container from the bridge gateway.
	a.Cfg.trusted, _ = parseTrusted([]string{"172.16.0.0/12"})
	if got := req("172.17.0.1:3000", map[string]string{"X-Forwarded-For": "198.51.100.7, 172.18.0.5"}); got != "198.51.100.7" {
		t.Errorf("trusted hops are skipped: %s", got)
	}
	if got := req("127.0.0.1:3000", map[string]string{"X-Forwarded-For": "198.51.100.7"}); got != "127.0.0.1" {
		t.Errorf("an explicit list replaces the loopback default: %s", got)
	}
}

func TestIsHTTPSAndCookie(t *testing.T) {
	c := defaultConfig("/tmp/x")
	c.Domain, c.TLS.Mode = "base.example.com", "proxy"
	c.derivePublicURL()
	a := &App{Cfg: c}
	mk := func(remote, proto string) (bool, bool) {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		return a.isHTTPS(r), a.secureCookie(r)
	}
	if https, sec := mk("127.0.0.1:1", "https"); !https || !sec {
		t.Error("nginx says https: HSTS + Secure cookie")
	}
	if https, sec := mk("127.0.0.1:1", ""); https || sec {
		t.Error("SSH tunnel to the local port is plain http: cookie must work without Secure")
	}
	if https, sec := mk("203.0.113.9:1", "https"); https || !sec {
		t.Error("untrusted X-Forwarded-Proto ignored, but the cookie stays Secure on an https deployment")
	}
}

func TestProxyConfigOutput(t *testing.T) {
	c := defaultConfig("/tmp/x")
	if _, err := ProxyConfig(c, "nginx"); err == nil {
		t.Fatal("needs a domain")
	}
	c.Domain, c.TLS.Mode = "base.example.com", "proxy"
	c.Listen = "0.0.0.0:8787"
	n, err := ProxyConfig(c, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"server_name base.example.com *.base.example.com;", "proxy_pass http://127.0.0.1:8787;",
		"proxy_set_header Host $host;", "X-Forwarded-For $proxy_add_x_forwarded_for", "X-Forwarded-Proto $scheme",
		"client_max_body_size 0;", "proxy_buffering off;", "proxy_read_timeout 310s;", "db.base.example.com:54329"} {
		if !strings.Contains(n, want) {
			t.Errorf("nginx config lacks %q", want)
		}
	}
	cd, err := ProxyConfig(c, "caddy")
	if err != nil || !strings.Contains(cd, "ask http://127.0.0.1:8787/_tls/ask") || !strings.Contains(cd, "base.example.com, *.base.example.com {") {
		t.Fatalf("caddy config: %v\n%s", err, cd)
	}
	if _, err := ProxyConfig(c, "apache"); err == nil {
		t.Fatal("unknown server refused")
	}
}

func TestCheckCertFiles(t *testing.T) {
	dir := t.TempDir()
	crt, key := writeTestCert(t, dir, []string{"base.example.com", "*.base.example.com"}, 90*24*time.Hour)
	if st, d, _ := checkCertFiles(crt, key, []string{"base.example.com", "db.base.example.com"}); st != "ok" {
		t.Fatalf("wildcard covers db.<domain>: %s %s", st, d)
	}
	if st, d, _ := checkCertFiles(crt, key, []string{"other.com"}); st != "warn" || !strings.Contains(d, "other.com") {
		t.Fatalf("missing name: %s %s", st, d)
	}
	crt2, key2 := writeTestCert(t, filepath.Join(dir, "soon"), []string{"base.example.com"}, 5*24*time.Hour)
	if st, d, _ := checkCertFiles(crt2, key2, []string{"base.example.com"}); st != "warn" || !strings.Contains(d, "expires") {
		t.Fatalf("near expiry: %s %s", st, d)
	}
	if st, _, _ := checkCertFiles(filepath.Join(dir, "nope.pem"), key, nil); st != "error" {
		t.Fatal("missing file is an error")
	}
}

func writeTestCert(t *testing.T, dir string, names []string, valid time.Duration) (string, string) {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(valid)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	crt, key := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	if _, err := tls.LoadX509KeyPair(crt, key); err != nil {
		t.Fatal(err)
	}
	return crt, key
}
