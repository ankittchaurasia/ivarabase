package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lib/pq"
)

// Monitoring: a sampler records one row per metric per minute into
// metrics_samples (kept 7 days). Scopes: "host", "pg" and "p:<slug>".

var projectServices = []string{"rest", "auth", "storage", "functions"}

// ---------------------------------------------------------------- request counters

type reqStat struct {
	Count, Errors int64
	TotalMs       float64
}

type metrics struct {
	mu  sync.Mutex
	cur map[string]*reqStat // slug|service
}

func newMetrics() *metrics { return &metrics{cur: map[string]*reqStat{}} }

// count is kept for older call sites; the gateway records everything via observe.
func (m *metrics) count(slug, service string) {}

func (m *metrics) observe(slug, service string, status int, d time.Duration) {
	known := false
	for _, s := range projectServices {
		if s == service {
			known = true
		}
	}
	if !known || slug == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := slug + "|" + service
	st := m.cur[k]
	if st == nil {
		st = &reqStat{}
		m.cur[k] = st
	}
	st.Count++
	if status >= 500 {
		st.Errors++
	}
	st.TotalMs += float64(d.Microseconds()) / 1000
}

func (m *metrics) drain() map[string]reqStat {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]reqStat{}
	for k, v := range m.cur {
		out[k] = *v
	}
	m.cur = map[string]*reqStat{}
	return out
}

// ---------------------------------------------------------------- sampler

type sample struct {
	scope, name string
	value       float64
}

type sampler struct {
	prevCG    [2]float64 // cgroup CPU usage (µs) and wall clock (µs)
	prevCPU   [2]float64 // busy, total (Linux)
	prevXact  map[string]float64
	prevBlks  [2]float64 // hit, read
	lastClean time.Time
}

func (a *App) monitorLoop(ctx context.Context) {
	s := &sampler{prevXact: map[string]float64{}}
	s.collect(a, true) // prime deltas
	for {
		now := time.Now()
		wait := now.Truncate(time.Minute).Add(time.Minute).Sub(now) + 500*time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		ts := time.Now().UTC().Truncate(time.Minute)
		samples := s.collect(a, false)
		if err := a.storeSamples(ts, samples); err != nil {
			log.Printf("monitor: store samples: %v", err)
		}
		a.evaluateAlerts(ctx, ts)
		if time.Since(s.lastClean) > time.Hour {
			s.lastClean = time.Now()
			a.Meta.Exec(`DELETE FROM metrics_samples WHERE ts < now() - interval '7 days'`)
			a.Meta.Exec(`DELETE FROM alert_events WHERE at < now() - interval '90 days'`)
		}
	}
}

