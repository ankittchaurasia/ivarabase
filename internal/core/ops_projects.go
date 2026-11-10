package core

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/lib/pq"
)

var slugRe = regexp.MustCompile(`^[a-z][a-z0-9_]{2,39}$`)

var reservedSlugs = map[string]bool{
	"postgres": true, "template0": true, "template1": true, superUser: true, metaDB: true,
	"public": true, "admin": true, "root": true, "pg_signal_backend": true,
}

func validSlug(s string) error {
	if !slugRe.MatchString(s) {
		return errBad("invalid_slug", fmt.Sprintf("%q is not a valid project slug", s),
			"3-40 characters: lowercase letters, digits and underscores, starting with a letter (e.g. my_app)")
	}
	if reservedSlugs[s] || strings.HasPrefix(s, "pg_") || strings.HasPrefix(s, "ivb_") {
		return errBad("reserved_slug", fmt.Sprintf("%q is reserved", s), "pick another name")
	}
	return nil
}

func registerProjectOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}

	r.add(&Op{Name: "projects.list", Tag: "projects", Method: "GET", Path: "/api/v1/projects", Level: LevelRead,
		Summary: "List projects with database size and activity",
		Run: func(c *Ctx, a Args) (any, error) {
			rows, err := c.App.Meta.Query(`SELECT slug,name,notes,status,created_at FROM projects ORDER BY created_at`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var slug, name, notes, status string
				var created time.Time
				if err := rows.Scan(&slug, &name, &notes, &status, &created); err != nil {
					return nil, err
				}
				if !c.Principal.canProject(slug) {
					continue
				}
				out = append(out, map[string]any{"slug": slug, "name": name, "notes": notes, "status": status, "created_at": created})
			}
			rows.Close()
			sizes := c.App.dbSizes()
			for _, p := range out {
				st := sizes[p["slug"].(string)]
				p["size_bytes"] = st.size
				p["connections"] = st.conns
				p["last_backup_at"] = c.App.lastBackupAt(p["slug"].(string))
			}
			return out, nil
		}})

	r.add(&Op{Name: "projects.get", Tag: "projects", Method: "GET", Path: "/api/v1/projects/{project}", Level: LevelRead,
		Summary: "Project details: size, tables, functions, backups (no password)",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			p, err := c.App.project(a.Str("project"))
			if err != nil {
				return nil, err
			}
			st := c.App.dbSizes()[p.Slug]
			var fns, secrets, migs, backups int
			c.App.Meta.QueryRow(`SELECT count(*) FROM functions WHERE project=$1`, p.Slug).Scan(&fns)
			c.App.Meta.QueryRow(`SELECT count(*) FROM secrets WHERE project=$1`, p.Slug).Scan(&secrets)
			c.App.Meta.QueryRow(`SELECT count(*) FROM migrations WHERE project=$1`, p.Slug).Scan(&migs)
			c.App.Meta.QueryRow(`SELECT count(*) FROM backups WHERE project=$1`, p.Slug).Scan(&backups)
			var tables int
			if db, err := c.App.projectDB(p.Slug); err == nil {
				db.QueryRowContext(c, `SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema')`).Scan(&tables)
			}
			conn := c.App.connectionInfo(p, "********")
			delete(conn, "password")
			conn["url"] = strings.Replace(conn["url"].(string), ":********@", ":<password>@", 1)
			return map[string]any{"slug": p.Slug, "name": p.Name, "notes": p.Notes, "status": p.Status,
				"created_at": p.CreatedAt, "size_bytes": st.size, "connections": st.conns, "tables": tables,
				"functions": fns, "secrets": secrets, "migrations": migs, "backups": backups,
				"last_backup_at": c.App.lastBackupAt(p.Slug), "connection": conn,
				"api_url": c.App.projectURL(p.Slug), "tables_without_rls": nonNil(c.App.tablesWithoutRLS(p.Slug)),
				"functions_url": c.App.Cfg.PublicURL + "/fn/" + p.Slug + "/<name>"}, nil
		}})

	r.add(&Op{Name: "projects.create", Tag: "projects", Method: "POST", Path: "/api/v1/projects", Level: LevelWrite,
		Summary:     "Create a project: its own Postgres database and login role (under a second)",
		Description: "Returns the connection string once. Fetch it again later with projects.credentials.",
		Params: []Param{{Name: "slug", Type: "string", Required: true, Desc: "database + role name: 3-40 chars, a-z 0-9 _"},
			{Name: "name", Type: "string", Desc: "display name (defaults to the slug)"},
			{Name: "notes", Type: "string"}},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := strings.ToLower(strings.TrimSpace(a.Str("slug")))
			if err := validSlug(slug); err != nil {
				return nil, err
			}
			if c.Principal.Kind == "token" && len(c.Principal.Projects) > 0 {
				return nil, errForbidden("project-scoped tokens cannot create new projects", "use a token without a project restriction")
			}
			var exists bool
			c.App.Meta.QueryRow(`SELECT EXISTS(SELECT 1 FROM projects WHERE slug=$1)`, slug).Scan(&exists)
			if exists {
				return nil, &APIError{Status: 409, Code: "project_exists", Message: fmt.Sprintf("project %q already exists", slug)}
			}
			name := a.Str("name")
			if name == "" {
				name = slug
			}
			start := time.Now()
			pw, err := c.App.createProjectDB(slug, name)
			if err != nil {
				return nil, err
			}
			if n := a.Str("notes"); n != "" {
				c.App.Meta.Exec(`UPDATE projects SET notes=$2 WHERE slug=$1`, slug, n)
			}
			p, _ := c.App.project(slug)
			return map[string]any{"slug": slug, "name": name, "created_ms": time.Since(start).Milliseconds(),
				"connection": c.App.connectionInfo(p, pw)}, nil
		}})

	r.add(&Op{Name: "projects.update", Tag: "projects", Method: "PATCH", Path: "/api/v1/projects/{project}", Level: LevelWrite,
		Summary: "Rename a project or edit its notes",
		Params:  []Param{projectParam, {Name: "name", Type: "string"}, {Name: "notes", Type: "string"}},
		Run: func(c *Ctx, a Args) (any, error) {
			p, err := c.App.project(a.Str("project"))
			if err != nil {
				return nil, err
			}
			name, notes := p.Name, p.Notes
			if _, ok := a["name"]; ok && a.Str("name") != "" {
				name = a.Str("name")
			}
			if _, ok := a["notes"]; ok {
				notes = a.Str("notes")
			}
			_, err = c.App.Meta.Exec(`UPDATE projects SET name=$2, notes=$3 WHERE slug=$1`, p.Slug, name, notes)
			return map[string]any{"slug": p.Slug, "name": name, "notes": notes}, err
		}})

	r.add(&Op{Name: "projects.credentials", Tag: "projects", Method: "GET", Path: "/api/v1/projects/{project}/credentials", Level: LevelWrite,
		Summary: "Connection string and password for the project database",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			p, err := c.App.project(a.Str("project"))
			if err != nil {
				return nil, err
			}
			pw, err := c.App.projectPassword(p)
			if err != nil {
				return nil, err
			}
			return c.App.connectionInfo(p, pw), nil
		}})

	r.add(&Op{Name: "projects.rotate_password", Tag: "projects", Method: "POST", Path: "/api/v1/projects/{project}/rotate-password", Level: LevelWrite,
		Destructive: true,
		Summary:     "Generate a new database password (apps using the old one will fail to connect)",
		Params:      []Param{projectParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			st := c.App.dbSizes()[a.Str("project")]
			return map[string]any{"effect": "the current password stops working immediately", "open_connections": st.conns}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			p, err := c.App.project(a.Str("project"))
			if err != nil {
				return nil, err
			}
			pw := randHex(18)
			sc, err := c.App.PG.Super("postgres")
			if err != nil {
				return nil, err
			}
			if _, err := sc.Exec(fmt.Sprintf(`ALTER ROLE %s PASSWORD %s`, pq.QuoteIdentifier(p.Role), pq.QuoteLiteral(pw))); err != nil {
				return nil, err
			}
			c.App.Meta.Exec(`UPDATE projects SET password_enc=$2 WHERE slug=$1`, p.Slug, seal(c.App.masterKey, pw))
			c.App.closeProjectDB(p.Slug)
			c.App.Fn.StopProject(p.Slug)
			return c.App.connectionInfo(p, pw), nil
		}})

	r.add(&Op{Name: "projects.delete", Tag: "projects", Method: "DELETE", Path: "/api/v1/projects/{project}", Level: LevelWrite,
		Destructive: true,
		Summary:     "Delete a project: database, role, functions and secrets (backups are kept)",
		Params:      []Param{projectParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			p, err := c.App.project(a.Str("project"))
			if err != nil {
				return nil, err
			}
			st := c.App.dbSizes()[p.Slug]
			var fns, backups int
			c.App.Meta.QueryRow(`SELECT count(*) FROM functions WHERE project=$1`, p.Slug).Scan(&fns)
			c.App.Meta.QueryRow(`SELECT count(*) FROM backups WHERE project=$1`, p.Slug).Scan(&backups)
			return map[string]any{"project": p.Slug, "database_bytes": st.size, "open_connections": st.conns,
				"functions_deleted": fns, "backups_kept": backups, "last_backup_at": c.App.lastBackupAt(p.Slug),
				"advice": "take a backup first with backups.create if you may need this data"}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			p, err := c.App.project(a.Str("project"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": p.Slug}, c.App.deleteProject(p)
		}})
}

func (a *App) deleteProject(p *projectRow) error {
	a.Fn.StopProject(p.Slug)
	a.Hooks.stop(p.Slug)
	a.deleteProjectStorage(context.Background(), p.Slug)
	os.RemoveAll(a.Cfg.Path("storage", p.Slug))
	a.closeProjectDB(p.Slug)
	if err := a.PG.dropDatabase(p.Slug); err != nil {
		return err
	}
	a.dropProjectAPI(p.Slug)
	if sc, err := a.PG.Super("postgres"); err == nil {
		sc.Exec(`DROP ROLE IF EXISTS ` + pq.QuoteIdentifier(p.Role))
	}
	for _, t := range []string{"functions", "function_logs", "function_console", "secrets", "migrations"} {
		a.Meta.Exec(`DELETE FROM `+t+` WHERE project=$1`, p.Slug)
	}
	os.RemoveAll(a.Cfg.Path("functions", p.Slug))
	_, err := a.Meta.Exec(`DELETE FROM projects WHERE slug=$1`, p.Slug)
	return err
}

type dbStat struct {
	size  int64
	conns int
}

func (a *App) dbSizes() map[string]dbStat {
	out := map[string]dbStat{}
	sc, err := a.PG.Super("postgres")
	if err != nil {
		return out
	}
	rows, err := sc.Query(`SELECT d.datname, pg_database_size(d.oid),
		(SELECT count(*) FROM pg_stat_activity s WHERE s.datname=d.datname)
		FROM pg_database d WHERE NOT d.datistemplate`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var s int64
		var c int
		if rows.Scan(&n, &s, &c) == nil {
			out[n] = dbStat{s, c}
		}
	}
	return out
}

func (a *App) lastBackupAt(slug string) any {
	var t *time.Time
	a.Meta.QueryRow(`SELECT max(created_at) FROM backups WHERE project=$1`, slug).Scan(&t)
	if t == nil {
		return nil
	}
	return *t
}

// ---------------------------------------------------------------- system ops

type healthCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | error | info
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	FixOp   string `json:"fix_op,omitempty"`
}

