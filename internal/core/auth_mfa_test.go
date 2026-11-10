package core

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B (SHA-1), last 6 digits.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for ts, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpCode(secret, ts/totpStep); got != want {
			t.Errorf("t=%d: %s, want %s", ts, got, want)
		}
	}
	now := time.Unix(1111111109, 0)
	if totpMatch(secret, "081804", now) == 0 || totpMatch(secret, "081804", now.Add(30*time.Second)) == 0 {
		t.Fatal("code should match now and one step later")
	}
	if totpMatch(secret, "081804", now.Add(90*time.Second)) != 0 || totpMatch(secret, "12345", now) != 0 {
		t.Fatal("stale or short code matched")
	}
}

func TestQRSVG(t *testing.T) {
	for _, n := range []int{1, 20, 120, 300, 600} {
		svg, err := qrSVG("otpauth://totp/x?secret=" + strings.Repeat("A", n))
		if err != nil || !strings.HasPrefix(svg, "<svg") || strings.Contains(svg, "#") {
			t.Fatalf("len %d: %v", n, err)
		}
	}
	if _, err := qrSVG(strings.Repeat("A", 700)); err == nil {
		t.Fatal("too long must fail")
	}
}

func TestNormPhone(t *testing.T) {
	for in, want := range map[string]string{"+1 (555) 123-4567": "15551234567", "919876543210": "919876543210"} {
		if got, err := normPhone(in); err != nil || got != want {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "123", "+0123456789", "555-abc-1234", "1234567890123456"} {
		if _, err := normPhone(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
