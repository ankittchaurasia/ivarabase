package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookPrivateTargets(t *testing.T) {
	ctx := context.Background()
	fakeDNS := func(answer string) func(context.Context, string) ([]net.IPAddr, error) {
		return func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP(answer)}}, nil
		}
	}
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "224.0.0.1"}
	for _, ip := range blocked {
		if !blockedIP(net.ParseIP(ip)) {
			t.Errorf("%s should be blocked", ip)
		}
		host := ip
		if strings.Contains(ip, ":") {
			host = "[" + ip + "]"
		}
		err := checkHookURL(ctx, "http://"+host+"/hook", false, nil)
		if AsAPIError(err).Code != "webhook_url_private" {
			t.Errorf("literal %s: got %v", ip, err)
		}
		if err := checkHookURL(ctx, "http://"+host+"/hook", true, nil); err != nil {
			t.Errorf("literal %s with allow_private: %v", ip, err)
		}
		// a name that resolves to a private address is refused too
		if err := checkHookURL(ctx, "https://hooks.example.com/x", false, fakeDNS(ip)); AsAPIError(err).Code != "webhook_url_private" {
			t.Errorf("name -> %s: got %v", ip, err)
		}
	}
	for _, ip := range []string{"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111"} {
		if blockedIP(net.ParseIP(ip)) {
			t.Errorf("%s should be allowed", ip)
		}
	}
	if err := checkHookURL(ctx, "https://hooks.example.com/x", false, fakeDNS("93.184.216.34")); err != nil {
		t.Errorf("public name: %v", err)
	}
	for _, bad := range []string{"ftp://example.com/", "example.com/x", "http:///x", "javascript:alert(1)"} {
		if AsAPIError(checkHookURL(ctx, bad, true, nil)).Code != "invalid_url" {
			t.Errorf("%q should be an invalid URL", bad)
		}
	}
}

// The dialer refuses private addresses at connect time, which also covers DNS
// rebinding (a name that resolved publicly when the hook was saved).
func TestWebhookDialerRefusesPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	if _, err := hookHTTPClient(false).Get(srv.URL); !errors.Is(err, errHookPrivate) {
		t.Fatalf("strict client reached %s: %v", srv.URL, err)
	}
	resp, err := hookHTTPClient(true).Get(srv.URL)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("permissive client: %v", err)
	}
	resp.Body.Close()
}

func TestWebhookSignature(t *testing.T) {
	body := []byte(`{"type":"INSERT"}`)
	m := hmac.New(sha256.New, []byte("whsec_x"))
	m.Write([]byte("1700000000." + string(body)))
	if got, want := signHook("whsec_x", 1700000000, body), "sha256="+hex.EncodeToString(m.Sum(nil)); got != want {
		t.Fatalf("signature %s, want %s", got, want)
	}
}

func TestWebhookArgs(t *testing.T) {
	if s, tb, err := parseHookTable("orders"); err != nil || s != "public" || tb != "orders" {
		t.Fatalf("orders: %s %s %v", s, tb, err)
	}
	if s, tb, _ := parseHookTable("auth.users"); s != "auth" || tb != "users" {
		t.Fatalf("auth.users: %s %s", s, tb)
	}
	for _, bad := range []string{"", "pg_catalog.pg_class", "ivarabase_hooks.events", "x."} {
		if _, _, err := parseHookTable(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if ev, err := parseHookEvents([]string{"delete", "INSERT"}); err != nil || strings.Join(ev, ",") != "INSERT,DELETE" {
		t.Fatalf("events: %v %v", ev, err)
	}
	if _, err := parseHookEvents([]string{"TRUNCATE"}); err == nil {
		t.Fatal("TRUNCATE should be refused")
	}
	if _, err := parseHookHeaders(map[string]any{"X-Ivarabase-Signature": "x"}); err == nil {
		t.Fatal("reserved header accepted")
	}
	if _, err := parseHookHeaders(map[string]any{"X-Evil": "a\r\nb"}); err == nil {
		t.Fatal("header injection accepted")
	}
	if h, err := parseHookHeaders(map[string]any{"Authorization": "Bearer t"}); err != nil || h["Authorization"] != "Bearer t" {
		t.Fatalf("headers: %v %v", h, err)
	}
}
