package core

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Postgres is either a cluster this process manages (initdb + pg_ctl on a
// local data directory) or an external cluster reached through a superuser DSN.
type Postgres struct {
	cfg      *Config
	BinDir   string
	Major    int
	Version  string
	External bool

	Host     string
	Port     int
	User     string
	Password string
	SSLMode  string

	mu    sync.Mutex
	pools map[string]*sql.DB // superuser pools keyed by database

	// Owned is true when this process started the cluster (or is the server).
	// Short-lived CLI commands never stop a cluster the server is using.
	Owned bool
}

const superUser = "ivarabase"

var pgVersionRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// pgBinCandidates lists directories that may hold Postgres client+server tools
// on macOS (Homebrew, Postgres.app) and Linux (Debian/Ubuntu, RHEL).
func pgBinCandidates(pref string) []string {
	var dirs []string
	if pref != "" {
		dirs = append(dirs, pref)
	}
	globs := []string{
		"/opt/homebrew/opt/postgresql@*/bin",
		"/opt/homebrew/opt/postgresql/bin",
		"/usr/local/opt/postgresql@*/bin",
		"/usr/local/opt/postgresql/bin",
		"/Applications/Postgres.app/Contents/Versions/*/bin",
		"/usr/lib/postgresql/*/bin",
		"/usr/pgsql-*/bin",
	}
	for _, g := range globs {
		m, _ := filepath.Glob(g)
		dirs = append(dirs, m...)
	}
	if p, err := exec.LookPath("pg_ctl"); err == nil {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			dirs = append(dirs, filepath.Dir(r))
		}
		dirs = append(dirs, filepath.Dir(p))
	}
	return dirs
}

func pgToolMajor(dir, tool string) (int, string, error) {
	out, err := exec.Command(filepath.Join(dir, tool), "--version").Output()
	if err != nil {
		return 0, "", err
	}
	m := pgVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return 0, "", fmt.Errorf("cannot parse %q", out)
	}
	major, _ := strconv.Atoi(m[1])
	ver := m[1]
	if m[2] != "" {
		ver += "." + m[2]
	}
	return major, ver, nil
}

// findPgBin picks the newest install that has every tool we need. When
// wantMajor > 0 (an existing data directory) only that major is accepted.
func findPgBin(pref string, wantMajor int, needServer bool) (string, int, string, error) {
	tools := []string{"pg_dump", "pg_restore", "psql"}
	if needServer {
		tools = append(tools, "pg_ctl", "initdb", "postgres")
	}
	type cand struct {
		dir   string
		major int
		ver   string
	}
	var found []cand
	seen := map[string]bool{}
	for _, d := range pgBinCandidates(pref) {
		if seen[d] {
			continue
		}
		seen[d] = true
		ok := true
		for _, t := range tools {
			if !fileExists(filepath.Join(d, t)) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		tool := "pg_dump"
		if needServer {
			tool = "postgres"
		}
		major, ver, err := pgToolMajor(d, tool)
		if err != nil {
			continue
		}
		if wantMajor > 0 && major != wantMajor {
			continue
		}
		found = append(found, cand{d, major, ver})
	}
	if len(found) == 0 {
		hint := "install PostgreSQL 14 or newer"
		if runtime.GOOS == "darwin" {
			hint = "brew install postgresql@17"
		} else {
			hint = "apt install postgresql (or set postgres.bin_dir in config.json)"
		}
		if wantMajor > 0 {
			if InContainer() {
				return "", 0, "", fmt.Errorf("the data volume was created by PostgreSQL %d, but this image has a different PostgreSQL major version. "+
					"Use the image tag for PostgreSQL %d (ivarabase:<version>-pg%d), or move to the new major: take backups with the old image "+
					"(Backups > Back up now, or `ivarabase backups create`), start the new image on a fresh volume and restore them "+
					"(`ivarabase dr restore --from <folder>`); backups are pg_dump bundles and restore across major versions", wantMajor, wantMajor, wantMajor)
			}
			return "", 0, "", fmt.Errorf("the data directory was created by PostgreSQL %d but no PostgreSQL %d binaries were found (%s)", wantMajor, wantMajor, hint)
		}
		return "", 0, "", fmt.Errorf("no PostgreSQL binaries found: %s", hint)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].major > found[j].major })
	return found[0].dir, found[0].major, found[0].ver, nil
}

