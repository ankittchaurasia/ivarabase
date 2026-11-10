package core

import (
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"github.com/lib/pq"
	"reflect"
	"sort"
	"strings"
)

// Config as code. A spec describes a project's desired state (migrations,
// functions, secrets, auth settings, storage buckets). config.plan diffs it
// against the server; config.apply runs the steps through the normal
// operations, so permissions, audit and approvals all still apply.

type specMigration struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

type specFunction struct {
	Files      map[string]string `json:"files"`
	Entrypoint string            `json:"entrypoint,omitempty"`
	VerifyJWT  *bool             `json:"verify_jwt,omitempty"`
	Schedule   string            `json:"schedule,omitempty"`
	TimeoutS   int               `json:"timeout_s,omitempty"`
	MemoryMB   int               `json:"memory_mb,omitempty"`
}

type specBucket struct {
	Public           bool     `json:"public"`
	FileSizeLimit    string   `json:"file_size_limit,omitempty"`
	AllowedMimeTypes []string `json:"allowed_mime_types,omitempty"`
}

type projectSpec struct {
	Project    string                  `json:"project"`
	Name       string                  `json:"name,omitempty"`
	Migrations []specMigration         `json:"migrations,omitempty"`
	Functions  map[string]specFunction `json:"functions,omitempty"`
	Secrets    map[string]string       `json:"secrets,omitempty"`
	Auth       map[string]any          `json:"auth,omitempty"`
	Storage    *struct {
		Buckets map[string]specBucket `json:"buckets"`
	} `json:"storage,omitempty"`
}

type planStep struct {
	Area   string `json:"area"`
	Action string `json:"action"` // create | update | apply | delete | error | note
	Target string `json:"target"`
	Detail string `json:"detail,omitempty"`
	Op     string `json:"-"`
	Args   Args   `json:"-"`
	Result any    `json:"result,omitempty"`
	Error  any    `json:"error,omitempty"`
}

func parseSpec(a Args) (*projectSpec, error) {
	raw, err := json.Marshal(a.Obj("spec"))
	if err != nil {
		return nil, err
	}
	var s projectSpec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, errBad("invalid_spec", "spec: "+err.Error(), "top-level keys: project, name, migrations, functions, secrets, auth, storage")
	}
	if err := validSlug(s.Project); err != nil {
		return nil, err
	}
	return &s, nil
}

// authFields maps spec keys to config_set parameters.
var authFlat = map[string]string{
	"site_url": "site_url", "redirect_urls": "redirect_urls", "disable_signup": "disable_signup", "confirm_email": "confirm_email",
	"jwt_expiry": "jwt_expiry", "password_min_length": "password_min_length", "email_rate_limit_per_hour": "email_rate_limit_per_hour",
}
var authNested = map[string]map[string]string{
	"smtp":   {"host": "smtp_host", "port": "smtp_port", "user": "smtp_user", "pass": "smtp_pass", "from_email": "smtp_from_email", "from_name": "smtp_from_name"},
	"google": {"enabled": "google_enabled", "client_id": "google_client_id", "client_secret": "google_client_secret"},
	"github": {"enabled": "github_enabled", "client_id": "github_client_id", "client_secret": "github_client_secret"},
	"apple": {"enabled": "apple_enabled", "client_id": "apple_client_id", "team_id": "apple_team_id", "key_id": "apple_key_id",
		"private_key": "apple_private_key", "bundle_ids": "apple_bundle_ids"},
}

