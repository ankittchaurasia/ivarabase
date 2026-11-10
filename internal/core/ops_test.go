package core

import (
	"testing"
	"time"
)

func TestCron(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	cases := []struct {
		expr string
		t    string
		want bool
	}{
		{"* * * * *", "2026-11-09T10:11:00Z", true},
		{"*/15 * * * *", "2026-11-09T10:30:00Z", true},
		{"*/15 * * * *", "2026-11-09T10:31:00Z", false},
		{"0 3 * * *", "2026-11-09T03:00:00Z", true},
		{"0 3 * * *", "2026-11-09T04:00:00Z", false},
		{"0 9 * * 1", "2026-11-09T09:00:00Z", true}, // a Monday
		{"0 9 * * 7", "2026-11-08T09:00:00Z", true}, // 7 = Sunday
		{"0 0 1 * *", "2026-12-01T00:00:00Z", true},
		{"0 0 1 * 1", "2026-11-09T00:00:00Z", true}, // dom OR dow when both are set
		{"5-10/5 * * * *", "2026-11-09T10:10:00Z", true},
		{"5-10/5 * * * *", "2026-11-09T10:07:00Z", false},
	}
	for _, c := range cases {
		cs, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := cs.matches(at(c.t)); got != c.want {
			t.Errorf("%q at %s = %v, want %v", c.expr, c.t, got, c.want)
		}
	}
	for _, bad := range []string{"* * * *", "61 * * * *", "* 24 * * *", "*/0 * * * *", "a * * * *"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	cs, _ := parseCron("0 3 * * *")
	if n := cs.next(at("2026-11-09T03:00:00Z")); !n.Equal(at("2026-11-10T03:00:00Z")) {
		t.Errorf("next = %s", n)
	}
}

func TestPublicURLs(t *testing.T) {
	c := defaultConfig("/tmp/x")
	c.derivePublicURL()
	if c.PublicURL != "http://127.0.0.1:8787" || c.TLSMode() != "off" {
		t.Fatalf("default: %s %s", c.PublicURL, c.TLSMode())
	}
	a := &App{Cfg: c}
	if u := a.projectURL("shop"); u != "http://127.0.0.1:8787/proj/shop" {
		t.Fatalf("path-style project URL: %s", u)
	}
	c.Domain = "base.example.com"
	c.PublicURL = ""
	c.derivePublicURL()
	if c.PublicURL != "https://base.example.com" || c.TLSMode() != "auto" {
		t.Fatalf("domain: %s %s", c.PublicURL, c.TLSMode())
	}
	if u := a.projectURL("shop"); u != "https://shop.base.example.com" {
		t.Fatalf("subdomain project URL: %s", u)
	}
	if u := a.projectURL("my_app"); u != "https://my-app.base.example.com" {
		t.Fatalf("underscores must become hyphens in hostnames: %s", u)
	}
	if slugFromLabel("my-app") != "my_app" {
		t.Fatal("hostname label must map back to the slug")
	}
	c.TLS.HTTPSAddr = ":8443"
	c.derivePublicURL()
	if u := a.projectURL("shop"); u != "https://shop.base.example.com:8443" {
		t.Fatalf("port kept: %s", u)
	}
	c.TLS.Mode = "off"
	c.PublicURL = ""
	c.derivePublicURL()
	if c.PublicURL != "http://base.example.com" {
		t.Fatalf("behind a proxy: %s", c.PublicURL)
	}
	c.TLS.Mode = ""
	c.TLS.CertFile, c.TLS.KeyFile = "/c.pem", "/k.pem"
	if c.TLSMode() != "files" {
		t.Fatalf("files mode: %s", c.TLSMode())
	}
}

func TestStorageNames(t *testing.T) {
	for _, ok := range []string{"a.png", "user/1/a.png", ".emptyFolderPlaceholder", "x y.txt"} {
		if validObjectName(ok) != nil {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "/abs", "a/../b", "a//b", "..", "a\\\\b"} {
		if validObjectName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	for _, bad := range []string{"..", ".", "a/b", ""} {
		if validBucketID(bad) {
			t.Errorf("bucket %q should be invalid", bad)
		}
	}
}
