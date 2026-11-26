package core

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

const metaDB = "ivarabase_meta"

// App is the whole platform: config, Postgres, meta store, operations,
// functions runtime and backup engine.
type App struct {
	Cfg         *Config
	PG          *Postgres
	Meta        *sql.DB
	masterKey   []byte
	recoveryKey string
	Ops         *Registry
	Fn          *FnRuntime
	Rest        *RestRuntime
	Hooks       *hookManager // database webhook dispatchers
	metrics     *metrics
	started     time.Time

	projMu    sync.Mutex
	projPools map[string]*sql.DB

	backupMu sync.Mutex // one backup/restore job at a time
}

// InitResult is printed once by `init` / first `serve`.
type InitResult struct {
	Fresh         bool
	OwnerUser     string
	OwnerPassword string
	RecoveryKey   string
}

// Open loads config and secrets and starts Postgres. When the data directory is
// new it initializes everything and returns the one-time credentials.
func Open(dataDir string, opts InitOptions) (*App, *InitResult, error) {
	cfg, err := LoadConfig(dataDir)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.ensureDirs(); err != nil {
		return nil, nil, err
	}
	if opts.AdminFromEnv {
		// LoadConfig has loaded <data>/ivarabase.env into the environment by now
		if opts.OwnerUser == "" {
			opts.OwnerUser = strings.TrimSpace(os.Getenv("IVARA_ADMIN_USER"))
		}
		if opts.OwnerPassword == "" {
			opts.OwnerPassword = os.Getenv("IVARA_ADMIN_PASSWORD")
			if opts.OwnerPassword == "" {
				opts.OwnerPassword = os.Getenv("IVARA_OWNER_PASSWORD")
			}
		}
		if opts.OwnerPassword != "" && len(opts.OwnerPassword) < 10 {
			return nil, nil, errors.New("IVARA_ADMIN_PASSWORD must be at least 10 characters")
		}
	}
	res := &InitResult{}

	// master key: encrypts secrets at rest in the meta store
	mk := cfg.Path("secrets", "master.key")
	if !fileExists(mk) {
		res.Fresh = true
		if err := writeFileAtomic(mk, []byte(randHex(32)+"\n"), 0o600); err != nil {
			return nil, nil, err
		}
	}
	mkb, err := os.ReadFile(mk)
	if err != nil {
		return nil, nil, err
	}
	master, err := hex.DecodeString(strings.TrimSpace(string(mkb)))
	if err != nil || len(master) != 32 {
		return nil, nil, fmt.Errorf("%s is corrupt", mk)
	}

	// recovery key: encrypts backups; the one thing the owner must keep elsewhere
	rk := cfg.Path("secrets", "recovery.key")
	if opts.RecoveryKey != "" {
		if err := writeFileAtomic(rk, []byte(opts.RecoveryKey+"\n"), 0o600); err != nil {
			return nil, nil, err
		}
	} else if !fileExists(rk) {
		k := newRecoveryKey()
		if err := writeFileAtomic(rk, []byte(k+"\n"), 0o600); err != nil {
			return nil, nil, err
		}
		res.RecoveryKey = k
	}
	rkb, err := os.ReadFile(rk)
	if err != nil {
		return nil, nil, err
	}

	if !fileExists(cfg.configFile()) || opts.Persist {
		if opts.Persist {
			applyListen(cfg, opts)
		}
		if err := cfg.Save(); err != nil {
			return nil, nil, err
		}
	}
	applyListen(cfg, opts) // command-line flags apply to this run only

	pg, err := OpenPostgres(cfg)
	if err != nil {
		return nil, nil, err
	}
	if !pg.Initialized() {
		log.Printf("initializing PostgreSQL %s cluster in %s", pg.Version, pg.dataDir())
		if err := pg.InitDB(); err != nil {
			return nil, nil, err
		}
	}
	log.Printf("starting PostgreSQL %s (%s) on port %d", pg.Version, pg.BinDir, pg.Port)
	if err := pg.Start(); err != nil {
		return nil, nil, err
	}

	a := &App{
		Cfg: cfg, PG: pg, masterKey: master, recoveryKey: strings.TrimSpace(string(rkb)),
		started: time.Now(), projPools: map[string]*sql.DB{},
	}
	if err := a.openMeta(); err != nil {
		pg.Stop()
		return nil, nil, err
	}
	if err := a.ensureClusterRoles(); err != nil {
		pg.Stop()
		return nil, nil, err
	}

	// owner account
	var n int
	a.Meta.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
	if n == 0 {
		user := opts.OwnerUser
		if user == "" {
			user = "admin"
		}
		pw := opts.OwnerPassword
		if pw == "" {
			pw = randHex(10)
		}
		if _, err := a.Meta.Exec(`INSERT INTO users(username, password_hash) VALUES ($1,$2)`, user, hashPassword(pw)); err != nil {
			return nil, nil, err
		}
		res.Fresh = true
		res.OwnerUser, res.OwnerPassword = user, pw
		if opts.AdminFromEnv && opts.OwnerPassword != "" {
			res.OwnerPassword = "(the IVARA_ADMIN_PASSWORD you set)"
		}
		if opts.OwnerPassword != "" {
			a.setSetting(envLoginKey, envLogin{User: user, Hash: hashPassword(opts.OwnerPassword)})
		}
	} else if opts.OwnerPassword != "" {
		if err := a.applyEnvLogin(opts.OwnerUser, opts.OwnerPassword); err != nil {
			pg.Stop()
			return nil, nil, err
		}
	}

	a.Ops = NewRegistry(a)
	a.Fn = newFnRuntime(a)
	a.Rest = newRestRuntime(a)
	a.Hooks = newHookManager(a)
	a.metrics = newMetrics()
	return a, res, nil
}

