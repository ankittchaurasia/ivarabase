package core

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Supabase-compatible project API.
//
// Roles: three shared NOLOGIN roles (anon, authenticated, service_role) so that
// Supabase-style SQL (`create policy ... to authenticated`) works unchanged.
// Sharing them is safe because each project has its own LOGIN role
// <slug>_authenticator that may only CONNECT to its own database: a session can
// only ever SET ROLE inside that one database.
//
// Keys: a per-project HS256 secret signs the anon and service_role keys (and,
// later, user sessions). PostgREST verifies them with the same secret.

const (
	roleAnon    = "anon"
	roleAuthed  = "authenticated"
	roleService = "service_role"
	internalNS  = "ivarabase_internal"
)

func authenticatorRole(slug string) string { return slug + "_authenticator" }

// ensureClusterRoles creates the shared API roles once per cluster.
func (a *App) ensureClusterRoles() error {
	c, err := a.PG.Super("postgres")
	if err != nil {
		return err
	}
	for _, r := range []struct{ name, attrs string }{
		{roleAnon, "NOLOGIN NOINHERIT"},
		{roleAuthed, "NOLOGIN NOINHERIT"},
		{roleService, "NOLOGIN NOINHERIT BYPASSRLS"},
	} {
		if !a.PG.roleExists(r.name) {
			if _, err := c.Exec(fmt.Sprintf(`CREATE ROLE %s %s`, pq.QuoteIdentifier(r.name), r.attrs)); err != nil {
				return fmt.Errorf("create role %s: %w", r.name, err)
			}
		}
	}
	return nil
}

type projectAPI struct {
	Secret []byte
	AuthPW string
}

// projectAPIState returns (and on first use provisions) the project's API secrets.
func (a *App) projectAPIState(slug string) (*projectAPI, error) {
	var secEnc, pwEnc string
	if err := a.Meta.QueryRow(`SELECT jwt_secret_enc, authenticator_pw_enc FROM projects WHERE slug=$1`, slug).Scan(&secEnc, &pwEnc); err != nil {
		if err == sql.ErrNoRows {
			_, perr := a.project(slug)
			return nil, perr
		}
		return nil, err
	}
	if secEnc == "" || pwEnc == "" {
		if err := a.ensureProjectAPI(slug); err != nil {
			return nil, err
		}
		return a.projectAPIState(slug)
	}
	sec, err := unseal(a.masterKey, secEnc)
	if err != nil {
		return nil, err
	}
	pw, err := unseal(a.masterKey, pwEnc)
	if err != nil {
		return nil, err
	}
	return &projectAPI{Secret: []byte(sec), AuthPW: pw}, nil
}