func (p *Postgres) dataDir() string { return p.cfg.Path("pg") }

func (p *Postgres) bin(tool string) string { return filepath.Join(p.BinDir, tool) }

// cmd runs a server-side tool with a fixed C locale: on macOS the postmaster
// refuses to start ("became multithreaded during startup") when LC_ALL is
// unset or invalid, which is common in non-login shells and launchd.
func (p *Postgres) cmd(tool string, args ...string) *exec.Cmd {
	c := exec.Command(p.bin(tool), args...)
	env := []string{"LC_ALL=C", "LANG=C"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LC_") && !strings.HasPrefix(kv, "LANG=") {
			env = append(env, kv)
		}
	}
	c.Env = env
	return c
}

// socketDir keeps the Unix socket path under the 103-byte macOS limit: deep
// data directories fall back to a short per-install directory in /tmp.
func (p *Postgres) socketDir() string {
	dir := p.cfg.Path("run")
	if len(dir)+len("/.s.PGSQL.65535") <= 100 {
		return dir
	}
	short := filepath.Join("/tmp", "ivarabase-"+sha256Hex(p.cfg.DataDir)[:10])
	os.MkdirAll(short, 0o700)
	return short
}

// OpenPostgres prepares (but does not start) the cluster.
func OpenPostgres(cfg *Config) (*Postgres, error) {
	p := &Postgres{cfg: cfg, pools: map[string]*sql.DB{}}
	if cfg.Postgres.ExternalDSN != "" {
		u, err := url.Parse(cfg.Postgres.ExternalDSN)
		if err != nil {
			return nil, fmt.Errorf("postgres.external_dsn: %w", err)
		}
		p.External = true
		p.Host = u.Hostname()
		p.Port, _ = strconv.Atoi(u.Port())
		if p.Port == 0 {
			p.Port = 5432
		}
		p.User = u.User.Username()
		p.Password, _ = u.User.Password()
		p.SSLMode = u.Query().Get("sslmode")
		if p.SSLMode == "" {
			p.SSLMode = "prefer"
		}
		dir, major, ver, err := findPgBin(cfg.Postgres.BinDir, 0, false)
		if err != nil {
			return nil, err
		}
		p.BinDir, p.Major, p.Version = dir, major, ver
		return p, nil
	}
	want := 0
	if b, err := os.ReadFile(filepath.Join(p.dataDir(), "PG_VERSION")); err == nil {
		want, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	dir, major, ver, err := findPgBin(cfg.Postgres.BinDir, want, true)
	if err != nil {
		return nil, err
	}
	p.BinDir, p.Major, p.Version = dir, major, ver
	p.Host = "127.0.0.1"
	p.Port = cfg.Postgres.Port
	p.User = superUser
	p.SSLMode = "disable"
	pw, err := os.ReadFile(cfg.Path("secrets", "pg_superuser"))
	if err == nil {
		p.Password = strings.TrimSpace(string(pw))
	}
	return p, nil
}

// Initialized reports whether the managed data directory exists.
func (p *Postgres) Initialized() bool {
	return p.External || fileExists(filepath.Join(p.dataDir(), "PG_VERSION"))
}

// InitDB creates the managed cluster.
func (p *Postgres) InitDB() error {
	if p.External || p.Initialized() {
		return nil
	}
	p.Password = randHex(24)
	if err := writeFileAtomic(p.cfg.Path("secrets", "pg_superuser"), []byte(p.Password+"\n"), 0o600); err != nil {
		return err
	}
	pwfile := p.cfg.Path("tmp", "initdb-pw")
	if err := os.WriteFile(pwfile, []byte(p.Password+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(pwfile)
	cmd := p.cmd("initdb", "-D", p.dataDir(), "-U", superUser, "--pwfile", pwfile,
		"-A", "scram-sha-256", "-E", "UTF8", "--locale=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("initdb: %v\n%s", err, tail(string(out), 1200))
	}
	return nil
}

// writeConf (re)writes the settings we own on every start, so changing
// config.json and restarting is enough.
func (p *Postgres) writeConf() error {
	conf := fmt.Sprintf(`# managed by ivarabase - rewritten on every start
port = %d
listen_addresses = '%s'
unix_socket_directories = '%s'
wal_level = replica
max_wal_senders = 4
password_encryption = 'scram-sha-256'
log_min_messages = warning
log_line_prefix = '%%m [%%p] %%d %%u '
`, p.cfg.Postgres.Port, p.cfg.Postgres.Listen, p.socketDir())
	tune, mem, src := pgTuning(p.cfg)
	p.preloadLibraries(tune)
	lines, err := pgConfLines(tune)
	if err != nil {
		return err
	}
	conf += fmt.Sprintf("# tuning for %s of memory (%s); override with postgres.settings or IVARA_PG_*\n", pgSize(mem), src) + lines
	if p.tlsReady() {
		crt, key := p.tlsFiles()
		conf += fmt.Sprintf("ssl = on\nssl_cert_file = '%s'\nssl_key_file = '%s'\nssl_min_protocol_version = 'TLSv1.2'\n", crt, key)
	}
	if err := os.WriteFile(filepath.Join(p.dataDir(), "ivarabase.conf"), []byte(conf), 0o600); err != nil {
		return err
	}
	main := filepath.Join(p.dataDir(), "postgresql.conf")
	b, err := os.ReadFile(main)
	if err != nil {
		return err
	}
	if !bytes.Contains(b, []byte("include_if_exists = 'ivarabase.conf'")) {
		b = append(b, []byte("\ninclude_if_exists = 'ivarabase.conf'\n")...)
		if err := os.WriteFile(main, b, 0o600); err != nil {
			return err
		}
	}
	hba := "# managed by ivarabase\nlocal all all scram-sha-256\nhost all all 127.0.0.1/32 scram-sha-256\nhost all all ::1/128 scram-sha-256\n"
	if l := p.cfg.Postgres.Listen; l != "127.0.0.1" && l != "localhost" && l != "::1" {
		// remote clients must use TLS once a certificate is installed
		kind := "host"
		if p.tlsReady() {
			kind = "hostssl"
		}
		hba += kind + " all all 0.0.0.0/0 scram-sha-256\n" + kind + " all all ::/0 scram-sha-256\n"
	}
	return os.WriteFile(filepath.Join(p.dataDir(), "pg_hba.conf"), []byte(hba), 0o600)
}

func (p *Postgres) running() bool {
	if p.External {
		return true
	}
	return p.cmd("pg_ctl", "status", "-D", p.dataDir()).Run() == nil
}

// Start boots the managed cluster (no-op when already running or external).
func (p *Postgres) Start() error {
	if p.External {
		return p.Ping()
	}
	if err := p.writeConf(); err != nil {
		return err
	}
	if p.running() {
		// a config rewrite only needs a reload
		p.cmd("pg_ctl", "reload", "-D", p.dataDir()).Run()
		return p.waitReady(15 * time.Second)
	}
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port), time.Second); err == nil {
		c.Close()
		return fmt.Errorf("port %d is already in use by another process; set postgres.port in %s", p.Port, p.cfg.configFile())
	}
	cmd := p.cmd("pg_ctl", "-D", p.dataDir(), "-l", p.cfg.Path("logs", "postgres.log"), "-w", "-t", "60", "start")
	out, err := cmd.CombinedOutput()
	if err == nil {
		p.Owned = true
	}
	if err != nil {
		logTail, _ := os.ReadFile(p.cfg.Path("logs", "postgres.log"))
		return fmt.Errorf("pg_ctl start: %v\n%s\n%s", err, tail(string(out), 600), tail(string(logTail), 1200))
	}
	return p.waitReady(30 * time.Second)
}

func (p *Postgres) Stop() {
	p.mu.Lock()
	for _, db := range p.pools {
		db.Close()
	}
	p.pools = map[string]*sql.DB{}
	p.mu.Unlock()
	if p.External || !p.Owned || !p.running() {
		return
	}
	p.cmd("pg_ctl", "-D", p.dataDir(), "-m", "fast", "-w", "stop").Run()
}

func (p *Postgres) waitReady(d time.Duration) error {
	deadline := time.Now().Add(d)
	var err error
	for time.Now().Before(deadline) {
		if err = p.Ping(); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("postgres did not become ready: %w", err)
}

func (p *Postgres) Ping() error {
	db, err := p.Super("postgres")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// Watch restarts a managed cluster that died. Runs until ctx is done.
func (p *Postgres) Watch(ctx context.Context) {
	if p.External {
		return
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.Ping() == nil {
				continue
			}
			if p.running() {
				continue // up but busy; don't fight it
			}
			log.Printf("postgres is down; restarting")
			if err := p.Start(); err != nil {
				log.Printf("postgres restart failed: %v", err)
			}
		}
	}
}

func (p *Postgres) dsn(user, password, db string) string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=5",
		p.Host, p.Port, quoteDSN(user), quoteDSN(password), quoteDSN(db), p.SSLMode)
}