// applyListen overrides the listen address / public URL from flags.
func applyListen(cfg *Config, opts InitOptions) {
	if opts.Listen != "" {
		cfg.Listen = opts.Listen
		if opts.PublicURL == "" {
			cfg.PublicURL = "http://" + opts.Listen
		}
	}
	if opts.PublicURL != "" {
		cfg.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	}
	cfg.derivePublicURL()
}

type InitOptions struct {
	Persist       bool // init: store --listen / --public-url in config.json
	Listen        string
	PublicURL     string
	OwnerUser     string
	OwnerPassword string
	RecoveryKey   string // adopt an existing key (disaster recovery)
	// AdminFromEnv (serve) fills OwnerUser / OwnerPassword from
	// IVARA_ADMIN_USER / IVARA_ADMIN_PASSWORD, read after <data>/ivarabase.env
	// is loaded, and applies a changed value to an existing install.
	AdminFromEnv bool
}

func (a *App) Close() {
	if a.Fn != nil {
		a.Fn.StopAll()
	}
	if a.Rest != nil {
		a.Rest.StopAll()
	}
	a.projMu.Lock()
	for _, db := range a.projPools {
		db.Close()
	}
	a.projPools = map[string]*sql.DB{}
	a.projMu.Unlock()
	if a.Meta != nil {
		a.Meta.Close()
	}
	a.PG.Stop()
}

// UpgradeProjects brings every project up to the current API/auth schema.
// It is idempotent and runs at server start, so an upgraded binary upgrades
// existing projects.
func (a *App) UpgradeProjects() {
	rows, err := a.Meta.Query(`SELECT slug FROM projects ORDER BY slug`)
	if err != nil {
		return
	}
	var slugs []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		slugs = append(slugs, s)
	}
	rows.Close()
	for _, s := range slugs {
		if err := a.ensureProjectAPI(s); err != nil {
			log.Printf("project %s: upgrade failed: %v", s, err)
		}
	}
}

// Background runs the Postgres watchdog, backup scheduler and housekeeping.
func (a *App) Background(ctx context.Context) {
	go a.PG.Watch(ctx)
	go a.backupScheduler(ctx)
	go a.Fn.reaper(ctx)
	go a.Rest.reaper(ctx)
	go a.cronLoop(ctx)
	go a.monitorLoop(ctx)
	go a.tusSweeper(ctx)
	go a.Hooks.run(ctx)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.Meta.Exec(`DELETE FROM sessions WHERE expires_at < now()`)
				a.maintainLogPartitions() // log tables: drop expired daily partitions
			}
		}
	}()
}

// ---------------------------------------------------------------- meta store