func registerSystemOps(r *Registry) {
	r.add(&Op{Name: "system.health", Tag: "system", Method: "GET", Path: "/api/v1/system/health", Level: LevelRead,
		Summary:     "Health checks with hints and the operation that fixes each problem",
		Description: "Agents should call this first and after any incident. status is ok when nothing needs attention.",
		Run:         func(c *Ctx, a Args) (any, error) { return c.App.health(), nil }})

	r.add(&Op{Name: "system.proxy_config", Tag: "system", Method: "GET", Path: "/api/v1/system/proxy-config", Level: LevelRead,
		Summary:     "Reverse-proxy config (nginx or Caddy) for running behind a proxy",
		Description: "Prints a ready-to-paste config that forwards <domain> and *.<domain> to this server with the right headers, body limits, buffering and timeouts. Use with tls.mode=proxy.",
		Params:      []Param{{Name: "server", Type: "string", Enum: []string{"nginx", "caddy"}, Default: "nginx", Desc: "nginx or caddy"}},
		Run: func(c *Ctx, a Args) (any, error) {
			server := a.Str("server")
			if server == "" {
				server = "nginx"
			}
			conf, err := ProxyConfig(c.App.Cfg, server)
			if err != nil {
				return nil, errBad("no_domain", err.Error(), "")
			}
			out := map[string]any{"server": server, "config": conf, "tls_mode": c.App.Cfg.TLSMode(), "upstream": "http://" + c.App.Cfg.upstreamAddr()}
			if c.App.Cfg.TLSMode() != "proxy" {
				out["warning"] = "tls.mode is " + c.App.Cfg.TLSMode() + ": set it to proxy (ivarabase config set tls.mode proxy) and restart, or IvaraBase will try to bind 80/443 itself"
			}
			return out, nil
		}})

	r.add(&Op{Name: "system.info", Tag: "system", Method: "GET", Path: "/api/v1/system/info", Level: LevelRead,
		Summary: "Versions, paths, endpoints and counts",
		Run: func(c *Ctx, a Args) (any, error) {
			var projects, tokens, pending int
			c.App.Meta.QueryRow(`SELECT count(*) FROM projects`).Scan(&projects)
			c.App.Meta.QueryRow(`SELECT count(*) FROM tokens WHERE revoked_at IS NULL`).Scan(&tokens)
			c.App.Meta.QueryRow(`SELECT count(*) FROM approvals WHERE status='pending'`).Scan(&pending)
			var pgFull string
			if sc, err := c.App.PG.Super("postgres"); err == nil {
				sc.QueryRow(`SHOW server_version`).Scan(&pgFull)
			}
			return map[string]any{
				"version": Version, "os": runtime.GOOS, "arch": runtime.GOARCH,
				"uptime_s": int(time.Since(c.App.started).Seconds()), "data_dir": c.App.Cfg.DataDir,
				"postgres": map[string]any{"version": pgFull, "bin_dir": c.App.PG.BinDir, "port": c.App.PG.Port,
					"managed": !c.App.PG.External, "host": c.App.Cfg.Postgres.PublicHost},
				"deno": c.App.Fn.denoVersion(),
				"tls": map[string]any{"mode": c.App.Cfg.TLSMode(), "domain": c.App.Cfg.Domain, "postgres_tls": c.App.PG.tlsReady(),
					"project_url_example": c.App.projectURL("<slug>")},
				"endpoints": map[string]any{"ui": c.App.Cfg.PublicURL, "api": c.App.Cfg.PublicURL + "/api/v1", "openapi": c.App.Cfg.PublicURL + "/api/v1/openapi.json", "mcp": c.App.Cfg.PublicURL + "/mcp", "functions": c.App.Cfg.PublicURL + "/fn/{project}/{name}"},
				"projects":  projects, "active_tokens": tokens, "pending_approvals": pending,
				"operations": len(c.App.Ops.All()),
			}, nil
		}})

	r.add(&Op{Name: "me", Tag: "system", Method: "GET", Path: "/api/v1/me", Level: LevelRead,
		Summary: "Who am I: the caller's identity, level and project scope",
		Run: func(c *Ctx, a Args) (any, error) {
			p := *c.Principal
			p.LevelName = p.Level.String()
			return struct {
				Principal
				Version string `json:"version"`
				Repo    string `json:"repo"`
			}{p, Version, RepoURL}, nil
		}})
}