func quoteDSN(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// Super returns a pooled superuser connection to a database.
func (p *Postgres) Super(db string) (*sql.DB, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.pools[db]; ok {
		return c, nil
	}
	c, err := sql.Open("postgres", p.dsn(p.User, p.Password, db))
	if err != nil {
		return nil, err
	}
	c.SetMaxOpenConns(8)
	c.SetMaxIdleConns(2)
	c.SetConnMaxIdleTime(2 * time.Minute)
	p.pools[db] = c
	return c, nil
}

// ClosePool drops a cached pool (before DROP DATABASE / RENAME).
func (p *Postgres) ClosePool(db string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.pools[db]; ok {
		c.Close()
		delete(p.pools, db)
	}
}

// Open opens a non-pooled connection as an arbitrary role.
func (p *Postgres) Open(user, password, db string) (*sql.DB, error) {
	return sql.Open("postgres", p.dsn(user, password, db))
}

// toolEnv is the environment for pg_dump / pg_restore / psql as superuser.
func (p *Postgres) toolEnv(db string) []string {
	env := []string{
		"PGHOST=" + p.Host,
		"PGPORT=" + strconv.Itoa(p.Port),
		"PGUSER=" + p.User,
		"PGPASSWORD=" + p.Password,
		"PGSSLMODE=" + p.SSLMode,
		"PGDATABASE=" + db,
		"PGCONNECT_TIMEOUT=10",
	}
	for _, k := range []string{"PATH", "HOME", "LANG", "TMPDIR"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (p *Postgres) terminate(db string) {
	if c, err := p.Super("postgres"); err == nil {
		c.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, db)
	}
}

func (p *Postgres) databaseExists(db string) bool {
	c, err := p.Super("postgres")
	if err != nil {
		return false
	}
	var ok bool
	c.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, db).Scan(&ok)
	return ok
}

func (p *Postgres) roleExists(role string) bool {
	c, err := p.Super("postgres")
	if err != nil {
		return false
	}
	var ok bool
	c.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&ok)
	return ok
}

func (p *Postgres) dropDatabase(db string) error {
	p.ClosePool(db)
	p.terminate(db)
	c, err := p.Super("postgres")
	if err != nil {
		return err
	}
	q := `DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(db)
	if p.Major >= 13 {
		q += ` WITH (FORCE)`
	}
	_, err = c.Exec(q)
	return err
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

var errNotRunning = errors.New("postgres is not running")
