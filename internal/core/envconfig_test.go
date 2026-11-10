package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvOverridesAreNotSaved(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"listen":"127.0.0.1:9000","domain":"file.example.com","postgres":{"settings":{"work_mem":"8MB"}}}`), 0o600)
	os.WriteFile(filepath.Join(dir, "ivarabase.env"), []byte("# comment\nIVARA_PG_SHARED_BUFFERS=\"512MB\"\nIVARA_DOMAIN=fromfile.example.com\n"), 0o600)
	t.Setenv("IVARA_DOMAIN", "Env.Example.com") // real env beats the .env file
	t.Setenv("IVARA_TRUSTED_PROXIES", "172.16.0.0/12, 10.0.0.0/8")
	t.Setenv("IVARA_PG_SET_LOG_MIN_DURATION_STATEMENT", "500ms")
	t.Setenv("IVARA_ENV_FILE", "")
	os.Unsetenv("IVARA_PG_SHARED_BUFFERS")
	t.Cleanup(func() { os.Unsetenv("IVARA_PG_SHARED_BUFFERS") })

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Domain != "env.example.com" {
		t.Fatalf("domain = %q", cfg.Domain)
	}
	if cfg.Postgres.Settings["shared_buffers"] != "512MB" || cfg.Postgres.Settings["work_mem"] != "8MB" ||
		cfg.Postgres.Settings["log_min_duration_statement"] != "500ms" {
		t.Fatalf("settings = %v", cfg.Postgres.Settings)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("trusted = %v", cfg.TrustedProxies)
	}
	cfg.Listen = "127.0.0.1:9100" // a real change from the app
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	s := string(b)
	if strings.Contains(s, "env.example.com") || strings.Contains(s, "512MB") || strings.Contains(s, "172.16") || strings.Contains(s, "500ms") {
		t.Fatalf("environment leaked into config.json:\n%s", s)
	}
	if !strings.Contains(s, "file.example.com") || !strings.Contains(s, "9100") || !strings.Contains(s, "8MB") {
		t.Fatalf("config.json lost file values:\n%s", s)
	}
}

func TestPGTuning(t *testing.T) {
	cfg := defaultConfig(t.TempDir())
	cfg.Postgres.Settings = map[string]string{"shared_buffers": "1GB", "search_path": "app, public"}
	tune, mem, _ := pgTuning(cfg)
	if mem <= 0 || tune["shared_buffers"] != "1GB" || tune["max_connections"] != "200" {
		t.Fatalf("tune = %v mem=%d", tune, mem)
	}
	lines, err := pgConfLines(tune)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lines, "shared_buffers = '1GB'\n") || !strings.Contains(lines, "search_path = 'app, public'\n") || !strings.Contains(lines, "random_page_cost = 1.1\n") {
		t.Fatal(lines)
	}
	if _, err := pgConfLines(map[string]string{"port": "1"}); err == nil {
		t.Fatal("port must be refused")
	}
	if _, err := pgConfLines(map[string]string{"x = 1\nssl": "on"}); err == nil {
		t.Fatal("bad name must be refused")
	}
}