func (a *App) health() map[string]any {
	var checks []healthCheck
	add := func(h healthCheck) { checks = append(checks, h) }

	if err := a.PG.Ping(); err != nil {
		add(healthCheck{Name: "postgres", Status: "error", Message: "PostgreSQL is not reachable: " + err.Error(),
			Hint: "check " + a.Cfg.Path("logs", "postgres.log") + "; the watchdog retries every 15s"})
	} else {
		add(healthCheck{Name: "postgres", Status: "ok", Message: "PostgreSQL " + a.PG.Version + " is running"})
	}

	var fs syscall.Statfs_t
	if syscall.Statfs(a.Cfg.DataDir, &fs) == nil {
		total := float64(fs.Blocks) * float64(fs.Bsize)
		free := float64(fs.Bavail) * float64(fs.Bsize)
		pct := 100 * free / total
		h := healthCheck{Name: "disk", Status: "ok", Message: fmt.Sprintf("%.1f GB free (%.0f%%)", free/1e9, pct)}
		if pct < 5 {
			h.Status, h.Hint = "error", "free space now: delete old backups (backups.delete) or grow the disk"
			h.FixOp = "backups.delete"
		} else if pct < 15 {
			h.Status, h.Hint = "warn", "lower backup retention (backups.config_set keep_last) or grow the disk"
			h.FixOp = "backups.config_set"
		}
		add(h)
	}

	cfg := a.backupConfig()
	switch {
	case !cfg.Enabled || cfg.ScheduleHours <= 0:
		add(healthCheck{Name: "backup_schedule", Status: "warn", Message: "scheduled backups are off",
			Hint: "enable them with backups.config_set enabled=true schedule_hours=24", FixOp: "backups.config_set"})
	default:
		add(healthCheck{Name: "backup_schedule", Status: "ok", Message: fmt.Sprintf("every %dh, keeping the last %d per project", cfg.ScheduleHours, cfg.KeepLast)})
	}
	if !cfg.S3.configured() {
		add(healthCheck{Name: "backup_offsite", Status: "warn", Message: "backups exist only on this machine",
			Hint: "add an S3-compatible bucket with backups.config_set (s3_*) so a lost machine is recoverable", FixOp: "backups.config_set"})
	} else {
		add(healthCheck{Name: "backup_offsite", Status: "ok", Message: "off-site copies go to " + cfg.S3.Bucket})
	}
	var last backupRunResult
	if a.getSetting("backup_last_run", &last) && !last.OK {
		add(healthCheck{Name: "backup_last_run", Status: "error", Message: "the last scheduled backup run had failures: " + strings.Join(last.Errors, "; "),
			Hint: "run backups.create for the failing project to see the full error", FixOp: "backups.create"})
	}
	if cfg.Enabled && cfg.ScheduleHours > 0 {
		rows, err := a.Meta.Query(`SELECT p.slug, max(b.created_at) FROM projects p LEFT JOIN backups b ON b.project=p.slug GROUP BY p.slug`)
		if err == nil {
			for rows.Next() {
				var slug string
				var t *time.Time
				rows.Scan(&slug, &t)
				limit := time.Duration(cfg.ScheduleHours*2+1) * time.Hour
				if t == nil || time.Since(*t) > limit {
					add(healthCheck{Name: "backup_age:" + slug, Status: "warn", Message: fmt.Sprintf("project %s has no backup in the last %s", slug, limit),
						Hint: "run backups.create project=" + slug, FixOp: "backups.create"})
				}
			}
			rows.Close()
		}
	}
	var badVerify int
	a.Meta.QueryRow(`SELECT count(*) FROM backups b WHERE verify_ok = false AND created_at = (SELECT max(created_at) FROM backups WHERE project=b.project)`).Scan(&badVerify)
	if badVerify > 0 {
		add(healthCheck{Name: "backup_verify", Status: "error", Message: fmt.Sprintf("%d project(s) have a newest backup that failed verification", badVerify),
			Hint: "list with backups.list and re-run backups.verify; take a fresh backup", FixOp: "backups.verify"})
	}
	if l := a.Cfg.Postgres.Listen; !a.PG.External && l != "127.0.0.1" && l != "localhost" && l != "::1" && !a.PG.tlsReady() {
		hint := "set a domain (ivarabase config set domain ...) so a certificate is installed, or set postgres.listen_addresses back to 127.0.0.1"
		if a.Cfg.TLSMode() == "proxy" {
			hint = "behind a proxy, point IvaraBase at the proxy's certificate: ivarabase config set tls.cert_file ... and tls.key_file ..., then restart"
		}
		add(healthCheck{Name: "postgres_tls", Status: "warn", Message: "Postgres accepts remote connections without TLS: passwords and data cross the network unencrypted",
			Hint: hint})
	}
	if a.Cfg.Domain != "" && a.Cfg.TLSMode() == "auto" && a.Cfg.TLS.Email == "" {
		add(healthCheck{Name: "tls_email", Status: "info", Message: "no Let's Encrypt contact email set; you won't get expiry warnings",
			Hint: "ivarabase config set tls.email you@example.com"})
	}
	if a.Rest.binary() == "" {
		add(healthCheck{Name: "postgrest", Status: "warn", Message: "PostgREST is not installed, so project REST APIs (/rest/v1) cannot run",
			Hint: "macOS: brew install postgrest. Linux: static binary from github.com/PostgREST/postgrest/releases into /usr/local/bin"})
	}
	if rows, err := a.Meta.Query(`SELECT slug FROM projects ORDER BY slug`); err == nil {
		var slugs []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			slugs = append(slugs, s)
		}
		rows.Close()
		for _, s := range slugs {
			if open := a.tablesWithoutRLS(s); len(open) > 0 {
				add(healthCheck{Name: "rls:" + s, Status: "warn",
					Message: fmt.Sprintf("%s: %d public table(s) without row level security are fully readable and writable with the anon key: %s", s, len(open), strings.Join(open, ", ")),
					Hint:    "enable RLS (db.set_rls) and add policies, or revoke access from anon", FixOp: "db.set_rls"})
			}
		}
	}
	if a.Fn.denoPath() == "" {
		add(healthCheck{Name: "deno", Status: "warn", Message: "Deno is not installed, so functions cannot run",
			Hint: "install Deno (curl -fsSL https://deno.land/install.sh | sh) or set deno_path in config.json"})
	}
	var pending int
	a.Meta.QueryRow(`SELECT count(*) FROM approvals WHERE status='pending'`).Scan(&pending)
	if pending > 0 {
		add(healthCheck{Name: "approvals", Status: "info", Message: fmt.Sprintf("%d destructive request(s) waiting for the owner", pending),
			Hint: "the owner decides them on the Approvals page"})
	}

	overall := "ok"
	for _, c := range checks {
		if c.Status == "error" {
			overall = "error"
			break
		}
		if c.Status == "warn" {
			overall = "warn"
		}
	}
	return map[string]any{"status": overall, "checks": checks, "checked_at": time.Now().UTC()}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
