package core

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Settings can come from IVARA_* environment variables as well as config.json.
// The environment wins and is never written back to config.json, so a Docker
// compose file or <data>/ivarabase.env stays the single place to change them.
//
// <data>/ivarabase.env (or the file named by IVARA_ENV_FILE) is read first;
// variables already set in the real environment take precedence over it.

type envSetting struct {
	name string
	desc string
	set  func(c *Config, v string) error
	keep func(dst, src *Config) // copy the config.json value back before saving
}

func envString(name, desc string, field func(c *Config) *string) envSetting {
	return envSetting{name: name, desc: desc,
		set:  func(c *Config, v string) error { *field(c) = strings.TrimSpace(v); return nil },
		keep: func(dst, src *Config) { *field(dst) = *field(src) }}
}

func envPGSetting(name, key, desc string) envSetting {
	return envSetting{name: name, desc: desc,
		set: func(c *Config, v string) error {
			if c.Postgres.Settings == nil {
				c.Postgres.Settings = map[string]string{}
			}
			c.Postgres.Settings[key] = strings.TrimSpace(v)
			return nil
		},
		keep: func(dst, src *Config) {
			m := map[string]string{}
			for k, v := range dst.Postgres.Settings {
				m[k] = v
			}
			if sv, ok := src.Postgres.Settings[key]; ok {
				m[key] = sv
			} else {
				delete(m, key)
			}
			dst.Postgres.Settings = m
		}}
}

var envSettings = []envSetting{
	envString("IVARA_LISTEN", "HTTP listen address (API, UI, MCP)", func(c *Config) *string { return &c.Listen }),
	envString("IVARA_PUBLIC_URL", "URL clients use to reach the server", func(c *Config) *string { return &c.PublicURL }),
	envString("IVARA_DOMAIN", "panel at https://<domain>, projects at https://<slug>.<domain>", func(c *Config) *string { return &c.Domain }),
	envString("IVARA_TLS_MODE", "auto | files | proxy | off", func(c *Config) *string { return &c.TLS.Mode }),
	envString("IVARA_TLS_EMAIL", "Let's Encrypt contact", func(c *Config) *string { return &c.TLS.Email }),
	envString("IVARA_TLS_CERT_FILE", "certificate (PEM); in proxy mode used for Postgres TLS", func(c *Config) *string { return &c.TLS.CertFile }),
	envString("IVARA_TLS_KEY_FILE", "private key (PEM)", func(c *Config) *string { return &c.TLS.KeyFile }),
	{name: "IVARA_TRUSTED_PROXIES", desc: "comma-separated IPs/CIDRs allowed to set X-Forwarded-For",
		set: func(c *Config, v string) error {
			c.TrustedProxies = nil
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					c.TrustedProxies = append(c.TrustedProxies, s)
				}
			}
			return nil
		},
		keep: func(dst, src *Config) { dst.TrustedProxies = src.TrustedProxies }},
	envString("IVARA_DENO_PATH", "path to deno", func(c *Config) *string { return &c.DenoPath }),
	envString("IVARA_POSTGREST_PATH", "path to postgrest", func(c *Config) *string { return &c.PostgrestPath }),
	{name: "IVARA_WEBHOOKS_ALLOW_PRIVATE", desc: "1 = database webhooks may call private/loopback addresses",
		set: func(c *Config, v string) error {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("IVARA_WEBHOOKS_ALLOW_PRIVATE: %q is not true/false", v)
			}
			c.WebhooksAllowPrivate = b
			return nil
		},
		keep: func(dst, src *Config) { dst.WebhooksAllowPrivate = src.WebhooksAllowPrivate }},
	envString("IVARA_PG_BIN_DIR", "folder with initdb/pg_ctl/postgres", func(c *Config) *string { return &c.Postgres.BinDir }),
	{name: "IVARA_PG_PORT", desc: "managed Postgres port",
		set: func(c *Config, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n <= 0 || n > 65535 {
				return fmt.Errorf("IVARA_PG_PORT: %q is not a port", v)
			}
			c.Postgres.Port = n
			return nil
		},
		keep: func(dst, src *Config) { dst.Postgres.Port = src.Postgres.Port }},
	envString("IVARA_PG_LISTEN", "Postgres listen_addresses (127.0.0.1 or *)", func(c *Config) *string { return &c.Postgres.Listen }),
	envString("IVARA_PG_PUBLIC_HOST", "host shown in connection strings", func(c *Config) *string { return &c.Postgres.PublicHost }),
	envString("IVARA_PG_EXTERNAL_DSN", "use an existing cluster (superuser DSN)", func(c *Config) *string { return &c.Postgres.ExternalDSN }),
	// tuning (defaults scale with available memory, see pgtune.go)
	envPGSetting("IVARA_PG_MAX_CONNECTIONS", "max_connections", "Postgres max_connections (default 200)"),
	envPGSetting("IVARA_PG_SHARED_BUFFERS", "shared_buffers", "default 20% of memory, 128MB-8GB"),
	envPGSetting("IVARA_PG_EFFECTIVE_CACHE_SIZE", "effective_cache_size", "default 60% of memory"),
	envPGSetting("IVARA_PG_WORK_MEM", "work_mem", "default scales with memory, 4MB-64MB"),
	envPGSetting("IVARA_PG_MAINTENANCE_WORK_MEM", "maintenance_work_mem", "default 5% of memory, 64MB-1GB"),
	envPGSetting("IVARA_PG_MAX_WAL_SIZE", "max_wal_size", "default 1GB"),
	envPGSetting("IVARA_PG_RANDOM_PAGE_COST", "random_page_cost", "default 1.1 (SSD)"),
	envPGSetting("IVARA_PG_MAX_WORKER_PROCESSES", "max_worker_processes", "default 8"),
	envPGSetting("IVARA_PG_MAX_PARALLEL_WORKERS", "max_parallel_workers", "default 8"),
	envPGSetting("IVARA_PG_STATEMENT_TIMEOUT", "statement_timeout", "default 0 (off)"),
}