// authCurrent flattens the stored auth config into config_set parameter names.
func authCurrent(c AuthConfig) map[string]any {
	return map[string]any{
		"site_url": c.SiteURL, "redirect_urls": c.RedirectURLs, "disable_signup": c.DisableSignup, "confirm_email": c.ConfirmEmail,
		"jwt_expiry": c.JWTExpiry, "password_min_length": c.PasswordMinLength, "email_rate_limit_per_hour": c.EmailRateLimit,
		"smtp_host": c.SMTP.Host, "smtp_port": c.SMTP.Port, "smtp_user": c.SMTP.User, "smtp_pass": c.SMTP.Pass,
		"smtp_from_email": c.SMTP.FromEmail, "smtp_from_name": c.SMTP.FromName,
		"google_enabled": c.Google.Enabled, "google_client_id": c.Google.ClientID, "google_client_secret": c.Google.ClientSecret,
		"github_enabled": c.GitHub.Enabled, "github_client_id": c.GitHub.ClientID, "github_client_secret": c.GitHub.ClientSecret,
		"apple_enabled": c.Apple.Enabled, "apple_client_id": c.Apple.ClientID, "apple_team_id": c.Apple.TeamID, "apple_key_id": c.Apple.KeyID,
		"apple_private_key": c.Apple.PrivateKey, "apple_bundle_ids": nonNilStrings(c.Apple.BundleIDs),
	}
}

func sameValue(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var va, vb any
	json.Unmarshal(ja, &va)
	json.Unmarshal(jb, &vb)
	return reflect.DeepEqual(va, vb)
}