var metaMigrations = []string{
	`CREATE TABLE users (
		id bigserial PRIMARY KEY,
		username text UNIQUE NOT NULL,
		password_hash text NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now())`,
	`CREATE TABLE sessions (
		id text PRIMARY KEY,
		user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at timestamptz NOT NULL DEFAULT now(),
		expires_at timestamptz NOT NULL,
		user_agent text NOT NULL DEFAULT '')`,
	`CREATE TABLE tokens (
		id bigserial PRIMARY KEY,
		name text NOT NULL,
		token_hash text UNIQUE NOT NULL,
		prefix text NOT NULL,
		level text NOT NULL,
		projects text[] NOT NULL DEFAULT '{}',
		allow_destructive boolean NOT NULL DEFAULT false,
		expires_at timestamptz,
		created_at timestamptz NOT NULL DEFAULT now(),
		last_used_at timestamptz,
		revoked_at timestamptz)`,
	`CREATE TABLE projects (
		slug text PRIMARY KEY,
		name text NOT NULL,
		notes text NOT NULL DEFAULT '',
		role_name text NOT NULL,
		password_enc text NOT NULL,
		status text NOT NULL DEFAULT 'active',
		created_at timestamptz NOT NULL DEFAULT now())`,
	`CREATE TABLE migrations (
		project text NOT NULL,
		version text NOT NULL,
		name text NOT NULL,
		sql text NOT NULL,
		checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now(),
		applied_by text NOT NULL DEFAULT '',
		PRIMARY KEY (project, version))`,
	`CREATE TABLE functions (
		project text NOT NULL,
		name text NOT NULL,
		code text NOT NULL,
		public boolean NOT NULL DEFAULT false,
		timeout_s int NOT NULL DEFAULT 30,
		memory_mb int NOT NULL DEFAULT 128,
		version int NOT NULL DEFAULT 1,
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, name))`,
	`CREATE TABLE function_logs (
		id bigserial PRIMARY KEY,
		project text NOT NULL,
		name text NOT NULL,
		at timestamptz NOT NULL DEFAULT now(),
		status int NOT NULL,
		ms int NOT NULL,
		error text NOT NULL DEFAULT '')`,
	`CREATE INDEX function_logs_p ON function_logs(project, at DESC)`,
	`CREATE TABLE secrets (
		project text NOT NULL,
		name text NOT NULL,
		value_enc text NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (project, name))`,
	`CREATE TABLE settings (key text PRIMARY KEY, value jsonb NOT NULL)`,
	`CREATE TABLE audit (
		id bigserial PRIMARY KEY,
		at timestamptz NOT NULL DEFAULT now(),
		actor text NOT NULL,
		op text NOT NULL,
		project text NOT NULL DEFAULT '',
		detail jsonb NOT NULL DEFAULT '{}',
		ok boolean NOT NULL,
		error text NOT NULL DEFAULT '')`,
	`CREATE INDEX audit_at ON audit(at DESC)`,
	`CREATE TABLE approvals (
		id text PRIMARY KEY,
		created_at timestamptz NOT NULL DEFAULT now(),
		actor text NOT NULL,
		token_id bigint,
		op text NOT NULL,
		project text NOT NULL DEFAULT '',
		args jsonb NOT NULL,
		preview jsonb,
		status text NOT NULL DEFAULT 'pending',
		decided_at timestamptz,
		decided_by text,
		result jsonb,
		error text NOT NULL DEFAULT '')`,
	`CREATE TABLE backups (
		id text PRIMARY KEY,
		project text NOT NULL,
		project_name text NOT NULL DEFAULT '',
		created_at timestamptz NOT NULL,
		kind text NOT NULL,
		size bigint NOT NULL,
		sha256 text NOT NULL,
		pg_version text NOT NULL DEFAULT '',
		tables int NOT NULL DEFAULT 0,
		local boolean NOT NULL DEFAULT true,
		remote boolean NOT NULL DEFAULT false,
		verified_at timestamptz,
		verify_ok boolean,
		verify_detail text NOT NULL DEFAULT '')`,
	`CREATE INDEX backups_p ON backups(project, created_at DESC)`,
	`ALTER TABLE projects ADD COLUMN jwt_secret_enc text NOT NULL DEFAULT '', ADD COLUMN authenticator_pw_enc text NOT NULL DEFAULT ''`,
	`CREATE TABLE auth_config (project text PRIMARY KEY, config_enc text NOT NULL)`,
	`CREATE TABLE auth_mails (
		id bigserial PRIMARY KEY,
		project text NOT NULL,
		at timestamptz NOT NULL DEFAULT now(),
		to_addr text NOT NULL,
		subject text NOT NULL,
		body text NOT NULL,
		kind text NOT NULL,
		delivered boolean NOT NULL,
		error text NOT NULL DEFAULT '',
		link text NOT NULL DEFAULT '')`,
	`CREATE INDEX auth_mails_p ON auth_mails(project, at DESC)`,
	`ALTER TABLE functions ADD COLUMN files jsonb NOT NULL DEFAULT '{}', ADD COLUMN entrypoint text NOT NULL DEFAULT 'index.ts',
		ADD COLUMN verify_jwt boolean NOT NULL DEFAULT true, ADD COLUMN schedule text NOT NULL DEFAULT '', ADD COLUMN last_run_at timestamptz;
	 UPDATE functions SET verify_jwt = NOT public, files = jsonb_build_object('index.ts', code)`,
	`CREATE TABLE function_console (
		id bigserial PRIMARY KEY,
		project text NOT NULL,
		name text NOT NULL,
		at timestamptz NOT NULL,
		stream text NOT NULL,
		line text NOT NULL);
	 CREATE INDEX function_console_p ON function_console(project, name, id DESC)`,
	`CREATE TABLE metrics_samples (ts timestamptz NOT NULL, scope text NOT NULL, name text NOT NULL, value double precision NOT NULL);
	 CREATE INDEX metrics_samples_i ON metrics_samples(scope, name, ts);
	 CREATE INDEX metrics_samples_ts ON metrics_samples(ts);
	 CREATE TABLE alert_rules (id bigserial PRIMARY KEY, name text NOT NULL, scope text NOT NULL, metric text NOT NULL, op text NOT NULL DEFAULT '>',
		threshold double precision NOT NULL, for_minutes int NOT NULL DEFAULT 5, enabled boolean NOT NULL DEFAULT true, builtin boolean NOT NULL DEFAULT false);
	 CREATE TABLE alert_state (rule_id bigint NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE, subject text NOT NULL, firing boolean NOT NULL,
		since timestamptz NOT NULL, value double precision, PRIMARY KEY (rule_id, subject));
	 CREATE TABLE alert_events (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now(), rule_id bigint, rule_name text NOT NULL,
		subject text NOT NULL, state text NOT NULL, value double precision, message text NOT NULL);
	 INSERT INTO alert_rules(name, scope, metric, op, threshold, for_minutes, builtin) VALUES
		('Disk almost full', 'host', 'disk_used_pct', '>', 90, 5, true),
		('Memory pressure', 'host', 'mem_used_pct', '>', 92, 10, true),
		('CPU overloaded', 'host', 'load_per_cpu', '>', 2, 10, true),
		('Postgres connections near limit', 'pg', 'connections_pct', '>', 80, 5, true),
		('Health check failing', 'host', 'health_errors', '>', 0, 3, true),
		('Project error rate', 'project', 'error_rate_pct', '>', 5, 5, true)`,
	`CREATE TABLE auth_sms (
		id bigserial PRIMARY KEY,
		project text NOT NULL,
		at timestamptz NOT NULL DEFAULT now(),
		to_phone text NOT NULL,
		body text NOT NULL,
		kind text NOT NULL,
		delivered boolean NOT NULL,
		error text NOT NULL DEFAULT '',
		message_id text NOT NULL DEFAULT '');
	 CREATE INDEX auth_sms_p ON auth_sms(project, at DESC)`,
	partitionLogsMigration(),
}

