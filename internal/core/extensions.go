package core

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/lib/pq"
)

// Postgres extensions a project may turn on. CREATE EXTENSION runs as the
// superuser, so only this curated list is allowed: extensions that stay inside
// the project's own database (no file access, no connections to other
// databases such as dblink/postgres_fdw, no untrusted languages).
type extInfo struct {
	Desc    string
	Install string // how to get it when the files are missing
}

var allowedExtensions = map[string]extInfo{
	"vector":             {"pgvector: vector type and similarity search (embeddings, RAG)", "Ubuntu: apt install postgresql-<major>-pgvector · macOS: brew install pgvector"},
	"pg_trgm":            {"trigram similarity, fast LIKE/ILIKE and fuzzy search indexes", "part of PostgreSQL contrib"},
	"pgcrypto":           {"hashing, encryption and gen_random_bytes()", "part of PostgreSQL contrib"},
	"uuid-ossp":          {"uuid_generate_v4() and other UUID generators", "part of PostgreSQL contrib"},
	"citext":             {"case-insensitive text type (emails, usernames)", "part of PostgreSQL contrib"},
	"pg_stat_statements": {"query statistics (powers the Slow queries page)", "part of PostgreSQL contrib"},
	"postgis":            {"geographic types, spatial indexes and functions", "Ubuntu: apt install postgresql-<major>-postgis-3 · macOS: brew install postgis"},
	"hstore":             {"key/value column type", "part of PostgreSQL contrib"},
	"ltree":              {"hierarchical tree labels (categories, paths)", "part of PostgreSQL contrib"},
	"unaccent":           {"remove accents for search", "part of PostgreSQL contrib"},
	"fuzzystrmatch":      {"soundex, levenshtein and metaphone", "part of PostgreSQL contrib"},
	"btree_gin":          {"GIN operator classes for scalar types", "part of PostgreSQL contrib"},
	"btree_gist":         {"GiST operator classes (exclusion constraints on ranges)", "part of PostgreSQL contrib"},
	"intarray":           {"integer array functions and indexes", "part of PostgreSQL contrib"},
	"cube":               {"multi-dimensional cubes", "part of PostgreSQL contrib"},
	"earthdistance":      {"great-circle distances (needs cube)", "part of PostgreSQL contrib"},
	"tablefunc":          {"crosstab and other table functions", "part of PostgreSQL contrib"},
	"pg_buffercache":     {"inspect the shared buffer cache", "part of PostgreSQL contrib"},
}

const extSchema = "extensions"

var (
	pkgLibOnce sync.Once
	pkgLibDir  string
)

// pgPkgLibDir is the folder with the extension libraries of the managed cluster.
func (p *Postgres) pgPkgLibDir() string {
	pkgLibOnce.Do(func() {
		if p.External || p.BinDir == "" {
			return
		}
		out, err := exec.Command(p.bin("pg_config"), "--pkglibdir").Output()
		if err == nil {
			pkgLibDir = strings.TrimSpace(string(out))
		}
	})
	return pkgLibDir
}

// hasLibrary reports whether a shared library such as pg_stat_statements is
// installed for the managed cluster.
func (p *Postgres) hasLibrary(name string) bool {
	dir := p.pgPkgLibDir()
	if dir == "" {
		return false
	}
	for _, ext := range []string{".so", ".dylib"} {
		if fileExists(filepath.Join(dir, name+ext)) {
			return true
		}
	}
	return false
}

// preloadLibraries adds pg_stat_statements to shared_preload_libraries when it
// is installed and the user hasn't chosen their own list.
func (p *Postgres) preloadLibraries(tune map[string]string) {
	if _, set := tune["shared_preload_libraries"]; set {
		return
	}
	if p.hasLibrary("pg_stat_statements") {
		tune["shared_preload_libraries"] = "pg_stat_statements"
		tune["pg_stat_statements.track"] = "top"
		tune["pg_stat_statements.max"] = "5000"
	}
}

// queryStatsReady makes sure pg_stat_statements exists in the postgres
// database, where the superuser reads statistics for every project.
func (a *App) queryStatsReady() error {
	c, err := a.PG.Super("postgres")
	if err != nil {
		return err
	}
	var preloaded string
	c.QueryRow(`SELECT current_setting('shared_preload_libraries', true)`).Scan(&preloaded)
	if !strings.Contains(preloaded, "pg_stat_statements") {
		return &APIError{Status: 409, Code: "query_stats_unavailable",
			Message: "pg_stat_statements is not loaded by Postgres",
			Hint:    "install PostgreSQL contrib, then restart IvaraBase (it adds pg_stat_statements to shared_preload_libraries automatically unless postgres.settings.shared_preload_libraries is set)"}
	}
	_, err = c.Exec(`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`)
	return err
}

func registerExtensionOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}
	nameParam := Param{Name: "name", Type: "string", Required: true, Desc: "extension name, e.g. vector"}

	r.add(&Op{Name: "db.extensions_list", Tag: "database", Method: "GET", Path: "/api/v1/projects/{project}/extensions", Level: LevelRead,
		Summary: "Postgres extensions a project can turn on, and which are on",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.PG.Super(a.Str("project"))
			if err != nil {
				return nil, err
			}
			if _, err := c.App.project(a.Str("project")); err != nil {
				return nil, err
			}
			rows, err := db.QueryContext(c, `SELECT a.name, coalesce(a.default_version,''), coalesce(a.installed_version,''), coalesce(n.nspname,'')
				FROM pg_available_extensions a LEFT JOIN pg_extension e ON e.extname=a.name LEFT JOIN pg_namespace n ON n.oid=e.extnamespace`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			found := map[string]map[string]any{}
			for rows.Next() {
				var name, def, inst, schema string
				if err := rows.Scan(&name, &def, &inst, &schema); err != nil {
					return nil, err
				}
				if _, ok := allowedExtensions[name]; ok {
					found[name] = map[string]any{"version": def, "installed_version": inst, "schema": schema, "enabled": inst != ""}
				}
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
			names := make([]string, 0, len(allowedExtensions))
			for n := range allowedExtensions {
				names = append(names, n)
			}
			sort.Strings(names)
			out := []map[string]any{}
			for _, n := range names {
				info := allowedExtensions[n]
				row := map[string]any{"name": n, "description": info.Desc, "available": false, "enabled": false}
				if f, ok := found[n]; ok {
					for k, v := range f {
						row[k] = v
					}
					row["available"] = true
				} else {
					row["install_hint"] = info.Install
				}
				out = append(out, row)
			}
			return out, nil
		}})

	r.add(&Op{Name: "db.extension_enable", Tag: "database", Method: "POST", Path: "/api/v1/projects/{project}/extensions/{name}", Level: LevelWrite,
		Summary:     "Turn on a Postgres extension in a project (vector, pg_trgm, postgis, ...)",
		Description: "Runs CREATE EXTENSION as the superuser for extensions on the allowed list (db.extensions_list). They go into the `extensions` schema by default, like Supabase, which is on the project's search_path and the REST API's.",
		Params:      []Param{projectParam, nameParam, {Name: "schema", Type: "string", Default: extSchema, Desc: "schema to install into"}},
		Run: func(c *Ctx, a Args) (any, error) {
			slug, name, schema := a.Str("project"), a.Str("name"), a.Str("schema")
			if schema == "" {
				schema = extSchema
			}
			if _, ok := allowedExtensions[name]; !ok {
				return nil, &APIError{Status: 400, Code: "extension_not_allowed", Message: fmt.Sprintf("extension %q is not on the allowed list", name), Hint: "db.extensions_list shows the extensions you can turn on"}
			}
			p, err := c.App.project(slug)
			if err != nil {
				return nil, err
			}
			db, err := c.App.PG.Super(slug)
			if err != nil {
				return nil, err
			}
			var avail bool
			db.QueryRowContext(c, `SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name=$1)`, name).Scan(&avail)
			if !avail {
				return nil, &APIError{Status: 409, Code: "extension_not_installed", Message: fmt.Sprintf("the %s extension is not installed on this server", name), Hint: allowedExtensions[name].Install}
			}
			if name == "pg_stat_statements" {
				if err := c.App.queryStatsReady(); err != nil {
					return nil, err
				}
			}
			tx, err := db.BeginTx(c, nil)
			if err != nil {
				return nil, err
			}
			defer tx.Rollback()
			qs := pq.QuoteIdentifier(schema)
			stmts := []string{
				`CREATE SCHEMA IF NOT EXISTS ` + qs,
				fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s, anon, authenticated, service_role`, qs, pq.QuoteIdentifier(p.Role)),
				fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS %s WITH SCHEMA %s CASCADE`, pq.QuoteIdentifier(name), qs),
				fmt.Sprintf(`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA %s TO %s, anon, authenticated, service_role`, qs, pq.QuoteIdentifier(p.Role)),
				fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT EXECUTE ON FUNCTIONS TO %s, anon, authenticated, service_role`, qs, pq.QuoteIdentifier(p.Role)),
			}
			if schema == extSchema {
				stmts = append(stmts, fmt.Sprintf(`ALTER DATABASE %s SET search_path TO "$user", public, extensions`, pq.QuoteIdentifier(slug)))
			}
			for _, s := range stmts {
				if _, err := tx.ExecContext(c, s); err != nil {
					return nil, fmt.Errorf("%s: %w", strings.SplitN(s, " ", 3)[0]+" "+strings.SplitN(s, " ", 3)[1], err)
				}
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			var ver string
			db.QueryRowContext(c, `SELECT extversion FROM pg_extension WHERE extname=$1`, name).Scan(&ver)
			// new sessions pick up the search_path; drop pooled ones
			c.App.closeProjectDB(slug)
			c.App.Rest.Stop(slug)
			return map[string]any{"name": name, "schema": schema, "version": ver, "enabled": true}, nil
		}})

	r.add(&Op{Name: "db.extension_disable", Tag: "database", Method: "DELETE", Path: "/api/v1/projects/{project}/extensions/{name}", Level: LevelWrite, Destructive: true,
		Summary:     "Turn off a Postgres extension (DROP EXTENSION, without CASCADE)",
		Description: "Fails if tables, columns or indexes still use it - drop those first.",
		Params:      []Param{projectParam, nameParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			return map[string]any{"would_drop_extension": a.Str("name"), "project": a.Str("project"), "note": "fails without changing anything if objects still depend on it"}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			slug, name := a.Str("project"), a.Str("name")
			if _, ok := allowedExtensions[name]; !ok {
				return nil, &APIError{Status: 400, Code: "extension_not_allowed", Message: fmt.Sprintf("extension %q is not managed by ivarabase", name)}
			}
			if _, err := c.App.project(slug); err != nil {
				return nil, err
			}
			db, err := c.App.PG.Super(slug)
			if err != nil {
				return nil, err
			}
			if _, err := db.ExecContext(c, `DROP EXTENSION IF EXISTS `+pq.QuoteIdentifier(name)); err != nil {
				return nil, &APIError{Status: 409, Code: "extension_in_use", Message: err.Error(), Hint: "drop the columns, indexes or functions that use it first"}
			}
			c.App.Rest.Stop(slug)
			return map[string]any{"name": name, "enabled": false}, nil
		}})

	r.add(&Op{Name: "db.slow_queries", Tag: "database", Method: "GET", Path: "/api/v1/projects/{project}/slow-queries", Level: LevelRead,
		Summary:     "Slowest and most frequent queries in a project (pg_stat_statements)",
		Description: "order_by: total (total time, default), mean (average time) or calls.",
		Params: []Param{projectParam,
			{Name: "order_by", Type: "string", Default: "total", Enum: []string{"total", "mean", "calls"}},
			{Name: "limit", Type: "integer", Default: 50}},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			if _, err := c.App.project(slug); err != nil {
				return nil, err
			}
			if err := c.App.queryStatsReady(); err != nil {
				return nil, err
			}
			db, err := c.App.PG.Super("postgres")
			if err != nil {
				return nil, err
			}
			order := map[string]string{"total": "total_exec_time", "mean": "mean_exec_time", "calls": "calls"}[a.Str("order_by")]
			if order == "" {
				order = "total_exec_time"
			}
			rows, err := db.QueryContext(c, `SELECT s.query, s.calls, s.total_exec_time, s.mean_exec_time, s.max_exec_time, s.rows,
				coalesce(r.rolname,''), s.shared_blks_hit, s.shared_blks_read
				FROM pg_stat_statements s JOIN pg_database d ON d.oid=s.dbid LEFT JOIN pg_roles r ON r.oid=s.userid
				WHERE d.datname=$1 AND s.query NOT ILIKE '%pg_stat_statements%'
				ORDER BY `+order+` DESC LIMIT $2`, slug, clamp(a.Int("limit", 50), 1, 500))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var q, role string
				var calls, nrows, hit, read int64
				var total, mean, max float64
				if err := rows.Scan(&q, &calls, &total, &mean, &max, &nrows, &role, &hit, &read); err != nil {
					return nil, err
				}
				ratio := 1.0
				if hit+read > 0 {
					ratio = float64(hit) / float64(hit+read)
				}
				out = append(out, map[string]any{"query": q, "calls": calls, "total_ms": total, "mean_ms": mean, "max_ms": max,
					"rows": nrows, "role": role, "cache_hit_ratio": ratio})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "db.slow_queries_reset", Tag: "database", Method: "POST", Path: "/api/v1/projects/{project}/slow-queries/reset", Level: LevelWrite,
		Summary: "Clear the query statistics of a project",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			if _, err := c.App.project(slug); err != nil {
				return nil, err
			}
			if err := c.App.queryStatsReady(); err != nil {
				return nil, err
			}
			db, err := c.App.PG.Super("postgres")
			if err != nil {
				return nil, err
			}
			_, err = db.ExecContext(c, `SELECT pg_stat_statements_reset(0, (SELECT oid FROM pg_database WHERE datname=$1), 0)`, slug)
			if err != nil {
				return nil, err
			}
			return map[string]any{"reset": true}, nil
		}})
}