func (a *App) storeSamples(ts time.Time, ss []sample) error {
	if len(ss) == 0 {
		return nil
	}
	scopes, names, values := make([]string, len(ss)), make([]string, len(ss)), make([]float64, len(ss))
	for i, s := range ss {
		scopes[i], names[i], values[i] = s.scope, s.name, s.value
	}
	_, err := a.Meta.Exec(`INSERT INTO metrics_samples(ts, scope, name, value)
		SELECT $1, s, n, v FROM unnest($2::text[], $3::text[], $4::float8[]) AS t(s, n, v)`,
		ts, pq.Array(scopes), pq.Array(names), pq.Array(values))
	return err
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func (s *sampler) collect(a *App, prime bool) []sample {
	var out []sample
	add := func(scope, name string, v float64) {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			out = append(out, sample{scope, name, round2(v)})
		}
	}

	// ---- host
	if cpu, ok := s.cpuPercent(); ok && !prime {
		add("host", "cpu_pct", cpu)
	}
	if l1, ok := loadAvg(); ok {
		add("host", "load1", l1)
		add("host", "load_per_cpu", l1/effectiveCPUs())
	}
	if total, used, ok := memUsage(); ok && total > 0 {
		add("host", "mem_used_pct", 100*used/total)
		add("host", "mem_used_mb", used/1e6)
		add("host", "mem_total_mb", total/1e6)
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(a.Cfg.DataDir, &fs) == nil {
		total := float64(fs.Blocks) * float64(fs.Bsize)
		free := float64(fs.Bavail) * float64(fs.Bsize)
		if total > 0 {
			add("host", "disk_used_pct", 100*(total-free)/total)
			add("host", "disk_free_gb", free/1e9)
		}
	}
	for comp, mb := range processRSS() {
		add("host", "rss_"+comp+"_mb", mb)
	}
	if !prime {
		errs := 0
		for _, c := range a.health()["checks"].([]healthCheck) {
			if c.Status == "error" {
				errs++
			}
		}
		add("host", "health_errors", float64(errs))
	}

	// ---- postgres
	if sc, err := a.PG.Super("postgres"); err == nil {
		var conns, maxConns float64
		sc.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend'`).Scan(&conns)
		sc.QueryRow(`SELECT setting::float8 FROM pg_settings WHERE name='max_connections'`).Scan(&maxConns)
		add("pg", "connections", conns)
		if maxConns > 0 {
			add("pg", "connections_pct", 100*conns/maxConns)
		}
		rows, err := sc.Query(`SELECT d.datname, (s.xact_commit + s.xact_rollback)::float8, s.blks_hit::float8, s.blks_read::float8,
			pg_database_size(d.oid)::float8, s.numbackends::float8
			FROM pg_database d JOIN pg_stat_database s ON s.datid=d.oid WHERE NOT d.datistemplate`)
		if err == nil {
			projects := map[string]bool{}
			if pr, err := a.Meta.Query(`SELECT slug FROM projects`); err == nil {
				for pr.Next() {
					var sl string
					pr.Scan(&sl)
					projects[sl] = true
				}
				pr.Close()
			}
			var totalTPS, hit, read, totalSize float64
			for rows.Next() {
				var name string
				var xact, h, r, size, backends float64
				rows.Scan(&name, &xact, &h, &r, &size, &backends)
				hit += h
				read += r
				totalSize += size
				prev, seen := s.prevXact[name]
				s.prevXact[name] = xact
				tps := 0.0
				if seen && xact >= prev {
					tps = (xact - prev) / 60
				}
				totalTPS += tps
				if projects[name] {
					scope := "p:" + name
					add(scope, "db_size_mb", size/1e6)
					add(scope, "connections", backends)
					if !prime {
						add(scope, "tps", tps)
					}
				}
			}
			rows.Close()
			add("pg", "db_total_mb", totalSize/1e6)
			if !prime {
				add("pg", "tps", totalTPS)
				dh, dr := hit-s.prevBlks[0], read-s.prevBlks[1]
				if dh+dr > 0 {
					add("pg", "cache_hit_pct", 100*dh/(dh+dr))
				}
			}
			s.prevBlks = [2]float64{hit, read}
		}
	}

	// ---- per-project requests (only non-zero rows are stored; queries fill gaps with 0)
	if !prime {
		bySlug := map[string]*[3]float64{} // count, errors, totalMs
		for k, st := range a.metrics.drain() {
			slug, svc, _ := strings.Cut(k, "|")
			scope := "p:" + slug
			add(scope, "req_"+svc, float64(st.Count))
			if st.Errors > 0 {
				add(scope, "err_"+svc, float64(st.Errors))
			}
			if st.Count > 0 {
				add(scope, "lat_"+svc, st.TotalMs/float64(st.Count))
			}
			t := bySlug[slug]
			if t == nil {
				t = &[3]float64{}
				bySlug[slug] = t
			}
			t[0] += float64(st.Count)
			t[1] += float64(st.Errors)
			t[2] += st.TotalMs
		}
		for slug, t := range bySlug {
			scope := "p:" + slug
			add(scope, "req_total", t[0])
			if t[1] > 0 {
				add(scope, "err_total", t[1])
			}
			if t[0] > 0 {
				add(scope, "lat_avg", t[2]/t[0])
			}
		}
	} else {
		a.metrics.drain()
	}
	return out
}

// ---------------------------------------------------------------- host readings (no cgo)

func (s *sampler) cpuPercent() (float64, bool) {
	if v, ok := s.cgroupCPUPercent(); ok || cgroupCPUs() > 0 {
		return v, ok
	}
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/stat")
		if err != nil {
			return 0, false
		}
		line := strings.SplitN(string(b), "\n", 2)[0]
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			return 0, false
		}
		var total, idle float64
		for i, v := range f[1:] {
			n, _ := strconv.ParseFloat(v, 64)
			total += n
			if i == 3 || i == 4 { // idle + iowait
				idle += n
			}
		}
		busy := total - idle
		prev := s.prevCPU
		s.prevCPU = [2]float64{busy, total}
		if prev[1] == 0 || total <= prev[1] {
			return 0, false
		}
		return 100 * (busy - prev[0]) / (total - prev[1]), true
	case "darwin":
		out, err := exec.Command("top", "-l", "2", "-n", "0", "-s", "1").Output()
		if err != nil {
			return 0, false
		}
		var idle float64 = -1
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			l := sc.Text()
			if strings.HasPrefix(l, "CPU usage:") {
				for _, part := range strings.Split(strings.TrimPrefix(l, "CPU usage:"), ",") {
					part = strings.TrimSpace(part)
					if strings.HasSuffix(part, "idle") {
						idle, _ = strconv.ParseFloat(strings.TrimSuffix(strings.Fields(part)[0], "%"), 64)
					}
				}
			}
		}
		if idle < 0 {
			return 0, false
		}
		return 100 - idle, true // the last "CPU usage" line is the 1-second sample
	}
	return 0, false
}