func (a *App) computePlan(ctx context.Context, s *projectSpec, prune bool) ([]planStep, error) {
	var steps []planStep
	add := func(st planStep) { steps = append(steps, st) }
	_, perr := a.project(s.Project)
	exists := perr == nil
	if !exists {
		add(planStep{Area: "project", Action: "create", Target: s.Project, Op: "projects.create", Args: Args{"slug": s.Project, "name": s.Name}})
	} else if s.Name != "" {
		if p, _ := a.project(s.Project); p != nil && p.Name != s.Name {
			add(planStep{Area: "project", Action: "update", Target: s.Project, Detail: "name: " + p.Name + " -> " + s.Name, Op: "projects.update", Args: Args{"project": s.Project, "name": s.Name}})
		}
	}

	// migrations: applied ones are matched by name, or by identical SQL under another name
	appliedByName := map[string]string{}
	appliedSQL := map[string]bool{}
	if exists {
		rows, err := a.Meta.Query(`SELECT name, sql FROM migrations WHERE project=$1`, s.Project)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var n, q string
			rows.Scan(&n, &q)
			appliedByName[n] = q
			appliedSQL[strings.TrimSpace(q)] = true
		}
		rows.Close()
	}
	for _, m := range s.Migrations {
		if q, ok := appliedByName[m.Name]; ok {
			if strings.TrimSpace(q) != strings.TrimSpace(m.SQL) {
				add(planStep{Area: "migrations", Action: "error", Target: m.Name, Detail: "this migration was changed after it was applied - add a new migration instead"})
			}
			continue
		}
		if appliedSQL[strings.TrimSpace(m.SQL)] {
			continue // same SQL already applied under another name (e.g. exported with a numeric prefix)
		}
		detail := ""
		if sqlIsDestructive(m.SQL) {
			detail = "drops or empties data"
		}
		add(planStep{Area: "migrations", Action: "apply", Target: m.Name, Detail: detail, Op: "db.migrations_apply", Args: Args{"project": s.Project, "name": m.Name, "sql": m.SQL}})
	}

	// functions
	current := map[string]*fnRow{}
	if exists {
		rows, err := a.Meta.Query(`SELECT name FROM functions WHERE project=$1`, s.Project)
		if err == nil {
			var names []string
			for rows.Next() {
				var n string
				rows.Scan(&n)
				names = append(names, n)
			}
			rows.Close()
			for _, n := range names {
				if f, err := a.function(s.Project, n); err == nil {
					current[n] = f
				}
			}
		}
	}
	fnNames := make([]string, 0, len(s.Functions))
	for n := range s.Functions {
		fnNames = append(fnNames, n)
	}
	sort.Strings(fnNames)
	for _, n := range fnNames {
		f := s.Functions[n]
		entry := f.Entrypoint
		if entry == "" {
			entry = "index.ts"
		}
		verify := true
		if f.VerifyJWT != nil {
			verify = *f.VerifyJWT
		}
		timeout, mem := f.TimeoutS, f.MemoryMB
		if timeout == 0 {
			timeout = 30
		}
		if mem == 0 {
			mem = 128
		}
		args := Args{"project": s.Project, "name": n, "files": toAnyMap(f.Files), "entrypoint": entry, "verify_jwt": verify,
			"schedule": f.Schedule, "timeout_s": float64(timeout), "memory_mb": float64(mem)}
		cur := current[n]
		if cur == nil {
			add(planStep{Area: "functions", Action: "create", Target: n, Detail: fmt.Sprintf("%d file(s)", len(f.Files)), Op: "functions.deploy", Args: args})
			continue
		}
		var diff []string
		if !reflect.DeepEqual(cur.Files, f.Files) {
			diff = append(diff, "code")
		}
		if cur.Entrypoint != entry {
			diff = append(diff, "entrypoint")
		}
		if cur.VerifyJWT != verify {
			diff = append(diff, "verify_jwt")
		}
		if cur.Schedule != f.Schedule {
			diff = append(diff, "schedule")
		}
		if cur.TimeoutS != timeout || cur.MemoryMB != mem {
			diff = append(diff, "limits")
		}
		if len(diff) > 0 {
			add(planStep{Area: "functions", Action: "update", Target: n, Detail: strings.Join(diff, ", "), Op: "functions.deploy", Args: args})
		}
	}
	if prune {
		for n := range current {
			if _, ok := s.Functions[n]; !ok {
				add(planStep{Area: "functions", Action: "delete", Target: n, Op: "functions.delete", Args: Args{"project": s.Project, "name": n, "confirm": true}})
			}
		}
	}

	// secrets (compared on the server; values never leave it)
	stored := map[string]string{}
	if exists {
		rows, err := a.Meta.Query(`SELECT name, value_enc FROM secrets WHERE project=$1`, s.Project)
		if err == nil {
			for rows.Next() {
				var n, enc string
				rows.Scan(&n, &enc)
				if v, err := unseal(a.masterKey, enc); err == nil {
					stored[n] = v
				}
			}
			rows.Close()
		}
	}
	secretNames := make([]string, 0, len(s.Secrets))
	for n := range s.Secrets {
		secretNames = append(secretNames, n)
	}
	sort.Strings(secretNames)
	for _, n := range secretNames {
		v := s.Secrets[n]
		if old, ok := stored[n]; !ok || old != v {
			action := "update"
			if !ok {
				action = "create"
			}
			add(planStep{Area: "secrets", Action: action, Target: n, Op: "secrets.set", Args: Args{"project": s.Project, "name": n, "value": v}})
		}
	}
	if prune {
		for n := range stored {
			if _, ok := s.Secrets[n]; !ok {
				add(planStep{Area: "secrets", Action: "delete", Target: n, Op: "secrets.delete", Args: Args{"project": s.Project, "name": n}})
			}
		}
	}

	// auth settings
	if len(s.Auth) > 0 {
		cur := authCurrent(a.authConfig(s.Project))
		want := Args{"project": s.Project}
		var changed []string
		consider := func(param string, v any) {
			if sv, ok := v.(string); ok && param == "apple_private_key" && sv != "" {
				if norm, _, err := normalizeAppleKey(sv); err == nil {
					v = norm // stored canonical; compare like with like
				}
			}
			if !sameValue(cur[param], v) {
				want[param] = v
				if strings.HasSuffix(param, "_secret") || strings.HasSuffix(param, "_pass") || strings.HasSuffix(param, "_private_key") {
					changed = append(changed, param+" (secret)")
				} else {
					changed = append(changed, param)
				}
			}
		}
		for k, v := range s.Auth {
			if p, ok := authFlat[k]; ok {
				consider(p, v)
				continue
			}
			if sub, ok := authNested[k]; ok {
				m, _ := v.(map[string]any)
				for sk, sv := range m {
					if p, ok := sub[sk]; ok {
						consider(p, sv)
					} else {
						return nil, errBad("invalid_spec", fmt.Sprintf("unknown auth.%s.%s", k, sk), "")
					}
				}
				continue
			}
			if k == "templates" {
				want["templates"] = v
				changed = append(changed, "templates")
				continue
			}
			return nil, errBad("invalid_spec", "unknown auth setting "+k, "")
		}
		if len(changed) > 0 {
			sort.Strings(changed)
			add(planStep{Area: "auth", Action: "update", Target: "settings", Detail: strings.Join(changed, ", "), Op: "auth.config_set", Args: want})
		}
	}

	// storage buckets
	if s.Storage != nil {
		cur := map[string]map[string]any{}
		if exists {
			if db, err := a.projectDB(s.Project); err == nil {
				rows, err := db.QueryContext(ctx, `SELECT id, public, coalesce(file_size_limit,0), coalesce(allowed_mime_types,'{}') FROM storage.buckets`)
				if err == nil {
					for rows.Next() {
						var id string
						var pub bool
						var lim int64
						var mimes []string
						rows.Scan(&id, &pub, &lim, pqArray(&mimes))
						cur[id] = map[string]any{"public": pub, "limit": lim, "mimes": mimes}
					}
					rows.Close()
				}
			}
		}
		ids := make([]string, 0, len(s.Storage.Buckets))
		for id := range s.Storage.Buckets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			b := s.Storage.Buckets[id]
			lim := int64(0)
			if b.FileSizeLimit != "" {
				v, err := parseSize(b.FileSizeLimit)
				if err != nil {
					return nil, err
				}
				if n, ok := v.(int64); ok {
					lim = n
				}
			}
			mimes := b.AllowedMimeTypes
			if mimes == nil {
				mimes = []string{}
			}
			args := Args{"project": s.Project, "bucket": id, "public": b.Public, "file_size_limit": b.FileSizeLimit, "allowed_mime_types": toAnySlice(mimes)}
			c, ok := cur[id]
			if !ok {
				add(planStep{Area: "storage", Action: "create", Target: id, Op: "storage.bucket_create", Args: args})
				continue
			}
			curMimes, _ := c["mimes"].([]string)
			if curMimes == nil {
				curMimes = []string{}
			}
			if c["public"] != b.Public || c["limit"] != lim || !reflect.DeepEqual(curMimes, mimes) {
				add(planStep{Area: "storage", Action: "update", Target: id, Op: "storage.bucket_update", Args: args})
			}
		}
		if prune {
			for id := range cur {
				if _, ok := s.Storage.Buckets[id]; !ok {
					add(planStep{Area: "storage", Action: "delete", Target: id, Detail: "deletes every file in it", Op: "storage.bucket_delete", Args: Args{"project": s.Project, "bucket": id, "confirm": true}})
				}
			}
		}
	}
	return steps, nil
}

