package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"ivarabase/internal/core"
)

func mkPair(t *testing.T, dir, cn string) (string, string) {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	crt, key := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	out, err := exec.Command("openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "2",
		"-subj", "/CN="+cn, "-keyout", key, "-out", crt).CombinedOutput()
	if err != nil {
		t.Skipf("openssl unavailable: %v %s", err, out)
	}
	return crt, key
}

func TestSyncCerts(t *testing.T) {
	data := t.TempDir()
	srcCrt, srcKey := mkPair(t, filepath.Join(t.TempDir(), "panel"), "a.test")
	copyCrt, copyKey := tlsCopyPaths(data)
	cfg := &core.Config{DataDir: data, TLS: core.TLSConfig{CertFile: copyCrt, KeyFile: copyKey, SyncCertFrom: srcCrt, SyncKeyFrom: srcKey}}

	if changed, err := syncCerts(cfg); err != nil || !changed {
		t.Fatalf("first copy: changed=%v err=%v", changed, err)
	}
	if st, _ := os.Stat(copyKey); st.Mode().Perm() != 0o600 {
		t.Fatalf("key copy must be 0600, got %v", st.Mode().Perm())
	}
	if changed, err := syncCerts(cfg); err != nil || changed {
		t.Fatalf("unchanged source must not rewrite: changed=%v err=%v", changed, err)
	}
	// renewal: new pair at the same paths
	mkPair(t, filepath.Dir(srcCrt), "a.test")
	if changed, _ := syncCerts(cfg); !changed {
		t.Fatal("renewed source must be copied")
	}
	a, _ := os.ReadFile(srcCrt)
	b, _ := os.ReadFile(copyCrt)
	if string(a) != string(b) {
		t.Fatal("copy differs from source")
	}
	// half-written renewal: key doesn't match the certificate
	_, otherKey := mkPair(t, filepath.Join(t.TempDir(), "other"), "b.test")
	k, _ := os.ReadFile(otherKey)
	os.WriteFile(srcKey, k, 0o600)
	if _, err := syncCerts(cfg); err == nil {
		t.Fatal("a mismatched pair must not be copied")
	}
	if b2, _ := os.ReadFile(copyCrt); string(b2) != string(b) {
		t.Fatal("copies must stay untouched on a bad pair")
	}
}