func (a *App) openMeta() error {
	if !a.PG.databaseExists(metaDB) {
		c, err := a.PG.Super("postgres")
		if err != nil {
			return err
		}
		if _, err := c.Exec(`CREATE DATABASE ` + metaDB); err != nil {
			return fmt.Errorf("create meta database: %w", err)
		}
	}
	db, err := a.PG.Super(metaDB)
	if err != nil {
		return err
	}
	a.Meta = db
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (v int NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := db.QueryRow(`SELECT coalesce(max(v),0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(metaMigrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(metaMigrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("meta migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(v) VALUES ($1)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	a.maintainLogPartitions()
	return nil
}

func (a *App) getSetting(key string, dst any) bool {
	var raw []byte
	if err := a.Meta.QueryRow(`SELECT value FROM settings WHERE key=$1`, key).Scan(&raw); err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

func (a *App) setSetting(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = a.Meta.Exec(`INSERT INTO settings(key,value) VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET value=$2`, key, b)
	return err
}

// ---------------------------------------------------------------- projects

type projectRow struct {
	Slug, Name, Notes, Role, PasswordEnc, Status string
	CreatedAt                                    time.Time
}

func (a *App) project(slug string) (*projectRow, error) {
	var p projectRow
	err := a.Meta.QueryRow(`SELECT slug,name,notes,role_name,password_enc,status,created_at FROM projects WHERE slug=$1`, slug).
		Scan(&p.Slug, &p.Name, &p.Notes, &p.Role, &p.PasswordEnc, &p.Status, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &APIError{Status: 404, Code: "project_not_found", Message: fmt.Sprintf("project %q does not exist", slug), Hint: "call projects.list to see existing projects"}
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (a *App) projectPassword(p *projectRow) (string, error) {
	return unseal(a.masterKey, p.PasswordEnc)
}

// projectDB returns a small pool connected as the project's own role, so the
// SQL editor, table browser and migrations see exactly what the app sees.
func (a *App) projectDB(slug string) (*sql.DB, error) {
	a.projMu.Lock()
	defer a.projMu.Unlock()
	if db, ok := a.projPools[slug]; ok {
		return db, nil
	}
	p, err := a.project(slug)
	if err != nil {
		return nil, err
	}
	pw, err := a.projectPassword(p)
	if err != nil {
		return nil, err
	}
	db, err := a.PG.Open(p.Role, pw, slug)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(time.Minute)
	a.projPools[slug] = db
	return db, nil
}

func (a *App) closeProjectDB(slug string) {
	a.projMu.Lock()
	defer a.projMu.Unlock()
	if db, ok := a.projPools[slug]; ok {
		db.Close()
		delete(a.projPools, slug)
	}
	a.PG.ClosePool(slug)
}

// createProjectDB creates role + database. Used by projects.create and restores.
// Restores pass setupAPI=false: the API/auth/storage schema is added after the
// data is loaded, so pre-created tables with foreign keys can't reject rows.
func (a *App) createProjectDB(slug, name string, setupAPI ...bool) (string, error) {
	if a.PG.databaseExists(slug) {
		return "", &APIError{Status: 409, Code: "database_exists", Message: fmt.Sprintf("a database named %q already exists in the cluster", slug)}
	}
	if a.PG.roleExists(slug) {
		return "", &APIError{Status: 409, Code: "role_exists", Message: fmt.Sprintf("a role named %q already exists in the cluster", slug)}
	}
	pw := randHex(18)
	c, err := a.PG.Super("postgres")
	if err != nil {
		return "", err
	}
	role := pq.QuoteIdentifier(slug)
	if _, err := c.Exec(fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE CONNECTION LIMIT 60`, role, pq.QuoteLiteral(pw))); err != nil {
		return "", err
	}
	if _, err := c.Exec(fmt.Sprintf(`CREATE DATABASE %s OWNER %s STRATEGY FILE_COPY`, role, role)); err != nil {
		c.Exec(`DROP ROLE ` + role)
		return "", err
	}
	c.Exec(fmt.Sprintf(`REVOKE ALL ON DATABASE %s FROM PUBLIC`, role))
	c.Exec(fmt.Sprintf(`GRANT ALL ON DATABASE %s TO %s`, role, role))
	// extensions (db.extension_enable) live in their own schema, like Supabase;
	// set here so restored projects keep it too
	c.Exec(fmt.Sprintf(`ALTER DATABASE %s SET search_path TO "$user", public, extensions`, role))
	if db, err := a.PG.Super(slug); err == nil {
		db.Exec(fmt.Sprintf(`ALTER SCHEMA public OWNER TO %s`, role))
		a.PG.ClosePool(slug)
	}
	if _, err := a.Meta.Exec(`INSERT INTO projects(slug,name,role_name,password_enc) VALUES ($1,$2,$3,$4)`,
		slug, name, slug, seal(a.masterKey, pw)); err != nil {
		a.PG.dropDatabase(slug)
		c.Exec(`DROP ROLE ` + role)
		return "", err
	}
	if len(setupAPI) == 0 || setupAPI[0] {
		if err := a.ensureProjectAPI(slug); err != nil {
			log.Printf("project %s: API setup failed (retried on first use): %v", slug, err)
		}
	}
	return pw, nil
}

