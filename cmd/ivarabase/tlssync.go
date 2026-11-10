package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"ivarabase/internal/core"
)

// Certificates the service user can't read. Hosting panels and certbot keep keys
// root-only in folders only root may enter. Rather than loosening permissions
// on another program's private folders, a root-run oneshot copies the pair
// into <data>/tls whenever the originals change (a systemd path unit) and once
// a day (a timer, in case a change was missed). IvaraBase already re-reads its
// certificate files every hour, so renewals reach Postgres without a restart.

const (
	tlsSyncName  = "ivarabase-tls-sync"
	tlsSyncUnits = "/etc/systemd/system/" + tlsSyncName
)

func tlsCopyPaths(data string) (string, string) {
	return filepath.Join(data, "tls", "fullchain.pem"), filepath.Join(data, "tls", "privkey.pem")
}

// readableBy reports whether account u can read path (checked as that user).
func readableBy(u, path string) bool {
	return exec.Command("runuser", "-u", u, "--", "test", "-r", path).Run() == nil
}

// setupCertSync decides whether the configured certificate needs copying for
// the service user and installs or removes the sync units accordingly.
func setupCertSync(data, userName, bin string) error {
	cfg, err := core.LoadConfig(data)
	if err != nil {
		return err
	}
	copyCrt, copyKey := tlsCopyPaths(data)
	t := &cfg.TLS
	if t.CertFile != "" && t.KeyFile != "" && t.CertFile != copyCrt { // a new or first source
		if readableBy(userName, t.CertFile) && readableBy(userName, t.KeyFile) {
			t.SyncCertFrom, t.SyncKeyFrom = "", ""
		} else {
			t.SyncCertFrom, t.SyncKeyFrom = t.CertFile, t.KeyFile
			t.CertFile, t.KeyFile = copyCrt, copyKey
		}
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	if t.SyncCertFrom == "" {
		removeCertSync()
		return nil
	}
	if _, err := syncCerts(cfg); err != nil {
		return err
	}
	units := map[string]string{
		".service": fmt.Sprintf(`[Unit]
Description=Copy the TLS certificate IvaraBase uses for Postgres (from %s)

[Service]
Type=oneshot
ExecStart=%s tls-sync --data %s
`, filepath.Dir(t.SyncCertFrom), bin, data),
		".path": fmt.Sprintf(`[Unit]
Description=Watch the certificate IvaraBase copies

[Path]
PathChanged=%s
PathChanged=%s

[Install]
WantedBy=multi-user.target
`, t.SyncCertFrom, t.SyncKeyFrom),
		".timer": `[Unit]
Description=Daily check of the certificate IvaraBase copies

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
`,
	}
	for ext, body := range units {
		if err := os.WriteFile(tlsSyncUnits+ext, []byte(body), 0o644); err != nil {
			return err
		}
	}
	run("systemctl", "daemon-reload")
	if err := run("systemctl", "enable", "--now", tlsSyncName+".path", tlsSyncName+".timer"); err != nil {
		return err
	}
	fmt.Printf("==> certificate %s is root-only: copied to %s, kept current by %s.path/.timer\n", t.SyncCertFrom, filepath.Dir(copyCrt), tlsSyncName)
	return nil
}

func removeCertSync() {
	if !fileExists(tlsSyncUnits + ".service") {
		return
	}
	exec.Command("systemctl", "disable", "--now", tlsSyncName+".path", tlsSyncName+".timer").Run()
	for _, ext := range []string{".service", ".path", ".timer"} {
		os.Remove(tlsSyncUnits + ext)
	}
	exec.Command("systemctl", "daemon-reload").Run()
}

// syncCerts copies sync_cert_from/sync_key_from over cert_file/key_file when
// they differ, owned by whoever owns the data directory, mode 0600. A pair
// that doesn't load (a renewal half written) is left alone.
func syncCerts(cfg *core.Config) (bool, error) {
	t := cfg.TLS
	if t.SyncCertFrom == "" || t.SyncKeyFrom == "" || t.CertFile == "" || t.KeyFile == "" {
		return false, errors.New("no certificate to sync (tls.sync_cert_from is empty)")
	}
	crt, err := os.ReadFile(t.SyncCertFrom)
	if err != nil {
		return false, err
	}
	key, err := os.ReadFile(t.SyncKeyFrom)
	if err != nil {
		return false, err
	}
	if _, err := tls.X509KeyPair(crt, key); err != nil {
		return false, fmt.Errorf("%s / %s don't form a valid pair (renewal in progress?): %w", t.SyncCertFrom, t.SyncKeyFrom, err)
	}
	oldCrt, _ := os.ReadFile(t.CertFile)
	oldKey, _ := os.ReadFile(t.KeyFile)
	if bytes.Equal(oldCrt, crt) && bytes.Equal(oldKey, key) {
		return false, nil
	}
	st, err := os.Stat(cfg.DataDir)
	if err != nil {
		return false, err
	}
	uid, gid := -1, -1
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		uid, gid = int(sys.Uid), int(sys.Gid)
	}
	dir := filepath.Dir(t.CertFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	os.Chown(dir, uid, gid)
	// key first: the server re-reads when the certificate file changes
	for _, f := range []struct {
		path string
		data []byte
	}{{t.KeyFile, key}, {t.CertFile, crt}} {
		tmp := f.path + ".tmp"
		if err := os.WriteFile(tmp, f.data, 0o600); err != nil {
			return false, err
		}
		if err := os.Chown(tmp, uid, gid); err != nil {
			os.Remove(tmp)
			return false, err
		}
		if err := os.Rename(tmp, f.path); err != nil {
			return false, err
		}
	}
	return true, nil
}

// `ivarabase tls-sync --data DIR` (run by ivarabase-tls-sync.service as root).
func cmdTLSSync(args []string) error {
	fs := flag.NewFlagSet("tls-sync", flag.ExitOnError)
	data := fs.String("data", "/var/lib/ivarabase", "data directory")
	fs.Parse(args)
	cfg, err := core.LoadConfig(*data)
	if err != nil {
		return err
	}
	changed, err := syncCerts(cfg)
	if err != nil {
		return err
	}
	if changed {
		fmt.Println("certificate updated from", cfg.TLS.SyncCertFrom, "- IvaraBase picks it up within the hour")
	} else {
		fmt.Println("certificate unchanged")
	}
	return nil
}