// ensureProjectAPI is idempotent: it creates the authenticator role, grants,
// the auth helper functions and the schema-reload trigger. It runs on project
// creation, after restores, and lazily for projects created before the API existed.
func (a *App) ensureProjectAPI(slug string) error {
	if err := a.ensureClusterRoles(); err != nil {
		return err
	}
	var secEnc, pwEnc string
	if err := a.Meta.QueryRow(`SELECT jwt_secret_enc, authenticator_pw_enc FROM projects WHERE slug=$1`, slug).Scan(&secEnc, &pwEnc); err != nil {
		return err
	}
	if secEnc == "" {
		secEnc = seal(a.masterKey, randHex(32))
	}
	pw := randHex(18)
	if pwEnc != "" {
		if old, err := unseal(a.masterKey, pwEnc); err == nil {
			pw = old
		}
	}
	pwEnc = seal(a.masterKey, pw)

	c, err := a.PG.Super("postgres")
	if err != nil {
		return err
	}
	auth := pq.QuoteIdentifier(authenticatorRole(slug))
	db := pq.QuoteIdentifier(slug)
	owner := pq.QuoteIdentifier(slug)
	if a.PG.roleExists(authenticatorRole(slug)) {
		if _, err := c.Exec(fmt.Sprintf(`ALTER ROLE %s LOGIN NOINHERIT PASSWORD %s`, auth, pq.QuoteLiteral(pw))); err != nil {
			return err
		}
	} else if _, err := c.Exec(fmt.Sprintf(`CREATE ROLE %s LOGIN NOINHERIT NOCREATEDB NOCREATEROLE CONNECTION LIMIT 40 PASSWORD %s`, auth, pq.QuoteLiteral(pw))); err != nil {
		return err
	}
	for _, q := range []string{
		fmt.Sprintf(`GRANT %s, %s, %s TO %s`, roleAnon, roleAuthed, roleService, auth),
		fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`, db, auth),
		fmt.Sprintf(`ALTER ROLE %s SET statement_timeout = '30s'`, auth),
	} {
		if _, err := c.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}

	pdb, err := a.PG.Super(slug)
	if err != nil {
		return err
	}
	defer a.PG.ClosePool(slug)
	roles := fmt.Sprintf("%s, %s, %s", roleAnon, roleAuthed, roleService)
	stmts := []string{
		// auth helpers, owned by the project (it owns its database anyway)
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS auth AUTHORIZATION %s`, owner),
		`CREATE OR REPLACE FUNCTION auth.jwt() RETURNS jsonb LANGUAGE sql STABLE AS $$
			SELECT coalesce(nullif(current_setting('request.jwt.claims', true), ''), '{}')::jsonb $$`,
		`CREATE OR REPLACE FUNCTION auth.uid() RETURNS uuid LANGUAGE sql STABLE AS $$
			SELECT nullif(coalesce(nullif(current_setting('request.jwt.claim.sub', true), ''), auth.jwt()->>'sub'), '')::uuid $$`,
		`CREATE OR REPLACE FUNCTION auth.role() RETURNS text LANGUAGE sql STABLE AS $$
			SELECT coalesce(nullif(current_setting('request.jwt.claim.role', true), ''), auth.jwt()->>'role') $$`,
		`CREATE OR REPLACE FUNCTION auth.email() RETURNS text LANGUAGE sql STABLE AS $$
			SELECT auth.jwt()->>'email' $$`,
		fmt.Sprintf(`ALTER SCHEMA auth OWNER TO %s`, owner),
		fmt.Sprintf(`ALTER FUNCTION auth.jwt() OWNER TO %s`, owner),
		fmt.Sprintf(`ALTER FUNCTION auth.uid() OWNER TO %s`, owner),
		fmt.Sprintf(`ALTER FUNCTION auth.role() OWNER TO %s`, owner),
		fmt.Sprintf(`ALTER FUNCTION auth.email() OWNER TO %s`, owner),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA auth TO %s`, roles),
		fmt.Sprintf(`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA auth TO %s`, roles),
		// PostgREST's db-extra-search-path names the extensions schema and won't
		// start without it, so every project has it before its first extension.
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS extensions AUTHORIZATION %s`, owner),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA extensions TO %s`, roles),

		// Supabase defaults: API roles may use public objects; row level
		// security is what protects rows (the UI and health checks flag
		// tables without it).
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, roles),
		// PostgREST's db-extra-search-path names it: without the schema the
		// REST API can't load its schema cache until an extension is enabled
		`CREATE SCHEMA IF NOT EXISTS ` + extSchema,
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s, %s`, extSchema, owner, roles),
		fmt.Sprintf(`GRANT ALL ON ALL TABLES IN SCHEMA public TO %s`, roles),
		fmt.Sprintf(`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO %s`, roles),
		fmt.Sprintf(`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO %s`, roles),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT ALL ON TABLES TO %s`, owner, roles),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT ALL ON SEQUENCES TO %s`, owner, roles),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO %s`, owner, roles),

		// Supabase's extensions schema: db.extension_enable installs there, and
		// it is on the REST API's search path (db-extra-search-path).
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS extensions AUTHORIZATION %s`, owner),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA extensions TO %s`, roles),

		// Reload the REST schema cache after any DDL. The trigger function is
		// owned by the superuser in a schema the project cannot write to, so a
		// project can never run code with superuser rights through it.
		`CREATE SCHEMA IF NOT EXISTS ` + internalNS,
		`REVOKE ALL ON SCHEMA ` + internalNS + ` FROM PUBLIC`,
		`CREATE OR REPLACE FUNCTION ` + internalNS + `.pgrst_watch() RETURNS event_trigger LANGUAGE plpgsql AS $$
			BEGIN NOTIFY pgrst, 'reload schema'; END $$`,
		`DROP EVENT TRIGGER IF EXISTS ivarabase_pgrst_watch`,
		`CREATE EVENT TRIGGER ivarabase_pgrst_watch ON ddl_command_end EXECUTE FUNCTION ` + internalNS + `.pgrst_watch()`,
	}
	for _, q := range stmts {
		if _, err := pdb.Exec(q); err != nil {
			return fmt.Errorf("project API setup (%s): %w", strings.SplitN(strings.TrimSpace(q), "\n", 2)[0], err)
		}
	}
	if _, err = a.Meta.Exec(`UPDATE projects SET jwt_secret_enc=$2, authenticator_pw_enc=$3 WHERE slug=$1`, slug, secEnc, pwEnc); err != nil {
		return err
	}
	if err := a.ensureAuthSchema(slug); err != nil {
		return err
	}
	if err := a.ensureStorageSchema(slug); err != nil {
		return err
	}
	if err := a.ensureRealtimeSchema(slug); err != nil {
		log.Printf("project %s: realtime setup failed (retried on first subscription): %v", slug, err)
	}
	authReady.Store(slug, true)
	return nil
}

func (a *App) dropProjectAPI(slug string) {
	authReady.Delete(slug)
	realtimeDropProject(slug)
	closeAPIDB(slug)
	a.Meta.Exec(`DELETE FROM auth_config WHERE project=$1`, slug)
	a.Meta.Exec(`DELETE FROM auth_mails WHERE project=$1`, slug)
	a.Meta.Exec(`DELETE FROM auth_sms WHERE project=$1`, slug)
	a.Rest.Stop(slug)
	if c, err := a.PG.Super("postgres"); err == nil {
		c.Exec(`DROP ROLE IF EXISTS ` + pq.QuoteIdentifier(authenticatorRole(slug)))
	}
}

func apiKey(secret []byte, slug, role string) string {
	return signJWT(secret, map[string]any{
		"iss": "ivarabase", "ref": slug, "role": role,
		"iat": 1700000000, "exp": time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
	})
}

// ---------------------------------------------------------------- ops

func registerProjectAPIOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}

	r.add(&Op{Name: "projects.api", Tag: "projects", Method: "GET", Path: "/api/v1/projects/{project}/api", Level: LevelRead,
		Summary:     "Project API URL and keys for supabase-js / REST clients",
		Description: "anon is safe in browsers (row level security applies). service_role bypasses row level security and is only returned to write-level callers.",
		Params:      []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			st, err := c.App.projectAPIState(slug)
			if err != nil {
				return nil, err
			}
			base := c.App.projectURL(slug)
			out := map[string]any{
				"url": base, "rest_url": base + "/rest/v1", "realtime_url": strings.Replace(base, "http", "ws", 1) + "/realtime/v1/websocket", "anon_key": apiKey(st.Secret, slug, roleAnon),
				"rest": c.App.Rest.status(slug),
			}
			if c.Principal.Level >= LevelWrite {
				out["service_role_key"] = apiKey(st.Secret, slug, roleService)
				out["jwt_secret"] = string(st.Secret)
			}
			return out, nil
		}})

	r.add(&Op{Name: "projects.rotate_jwt_secret", Tag: "projects", Method: "POST", Path: "/api/v1/projects/{project}/rotate-jwt-secret", Level: LevelWrite,
		Destructive: true,
		Summary:     "Rotate the JWT secret: new anon/service keys, every existing key and user session stops working",
		Params:      []Param{projectParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			return map[string]any{"effect": "the current anon and service_role keys and all signed-in user sessions become invalid immediately",
				"after": "update every app with the new keys from projects.api"}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			if _, err := c.App.project(slug); err != nil {
				return nil, err
			}
			if _, err := c.App.Meta.Exec(`UPDATE projects SET jwt_secret_enc=$2 WHERE slug=$1`, slug, seal(c.App.masterKey, randHex(32))); err != nil {
				return nil, err
			}
			c.App.Rest.Stop(slug)
			realtimeDropProject(slug)
			return c.App.Ops.Get("projects.api").Run(c, Args{"project": slug})
		}})

	r.add(&Op{Name: "db.set_rls", Tag: "database", Method: "POST", Path: "/api/v1/projects/{project}/tables/{table}/rls", Level: LevelWrite,
		Summary:     "Enable or disable row level security on a table",
		Description: "With RLS enabled and no policies, the anon and authenticated roles see no rows; add policies with a migration. With RLS disabled, anyone holding the anon key can read and write the table.",
		Params:      []Param{projectParam, {Name: "table", Type: "string", Required: true}, {Name: "schema", Type: "string", Default: "public"}, {Name: "enabled", Type: "boolean", Required: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			verb := "DISABLE"
			if a.Bool("enabled", true) {
				verb = "ENABLE"
			}
			if _, err := db.ExecContext(c, fmt.Sprintf(`ALTER TABLE %s %s ROW LEVEL SECURITY`, qualified(a.Str("schema"), a.Str("table")), verb)); err != nil {
				return nil, sqlError(err)
			}
			return map[string]any{"table": a.Str("table"), "rls_enabled": a.Bool("enabled", true)}, nil
		}})
}

// tablesWithoutRLS lists public tables the anon key can reach unfiltered.
func (a *App) tablesWithoutRLS(slug string) []string {
	db, err := a.projectDB(slug)
	if err != nil {
		return nil
	}
	rows, err := db.Query(`SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind IN ('r','p') AND NOT c.relrowsecurity
		AND has_table_privilege('anon', c.oid, 'SELECT') ORDER BY 1`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out
}