func (a *App) connectionInfo(p *projectRow, pw string) map[string]any {
	host := a.Cfg.Postgres.PublicHost
	if a.Cfg.Domain != "" && (host == "" || host == "127.0.0.1" || host == "localhost") {
		host = a.Cfg.Domain
	}
	port := a.PG.Port
	if a.PG.External {
		host = a.PG.Host
	}
	ssl := "disable"
	if a.PG.tlsReady() {
		ssl = "require"
	}
	if a.PG.External {
		ssl = a.PG.SSLMode
	}
	return map[string]any{
		"host": host, "port": port, "database": p.Slug, "user": p.Role, "password": pw, "sslmode": ssl,
		"url": fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=%s", p.Role, pw, host, port, p.Slug, ssl),
	}
}

// ResetPassword sets (or generates) a new owner password and signs out every session.
func (a *App) ResetPassword(user, pw string) (string, error) {
	if pw == "" {
		pw = randHex(10)
	}
	res, err := a.Meta.Exec(`UPDATE users SET password_hash=$2 WHERE username=$1`, user, hashPassword(pw))
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("no user %q", user)
	}
	a.Meta.Exec(`DELETE FROM sessions`)
	return pw, nil
}

// envLoginKey remembers the IVARA_ADMIN_USER / IVARA_ADMIN_PASSWORD pair that
// was last applied (the password as a salted hash), so a restart only touches
// the login when one of them changes. envPasswordKey is the 0.2.2 form, which
// stored only the password hash.
const (
	envLoginKey    = "admin_env_login"
	envPasswordKey = "admin_env_password"
)