func loadAvg() (float64, bool) {
	var raw string
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return 0, false
		}
		raw = string(b)
	case "darwin":
		b, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0, false
		}
		raw = strings.Trim(strings.TrimSpace(string(b)), "{} ")
	default:
		return 0, false
	}
	f := strings.Fields(raw)
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

// memUsage returns total and used bytes (used excludes reclaimable cache).
func memUsage() (float64, float64, bool) {
	if t, u, ok := cgroupMem(); ok {
		return t, u, true
	}
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, 0, false
		}
		vals := map[string]float64{}
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				v, _ := strconv.ParseFloat(f[1], 64)
				vals[strings.TrimSuffix(f[0], ":")] = v * 1024
			}
		}
		return vals["MemTotal"], vals["MemTotal"] - vals["MemAvailable"], vals["MemTotal"] > 0
	case "darwin":
		tb, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, 0, false
		}
		total, _ := strconv.ParseFloat(strings.TrimSpace(string(tb)), 64)
		vb, err := exec.Command("vm_stat").Output()
		if err != nil {
			return 0, 0, false
		}
		page := 4096.0
		var used float64
		for _, l := range strings.Split(string(vb), "\n") {
			if strings.Contains(l, "page size of") {
				for _, f := range strings.Fields(l) {
					if n, err := strconv.ParseFloat(f, 64); err == nil {
						page = n
					}
				}
				continue
			}
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			n, _ := strconv.ParseFloat(strings.Trim(strings.TrimSpace(v), "."), 64)
			switch strings.TrimSpace(k) {
			case "Pages active", "Pages wired down", "Pages occupied by compressor":
				used += n
			}
		}
		return total, used * page, total > 0
	}
	return 0, 0, false
}

// processRSS sums resident memory (MB) of the processes IvaraBase runs.
func processRSS() map[string]float64 {
	out := map[string]float64{"postgres": 0, "postgrest": 0, "deno": 0, "ivarabase": 0}
	b, err := exec.Command("ps", "-A", "-o", "pid=,rss=,comm=").Output()
	if err != nil {
		return out
	}
	self := os.Getpid()
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		kb, _ := strconv.ParseFloat(f[1], 64)
		name := filepath.Base(strings.Join(f[2:], " "))
		mb := kb / 1024
		switch {
		case pid == self:
			out["ivarabase"] += mb
		case name == "postgrest":
			out["postgrest"] += mb
		case strings.HasPrefix(name, "postgres"):
			out["postgres"] += mb
		case name == "deno":
			out["deno"] += mb
		}
	}
	return out
}

// ---------------------------------------------------------------- series queries

var rangeBuckets = map[string][2]string{ // range -> {interval, bucket}
	"1h": {"1 hour", "1 minute"}, "6h": {"6 hours", "5 minutes"}, "24h": {"24 hours", "15 minutes"}, "7d": {"7 days", "2 hours"},
}