// EnvSettings lists the supported variables (for docs and `ivarabase env`).
func EnvSettings() [][2]string {
	out := make([][2]string, 0, len(envSettings)+1)
	for _, e := range envSettings {
		out = append(out, [2]string{e.name, e.desc})
	}
	out = append(out, [2]string{"IVARA_PG_SET_<name>", "any other Postgres setting, e.g. IVARA_PG_SET_LOG_MIN_DURATION_STATEMENT=500ms"})
	return out
}

// loadEnvFile reads KEY=VALUE lines into the environment without overriding
// variables that are already set. Missing file = nothing to do.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, ln)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// applyEnv overlays IVARA_* variables onto cfg and remembers which ones it set.
func (c *Config) applyEnv() error {
	file := os.Getenv("IVARA_ENV_FILE")
	if file == "" {
		file = c.Path("ivarabase.env")
	}
	if err := loadEnvFile(file); err != nil {
		return err
	}
	snap := c.clone()
	c.fileValues = snap
	c.envSet = nil
	for _, e := range envSettings {
		if v, ok := os.LookupEnv(e.name); ok && v != "" {
			if err := e.set(c, v); err != nil {
				return err
			}
			c.envSet = append(c.envSet, e)
		}
	}
	// IVARA_PG_SET_<NAME>=value for any other Postgres setting
	var extra []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if name, ok := strings.CutPrefix(k, "IVARA_PG_SET_"); ok && name != "" && v != "" {
			extra = append(extra, strings.ToLower(name)+"="+v)
		}
	}
	sort.Strings(extra)
	for _, kv := range extra {
		k, v, _ := strings.Cut(kv, "=")
		e := envPGSetting("IVARA_PG_SET_"+strings.ToUpper(k), k, "")
		e.set(c, v)
		c.envSet = append(c.envSet, e)
	}
	return nil
}

// EnvOverrides names the variables that changed the loaded config.
func (c *Config) EnvOverrides() []string {
	var out []string
	for _, e := range c.envSet {
		out = append(out, e.name)
	}
	return out
}

func (c *Config) clone() *Config {
	cp := *c
	cp.TrustedProxies = append([]string(nil), c.TrustedProxies...)
	if c.Postgres.Settings != nil {
		cp.Postgres.Settings = map[string]string{}
		for k, v := range c.Postgres.Settings {
			cp.Postgres.Settings[k] = v
		}
	}
	return &cp
}

// forSave returns the config with environment overrides replaced by the
// values that came from config.json.
func (c *Config) forSave() *Config {
	out := c.clone()
	if c.fileValues != nil {
		for _, e := range c.envSet {
			e.keep(out, c.fileValues)
		}
		// a public URL derived from an environment domain isn't saved either
		if len(c.envSet) > 0 && out.PublicURL == c.loadedURL {
			out.PublicURL = c.fileValues.PublicURL
		}
	}
	return out
}