type envLogin struct {
	User string `json:"user"`
	Hash string `json:"hash"`
}

func (a *App) userExists(name string) bool {
	var n int
	a.Meta.QueryRow(`SELECT count(*) FROM users WHERE username=$1`, name).Scan(&n)
	return n > 0
}

// applyEnvLogin makes a changed IVARA_ADMIN_USER or IVARA_ADMIN_PASSWORD take
// effect on an existing install. The same pair as last time does nothing, so a
// password changed in the UI or with `ivarabase reset-password` stays until an
// environment variable itself changes.
//
// A new IVARA_ADMIN_USER renames the login the environment set last time (when
// the new name isn't taken), so changing the name doesn't leave the old one
// behind. A name that isn't there yet is otherwise created.
func (a *App) applyEnvLogin(user, pw string) error {
	if user == "" {
		user = "admin"
	}
	var last envLogin
	have := a.getSetting(envLoginKey, &last) && last.Hash != ""
	if !have {
		var legacy string
		if a.getSetting(envPasswordKey, &legacy) && legacy != "" {
			// 0.2.2 stored only the password; it was applied to the default user
			last, have = envLogin{User: "admin", Hash: legacy}, true
		}
	}
	if have && last.User == user && checkPassword(last.Hash, pw) {
		if !a.userExists(user) { // deleted by hand since: put it back
			have = false
		} else {
			a.setSetting(envLoginKey, last)
			return nil
		}
	}

	action := "password updated"
	if have && last.User != user && a.userExists(last.User) && !a.userExists(user) {
		if _, err := a.Meta.Exec(`UPDATE users SET username=$1 WHERE username=$2`, user, last.User); err != nil {
			return err
		}
		action = fmt.Sprintf("renamed from %q", last.User)
	}
	res, err := a.Meta.Exec(`UPDATE users SET password_hash=$2 WHERE username=$1`, user, hashPassword(pw))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := a.Meta.Exec(`INSERT INTO users(username, password_hash) VALUES ($1,$2)`, user, hashPassword(pw)); err != nil {
			return err
		}
		action = "created"
	}
	// signed-in browsers keep working only with the new login
	a.Meta.Exec(`DELETE FROM sessions`)
	detail, _ := json.Marshal(map[string]string{"user": user, "action": action})
	a.Meta.Exec(`INSERT INTO audit(actor,op,detail,ok) VALUES ('system','account.env_login',$1,true)`, string(detail))
	log.Printf("IVARA_ADMIN_USER/IVARA_ADMIN_PASSWORD changed: login %q %s, sessions signed out", user, action)
	return a.setSetting(envLoginKey, envLogin{User: user, Hash: hashPassword(pw)})
}

// SetBackupTarget stores an S3 target (used by disaster recovery).
func (a *App) SetBackupTarget(s3 S3Config) error {
	cfg := a.backupConfig()
	if s3.Endpoint != "" && !strings.HasPrefix(s3.Endpoint, "http") {
		s3.Endpoint = "https://" + s3.Endpoint
	}
	cfg.S3 = s3
	return a.setSetting("backup_config", cfg)
}