func bucketMinutes(b string) float64 {
	n, _ := strconv.ParseFloat(strings.Fields(b)[0], 64)
	if strings.Contains(b, "hour") {
		n *= 60
	}
	return n
}

// isCounter: request/error counts are summed per bucket and shown per minute;
// everything else is a gauge (averaged).
func isCounter(name string) bool {
	return strings.HasPrefix(name, "req_") || strings.HasPrefix(name, "err_")
}

func (a *App) series(ctx context.Context, scope string, names []string, rng string) (map[string]any, error) {
	rb, ok := rangeBuckets[rng]
	if !ok {
		return nil, errBad("invalid_parameter", "range must be 1h, 6h, 24h or 7d", "")
	}
	perMin := bucketMinutes(rb[1])
	out := map[string]any{"range": rng, "bucket_minutes": perMin, "scope": scope}
	series := map[string]any{}
	for _, n := range names {
		agg := "avg(m.value)"
		if isCounter(n) {
			agg = fmt.Sprintf("coalesce(sum(m.value),0) / %g", perMin)
		}
		rows, err := a.Meta.QueryContext(ctx, `
			WITH b AS (
				SELECT generate_series(date_bin($3::interval, now() - $4::interval, '2000-01-01'), now(), $3::interval) AS t
			)
			SELECT b.t, `+agg+` FROM b
			LEFT JOIN metrics_samples m ON m.scope=$1 AND m.name=$2 AND m.ts >= b.t AND m.ts < b.t + $3::interval
			GROUP BY b.t ORDER BY b.t`, scope, n, rb[1], rb[0])
		if err != nil {
			return nil, err
		}
		pts := [][2]any{}
		for rows.Next() {
			var t time.Time
			var v *float64
			rows.Scan(&t, &v)
			if v != nil {
				pts = append(pts, [2]any{t.Unix() * 1000, round2(*v)})
			} else {
				pts = append(pts, [2]any{t.Unix() * 1000, nil})
			}
		}
		rows.Close()
		series[n] = pts
	}
	out["series"] = series
	return out, nil
}

func (a *App) latest(scope string) map[string]float64 {
	out := map[string]float64{}
	rows, err := a.Meta.Query(`SELECT DISTINCT ON (name) name, value FROM metrics_samples
		WHERE scope=$1 AND ts > now() - interval '10 minutes' ORDER BY name, ts DESC`, scope)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var v float64
		rows.Scan(&n, &v)
		out[n] = v
	}
	return out
}

// ---------------------------------------------------------------- alerts

type alertRule struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Scope      string  `json:"scope"` // host | pg | project
	Metric     string  `json:"metric"`
	Op         string  `json:"op"` // > or <
	Threshold  float64 `json:"threshold"`
	ForMinutes int     `json:"for_minutes"`
	Enabled    bool    `json:"enabled"`
	Builtin    bool    `json:"builtin"`
}

type alertChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // webhook | discord | slack
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

