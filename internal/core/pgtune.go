package core

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Postgres tuning: defaults scale with the memory IvaraBase may use (the
// container's cgroup limit when there is one, otherwise the machine's RAM).
// They are deliberately conservative because Postgres shares the machine with
// PostgREST, Deno and often other apps. postgres.settings in config.json and
// IVARA_PG_* variables override any of them.

// memoryLimit returns the bytes available to this process tree and where the
// number came from ("cgroup", "host").
func memoryLimit() (int64, string) {
	host := hostMemory()
	if runtime.GOOS == "linux" {
		for _, f := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			s := strings.TrimSpace(string(b))
			if s == "max" {
				break
			}
			if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 && (host == 0 || n < host) {
				return n, "cgroup"
			}
		}
	}
	return host, "host"
}

func hostMemory() int64 {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0
		}
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemTotal:" {
				n, _ := strconv.ParseInt(f[1], 10, 64)
				return n * 1024
			}
		}
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
			return n
		}
	}
	return 0
}

const mb = int64(1 << 20)

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func pgSize(b int64) string {
	m := b / mb
	if m >= 1024 && m%1024 == 0 {
		return fmt.Sprintf("%dGB", m/1024)
	}
	return fmt.Sprintf("%dMB", m)
}

// pgTuning returns the settings IvaraBase writes, as name -> value (already
// quoted where Postgres needs it), plus the memory figure they are based on.
func pgTuning(cfg *Config) (map[string]string, int64, string) {
	mem, src := memoryLimit()
	if mem <= 0 {
		mem, src = 1<<30, "unknown (assumed 1GB)"
	}
	maxConn := int64(200)
	if v, ok := cfg.Postgres.Settings["max_connections"]; ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
			maxConn = n
		}
	}
	shared := clamp64(mem/5, 128*mb, 8192*mb)
	out := map[string]string{
		"max_connections":              strconv.FormatInt(maxConn, 10),
		"shared_buffers":               pgSize(shared),
		"effective_cache_size":         pgSize(clamp64(mem*6/10, 256*mb, 64*1024*mb)),
		"maintenance_work_mem":         pgSize(clamp64(mem/20, 64*mb, 1024*mb)),
		"work_mem":                     pgSize(clamp64((mem-shared)/(maxConn*3), 4*mb, 64*mb)),
		"max_wal_size":                 "1GB",
		"min_wal_size":                 "80MB",
		"checkpoint_completion_target": "0.9",
		"random_page_cost":             "1.1",
		"wal_compression":              "zstd", // smaller WAL than "on" (pglz) at the same or lower CPU cost
		"default_toast_compression":    "lz4",  // faster and usually smaller than pglz for large text/jsonb values
	}
	if runtime.GOOS == "linux" {
		out["effective_io_concurrency"] = "200" // needs posix_fadvise (not on macOS)
	}
	for k, v := range cfg.Postgres.Settings {
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out, mem, src
}

var pgIdent = regexp.MustCompile(`^[a-z_][a-z0-9_.]*$`)

// owned are written elsewhere in writeConf; user settings can't change them.
var pgOwned = map[string]bool{"port": true, "listen_addresses": true, "unix_socket_directories": true,
	"ssl": true, "ssl_cert_file": true, "ssl_key_file": true, "hba_file": true, "data_directory": true}

// pgConfLines renders tuning as postgresql.conf lines (sorted, validated).
func pgConfLines(settings map[string]string) (string, error) {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		if !pgIdent.MatchString(k) {
			return "", fmt.Errorf("postgres setting %q is not a valid name", k)
		}
		if pgOwned[k] {
			return "", fmt.Errorf("postgres setting %q is managed by ivarabase (use the postgres.* config fields)", k)
		}
		v := settings[k]
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("postgres setting %s: value contains a newline", k)
		}
		if isBareConfValue(v) {
			fmt.Fprintf(&sb, "%s = %s\n", k, v)
		} else {
			fmt.Fprintf(&sb, "%s = '%s'\n", k, strings.ReplaceAll(strings.Trim(v, "'"), "'", "''"))
		}
	}
	return sb.String(), nil
}

var bareConf = regexp.MustCompile(`^-?[0-9.]+$|^(on|off|true|false)$`)

func isBareConfValue(v string) bool { return bareConf.MatchString(v) }