func pqArray(dst *[]string) any { return pq.Array(dst) }

func toAnyMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func summarize(steps []planStep) map[string]int {
	out := map[string]int{}
	for _, s := range steps {
		out[s.Action]++
	}
	return out
}

func registerConfigOps(r *Registry) {
	specParam := Param{Name: "spec", Type: "object", Required: true, Secret: true, Desc: "desired state: {project, name, migrations: [{name, sql}], functions: {name: {files, entrypoint, verify_jwt, schedule, timeout_s, memory_mb}}, secrets: {NAME: value}, auth: {...}, storage: {buckets: {id: {public, file_size_limit, allowed_mime_types}}}}"}
	pruneParam := Param{Name: "prune", Type: "boolean", Default: false, Desc: "also delete functions, secrets and buckets that are not in the spec"}

	r.add(&Op{Name: "config.plan", Tag: "config", Method: "POST", Path: "/api/v1/config/plan", Level: LevelRead,
		Summary:     "Show what config.apply would change (nothing is changed)",
		Description: "Migrations already applied are skipped (matched by name, or by identical SQL). A migration edited after it was applied is reported as an error.",
		Params:      []Param{specParam, pruneParam},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := parseSpec(a)
			if err != nil {
				return nil, err
			}
			if !c.Principal.canProject(s.Project) {
				return nil, errForbidden("not allowed for this project", "")
			}
			steps, err := c.App.computePlan(c, s, a.Bool("prune", false))
			if err != nil {
				return nil, err
			}
			if steps == nil {
				steps = []planStep{}
			}
			return map[string]any{"project": s.Project, "changes": steps, "summary": summarize(steps), "up_to_date": len(steps) == 0}, nil
		}})

	r.add(&Op{Name: "config.apply", Tag: "config", Method: "POST", Path: "/api/v1/config/apply", Level: LevelWrite,
		Summary:       "Make the project match the spec: create, migrate, deploy, set secrets/auth/buckets",
		Description:   "Steps run in order through the normal operations (same permissions, audit and approvals); it stops at the first failure. With prune=true deletions are included, which makes the call destructive.",
		DestructiveIf: func(a Args) bool { return a.Bool("prune", false) },
		Params:        []Param{specParam, pruneParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			return c.App.Ops.Get("config.plan").Run(c, Args{"spec": a["spec"], "prune": a.Bool("prune", false)})
		},
		Run: func(c *Ctx, a Args) (any, error) {
			s, err := parseSpec(a)
			if err != nil {
				return nil, err
			}
			if !c.Principal.canProject(s.Project) {
				return nil, errForbidden("not allowed for this project", "")
			}
			steps, err := c.App.computePlan(c, s, a.Bool("prune", false))
			if err != nil {
				return nil, err
			}
			for _, st := range steps {
				if st.Action == "error" {
					return map[string]any{"applied": 0, "changes": steps, "stopped": "the plan has errors; nothing was changed"}, nil
				}
			}
			done := 0
			for i := range steps {
				st := &steps[i]
				if st.Op == "" {
					continue
				}
				res, err := c.App.Ops.Execute(c, c.Principal, st.Op, st.Args)
				if err != nil {
					var ap *ApprovalPending
					if errors.As(err, &ap) {
						st.Result = map[string]any{"approval_required": ap.ID}
						return map[string]any{"applied": done, "changes": steps, "stopped": "waiting for owner approval of " + st.Op + " " + st.Target + " (" + ap.ID + "); run apply again once approved"}, nil
					}
					st.Error = AsAPIError(err)
					return map[string]any{"applied": done, "changes": steps, "stopped": "failed at " + st.Area + " " + st.Target}, nil
				}
				if st.Area == "secrets" || st.Area == "auth" {
					res = map[string]any{"ok": true} // never echo secret values
				}
				st.Result = res
				done++
			}
			if steps == nil {
				steps = []planStep{}
			}
			return map[string]any{"applied": done, "changes": steps, "up_to_date": done == 0}, nil
		}})

	r.add(&Op{Name: "config.export", Tag: "config", Method: "GET", Path: "/api/v1/config/export", Level: LevelWrite,
		Summary:     "The project's current state as a spec (secrets as ${NAME} placeholders)",
		Description: "Used by `ivarabase export` to write ivarabase.yaml, migrations/ and functions/ for an existing project.",
		Params:      []Param{{Name: "project", Type: "string", Required: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			p, err := c.App.project(slug)
			if err != nil {
				return nil, err
			}
			spec := map[string]any{"project": slug, "name": p.Name}
			var migs []map[string]any
			rows, err := c.App.Meta.Query(`SELECT version, name, sql FROM migrations WHERE project=$1 ORDER BY version`, slug)
			if err == nil {
				for rows.Next() {
					var v, n, q string
					rows.Scan(&v, &n, &q)
					migs = append(migs, map[string]any{"version": v, "name": n, "sql": q})
				}
				rows.Close()
			}
			spec["migrations"] = migs
			fns := map[string]any{}
			if rows, err := c.App.Meta.Query(`SELECT name FROM functions WHERE project=$1 ORDER BY name`, slug); err == nil {
				var names []string
				for rows.Next() {
					var n string
					rows.Scan(&n)
					names = append(names, n)
				}
				rows.Close()
				for _, n := range names {
					f, err := c.App.function(slug, n)
					if err != nil {
						continue
					}
					fns[n] = map[string]any{"files": f.Files, "entrypoint": f.Entrypoint, "verify_jwt": f.VerifyJWT, "schedule": f.Schedule,
						"timeout_s": f.TimeoutS, "memory_mb": f.MemoryMB}
				}
			}
			spec["functions"] = fns
			secrets := map[string]any{}
			if rows, err := c.App.Meta.Query(`SELECT name FROM secrets WHERE project=$1 ORDER BY name`, slug); err == nil {
				for rows.Next() {
					var n string
					rows.Scan(&n)
					secrets[n] = "${" + n + "}"
				}
				rows.Close()
			}
			spec["secrets"] = secrets
			ac := c.App.authConfig(slug)
			ph := func(v, name string) string {
				if v == "" {
					return ""
				}
				return "${" + name + "}"
			}
			spec["auth"] = map[string]any{
				"site_url": ac.SiteURL, "redirect_urls": ac.RedirectURLs, "disable_signup": ac.DisableSignup, "confirm_email": ac.ConfirmEmail,
				"jwt_expiry": ac.JWTExpiry, "password_min_length": ac.PasswordMinLength, "email_rate_limit_per_hour": ac.EmailRateLimit,
				"smtp": map[string]any{"host": ac.SMTP.Host, "port": ac.SMTP.Port, "user": ac.SMTP.User, "pass": ph(ac.SMTP.Pass, "SMTP_PASS"),
					"from_email": ac.SMTP.FromEmail, "from_name": ac.SMTP.FromName},
				"google": map[string]any{"enabled": ac.Google.Enabled, "client_id": ac.Google.ClientID, "client_secret": ph(ac.Google.ClientSecret, "GOOGLE_CLIENT_SECRET")},
				"github": map[string]any{"enabled": ac.GitHub.Enabled, "client_id": ac.GitHub.ClientID, "client_secret": ph(ac.GitHub.ClientSecret, "GITHUB_CLIENT_SECRET")},
				"apple": map[string]any{"enabled": ac.Apple.Enabled, "client_id": ac.Apple.ClientID, "team_id": ac.Apple.TeamID, "key_id": ac.Apple.KeyID,
					"private_key": ph(ac.Apple.PrivateKey, "APPLE_PRIVATE_KEY"), "bundle_ids": nonNilStrings(ac.Apple.BundleIDs)},
			}
			buckets := map[string]any{}
			if db, err := c.App.projectDB(slug); err == nil {
				if rows, err := db.QueryContext(c, `SELECT id, public, file_size_limit, coalesce(allowed_mime_types,'{}') FROM storage.buckets ORDER BY id`); err == nil {
					for rows.Next() {
						var id string
						var pub bool
						var lim *int64
						var mimes []string
						rows.Scan(&id, &pub, &lim, pqArray(&mimes))
						b := map[string]any{"public": pub}
						if lim != nil {
							b["file_size_limit"] = fmt.Sprint(*lim)
						}
						if len(mimes) > 0 {
							b["allowed_mime_types"] = mimes
						}
						buckets[id] = b
					}
					rows.Close()
				}
			}
			spec["storage"] = map[string]any{"buckets": buckets}
			return spec, nil
		}})
}