func (a *App) alertRules() ([]alertRule, error) {
	rows, err := a.Meta.Query(`SELECT id, name, scope, metric, op, threshold, for_minutes, enabled, builtin FROM alert_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alertRule
	for rows.Next() {
		var r alertRule
		rows.Scan(&r.ID, &r.Name, &r.Scope, &r.Metric, &r.Op, &r.Threshold, &r.ForMinutes, &r.Enabled, &r.Builtin)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (a *App) alertChannels() []alertChannel {
	var ch []alertChannel
	a.getSetting("alert_channels", &ch)
	return ch
}

// ruleValue evaluates one rule for one subject over its window.
func (a *App) ruleValue(ctx context.Context, r alertRule, subject string, ts time.Time) (float64, bool) {
	since := ts.Add(-time.Duration(max(r.ForMinutes, 1)) * time.Minute)
	if r.Metric == "error_rate_pct" {
		var req, errs float64
		a.Meta.QueryRowContext(ctx, `SELECT coalesce(sum(value) FILTER (WHERE name='req_total'),0), coalesce(sum(value) FILTER (WHERE name='err_total'),0)
			FROM metrics_samples WHERE scope=$1 AND ts > $2 AND ts <= $3`, subject, since, ts).Scan(&req, &errs)
		if req < 20 { // too little traffic to judge
			return 0, false
		}
		return 100 * errs / req, true
	}
	var v *float64
	var n int
	a.Meta.QueryRowContext(ctx, `SELECT avg(value), count(*) FROM metrics_samples WHERE scope=$1 AND name=$2 AND ts > $3 AND ts <= $4`,
		subject, r.Metric, since, ts).Scan(&v, &n)
	if v == nil || n < max(1, r.ForMinutes/2) {
		return 0, false
	}
	return *v, true
}

func (a *App) evaluateAlerts(ctx context.Context, ts time.Time) {
	rules, err := a.alertRules()
	if err != nil {
		return
	}
	var projects []string
	if rows, err := a.Meta.Query(`SELECT slug FROM projects ORDER BY slug`); err == nil {
		for rows.Next() {
			var s string
			rows.Scan(&s)
			projects = append(projects, s)
		}
		rows.Close()
	}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		subjects := []string{r.Scope}
		if r.Scope == "project" {
			subjects = nil
			for _, p := range projects {
				subjects = append(subjects, "p:"+p)
			}
		}
		for _, subj := range subjects {
			v, ok := a.ruleValue(ctx, r, subj, ts)
			breach := ok && ((r.Op == ">" && v > r.Threshold) || (r.Op == "<" && v < r.Threshold))
			var firing bool
			a.Meta.QueryRow(`SELECT firing FROM alert_state WHERE rule_id=$1 AND subject=$2`, r.ID, subj).Scan(&firing)
			if breach == firing {
				if firing {
					a.Meta.Exec(`UPDATE alert_state SET value=$3 WHERE rule_id=$1 AND subject=$2`, r.ID, subj, v)
				}
				continue
			}
			state := "resolved"
			if breach {
				state = "firing"
			}
			a.Meta.Exec(`INSERT INTO alert_state(rule_id, subject, firing, since, value) VALUES ($1,$2,$3,now(),$4)
				ON CONFLICT (rule_id, subject) DO UPDATE SET firing=$3, since=now(), value=$4`, r.ID, subj, breach, v)
			msg := alertMessage(a, r, subj, v, state)
			a.Meta.Exec(`INSERT INTO alert_events(rule_id, rule_name, subject, state, value, message) VALUES ($1,$2,$3,$4,$5,$6)`,
				r.ID, r.Name, subj, state, v, msg)
			go a.notify(context.Background(), map[string]any{"event": "alert", "state": state, "rule": r.Name, "subject": strings.TrimPrefix(subj, "p:"),
				"metric": r.Metric, "value": round2(v), "threshold": r.Threshold, "message": msg, "at": ts}, msg)
		}
	}
}

func alertMessage(a *App, r alertRule, subj string, v float64, state string) string {
	where := a.Cfg.Domain
	if where == "" {
		where, _ = os.Hostname()
	}
	target := ""
	if strings.HasPrefix(subj, "p:") {
		target = " · project " + strings.TrimPrefix(subj, "p:")
	}
	head := "FIRING"
	if state == "resolved" {
		head = "RESOLVED"
	}
	return fmt.Sprintf("[%s] %s%s: %s = %.2f (%s %g over %dm) on %s", head, r.Name, target, r.Metric, v, r.Op, r.Threshold, r.ForMinutes, where)
}

// notify posts to every enabled channel. Returns per-channel results.
func (a *App) notify(ctx context.Context, payload map[string]any, text string) []map[string]any {
	var results []map[string]any
	for _, ch := range a.alertChannels() {
		if !ch.Enabled || ch.URL == "" {
			continue
		}
		var body []byte
		switch ch.Type {
		case "discord":
			body, _ = json.Marshal(map[string]any{"content": text})
		case "slack":
			body, _ = json.Marshal(map[string]any{"text": text})
		default:
			body, _ = json.Marshal(payload)
		}
		res := map[string]any{"channel": ch.Name, "ok": false}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, _ := http.NewRequestWithContext(cctx, "POST", ch.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "IvaraBase-Alerts/"+Version)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			res["error"] = err.Error()
		} else {
			resp.Body.Close()
			res["status"] = resp.StatusCode
			res["ok"] = resp.StatusCode < 300
		}
		results = append(results, res)
	}
	return results
}

func numCPU() int { return runtime.NumCPU() }
