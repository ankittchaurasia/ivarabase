package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

const (
	tokenPrefix   = "ivb_"
	sessionCookie = "ivara_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// ---------------------------------------------------------------- sessions

func (a *App) Login(username, password, userAgent string) (string, error) {
	var id int64
	var hash string
	err := a.Meta.QueryRow(`SELECT id, password_hash FROM users WHERE username=$1`, username).Scan(&id, &hash)
	if err != nil || !checkPassword(hash, password) {
		return "", &APIError{Status: 401, Code: "invalid_credentials", Message: "wrong username or password"}
	}
	raw := randHex(32)
	if len(userAgent) > 200 {
		userAgent = userAgent[:200]
	}
	_, err = a.Meta.Exec(`INSERT INTO sessions(id,user_id,expires_at,user_agent) VALUES ($1,$2,$3,$4)`,
		sha256Hex(raw), id, time.Now().Add(sessionTTL), userAgent)
	return raw, err
}

func (a *App) Logout(raw string) {
	a.Meta.Exec(`DELETE FROM sessions WHERE id=$1`, sha256Hex(raw))
}

func (a *App) SessionPrincipal(raw string) (*Principal, bool) {
	var uid int64
	var name string
	err := a.Meta.QueryRow(`SELECT u.id, u.username FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.id=$1 AND s.expires_at > now()`, sha256Hex(raw)).Scan(&uid, &name)
	if err != nil {
		return nil, false
	}
	return &Principal{Kind: "user", Name: name, UserID: uid, Level: LevelAdmin, LevelName: "admin", AllowDestructive: true}, true
}

// ---------------------------------------------------------------- tokens

var lastUsedMu sync.Mutex
var lastUsedAt = map[int64]time.Time{}

func (a *App) TokenPrincipal(raw string) (*Principal, error) {
	if !strings.HasPrefix(raw, tokenPrefix) {
		return nil, &APIError{Status: 401, Code: "invalid_token", Message: "API tokens start with " + tokenPrefix}
	}
	var (
		id       int64
		name     string
		level    string
		projects []string
		allowD   bool
		expires  sql.NullTime
		revoked  sql.NullTime
	)
	err := a.Meta.QueryRow(`SELECT id,name,level,projects,allow_destructive,expires_at,revoked_at FROM tokens WHERE token_hash=$1`,
		sha256Hex(raw)).Scan(&id, &name, &level, pq.Array(&projects), &allowD, &expires, &revoked)
	if err != nil {
		return nil, &APIError{Status: 401, Code: "invalid_token", Message: "unknown API token"}
	}
	if revoked.Valid {
		return nil, &APIError{Status: 401, Code: "token_revoked", Message: "this token was revoked"}
	}
	if expires.Valid && expires.Time.Before(time.Now()) {
		return nil, &APIError{Status: 401, Code: "token_expired", Message: "this token expired", Hint: "create a new token in the UI"}
	}
	lv, _ := parseLevel(level)
	lastUsedMu.Lock()
	if time.Since(lastUsedAt[id]) > time.Minute {
		lastUsedAt[id] = time.Now()
		go a.Meta.Exec(`UPDATE tokens SET last_used_at=now() WHERE id=$1`, id)
	}
	lastUsedMu.Unlock()
	return &Principal{Kind: "token", Name: name, TokenID: id, Level: lv, LevelName: level, Projects: projects, AllowDestructive: allowD}, nil
}

// CreateToken is also used by the CLI bootstrap (`ivarabase token create`).
func (a *App) CreateToken(name string, level Level, projects []string, allowDestructive bool, expiresDays int) (map[string]any, error) {
	if name == "" {
		return nil, errBad("missing_parameter", "token name is required", "")
	}
	for _, p := range projects {
		if _, err := a.project(p); err != nil {
			return nil, err
		}
	}
	raw := tokenPrefix + randHex(24)
	var exp any
	if expiresDays > 0 {
		exp = time.Now().Add(time.Duration(expiresDays) * 24 * time.Hour)
	}
	if projects == nil {
		projects = []string{}
	}
	var id int64
	err := a.Meta.QueryRow(`INSERT INTO tokens(name,token_hash,prefix,level,projects,allow_destructive,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		name, sha256Hex(raw), raw[:len(tokenPrefix)+6], level.String(), pq.Array(projects), allowDestructive, exp).Scan(&id)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "name": name, "token": raw, "level": level.String(), "projects": projects,
		"allow_destructive": allowDestructive, "expires_at": exp,
		"note": "the token is shown only once - store it now",
	}, nil
}

// ---------------------------------------------------------------- approvals

func (a *App) createApproval(p *Principal, op *Op, args Args, preview any) (string, error) {
	id := "apr_" + randHex(8)
	clean := Args{}
	for k, v := range args {
		clean[k] = v
	}
	clean["confirm"] = true
	ab, _ := json.Marshal(clean)
	pb, _ := json.Marshal(preview)
	_, err := a.Meta.Exec(`INSERT INTO approvals(id,actor,token_id,op,project,args,preview) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, p.actor(), nullInt(p.TokenID), op.Name, args.Str("project"), ab, pb)
	return id, err
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// decideApproval runs (or rejects) a pending approval. The operation runs with
// the requesting token's level and project scope plus the owner's consent.
func (a *App) decideApproval(ctx context.Context, owner *Principal, id string, approve bool) (map[string]any, error) {
	tx, err := a.Meta.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var (
		opName, status, actor string
		argsRaw               []byte
		tokenID               sql.NullInt64
	)
	err = tx.QueryRow(`SELECT op,status,actor,args,token_id FROM approvals WHERE id=$1 FOR UPDATE`, id).
		Scan(&opName, &status, &actor, &argsRaw, &tokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound("approval_not_found", "no approval "+id, "")
	}
	if err != nil {
		return nil, err
	}
	if status != "pending" {
		return nil, errBad("approval_decided", "this approval was already "+status, "")
	}
	if !approve {
		if _, err := tx.Exec(`UPDATE approvals SET status='rejected', decided_at=now(), decided_by=$2 WHERE id=$1`, id, owner.Name); err != nil {
			return nil, err
		}
		return map[string]any{"id": id, "status": "rejected"}, tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE approvals SET status='running', decided_at=now(), decided_by=$2 WHERE id=$1`, id, owner.Name); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	var args Args
	json.Unmarshal(argsRaw, &args)
	runAs := &Principal{Kind: "user", Name: owner.Name + " (approved " + actor + ")", Level: LevelAdmin, AllowDestructive: true}
	if tokenID.Valid {
		var level string
		var projects []string
		if a.Meta.QueryRow(`SELECT level, projects FROM tokens WHERE id=$1 AND revoked_at IS NULL`, tokenID.Int64).
			Scan(&level, pq.Array(&projects)) != nil {
			a.Meta.Exec(`UPDATE approvals SET status='failed', error='requesting token no longer exists' WHERE id=$1`, id)
			return nil, errBad("token_gone", "the requesting token was revoked; nothing was run", "")
		}
		runAs.Level, _ = parseLevel(level)
		runAs.LevelName = level
		runAs.Projects = projects
	}
	res, runErr := a.Ops.Execute(ctx, runAs, opName, args)
	rb, _ := json.Marshal(res)
	if runErr != nil {
		a.Meta.Exec(`UPDATE approvals SET status='failed', error=$2 WHERE id=$1`, id, runErr.Error())
		return map[string]any{"id": id, "status": "failed", "error": AsAPIError(runErr)}, nil
	}
	a.Meta.Exec(`UPDATE approvals SET status='executed', result=$2 WHERE id=$1`, id, rb)
	return map[string]any{"id": id, "status": "executed", "result": res}, nil
}

// ---------------------------------------------------------------- access ops

func registerAccessOps(r *Registry) {
	r.add(&Op{Name: "tokens.list", Tag: "access", Method: "GET", Path: "/api/v1/tokens", Level: LevelAdmin,
		Summary: "List API tokens (values are never shown again)",
		Run: func(c *Ctx, a Args) (any, error) {
			rows, err := c.App.Meta.Query(`SELECT id,name,prefix,level,projects,allow_destructive,expires_at,created_at,last_used_at,revoked_at
				FROM tokens ORDER BY revoked_at IS NOT NULL, created_at DESC`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var (
					id                  int64
					name, prefix, level string
					projects            []string
					allowD              bool
					exp, last, revoked  sql.NullTime
					created             time.Time
				)
				if err := rows.Scan(&id, &name, &prefix, &level, pq.Array(&projects), &allowD, &exp, &created, &last, &revoked); err != nil {
					return nil, err
				}
				out = append(out, map[string]any{"id": id, "name": name, "prefix": prefix + "...", "level": level,
					"projects": projects, "allow_destructive": allowD, "expires_at": nt(exp), "created_at": created,
					"last_used_at": nt(last), "revoked": revoked.Valid})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "tokens.create", Tag: "access", Method: "POST", Path: "/api/v1/tokens", Level: LevelAdmin,
		Summary:     "Create a scoped API token for an agent, script or CI",
		Description: "Levels: read (inspect only), write (change data and deploy), admin (tokens and backup settings). Destructive calls from a token without allow_destructive become approval requests the owner decides in the UI.",
		Params: []Param{
			{Name: "name", Type: "string", Required: true, Desc: "label, e.g. claude-code or github-ci"},
			{Name: "level", Type: "string", Enum: []string{"read", "write", "admin"}, Default: "write"},
			{Name: "projects", Type: "array", Desc: "restrict to these project slugs (empty = all projects)"},
			{Name: "allow_destructive", Type: "boolean", Default: false, Desc: "let the token delete or replace things without owner approval"},
			{Name: "expires_days", Type: "integer", Default: 0, Desc: "0 = never expires"},
		},
		Run: func(c *Ctx, a Args) (any, error) {
			lv, _ := parseLevel(a.Str("level"))
			if lv > c.Principal.Level {
				return nil, errForbidden("cannot create a token above your own level", "")
			}
			return c.App.CreateToken(a.Str("name"), lv, a.StrList("projects"), a.Bool("allow_destructive", false), a.Int("expires_days", 0))
		}})

	r.add(&Op{Name: "tokens.revoke", Tag: "access", Method: "DELETE", Path: "/api/v1/tokens/{id}", Level: LevelAdmin,
		Summary: "Revoke an API token immediately",
		Params:  []Param{{Name: "id", Type: "integer", Required: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			res, err := c.App.Meta.Exec(`UPDATE tokens SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, a.Int("id", 0))
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, errNotFound("token_not_found", "no active token with that id", "")
			}
			return map[string]any{"revoked": a.Int("id", 0)}, nil
		}})

	r.add(&Op{Name: "approvals.list", Tag: "access", Method: "GET", Path: "/api/v1/approvals", Level: LevelRead,
		Summary: "List approval requests (destructive calls waiting for the owner)",
		Params:  []Param{{Name: "status", Type: "string", Enum: []string{"pending", "executed", "rejected", "failed", "all"}, Default: "pending"}},
		Run: func(c *Ctx, a Args) (any, error) {
			q := `SELECT id FROM approvals`
			var args []any
			if s := a.Str("status"); s != "all" {
				q += ` WHERE status=$1`
				args = append(args, s)
			}
			q += ` ORDER BY created_at DESC LIMIT 200`
			rows, err := c.App.Meta.Query(q, args...)
			if err != nil {
				return nil, err
			}
			var ids []string
			for rows.Next() {
				var id string
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			out := []map[string]any{}
			for _, id := range ids {
				if ap, err := c.App.approval(id); err == nil {
					if c.Principal.Kind == "token" && !c.Principal.canProject(fmt.Sprint(ap["project"])) && ap["project"] != "" {
						continue
					}
					out = append(out, ap)
				}
			}
			return out, nil
		}})

	r.add(&Op{Name: "approvals.get", Tag: "access", Method: "GET", Path: "/api/v1/approvals/{id}", Level: LevelRead,
		Summary: "Get one approval request, including its result once executed",
		Params:  []Param{{Name: "id", Type: "string", Required: true}},
		Run:     func(c *Ctx, a Args) (any, error) { return c.App.approval(a.Str("id")) }})

	r.add(&Op{Name: "approvals.decide", Tag: "access", Method: "POST", Path: "/api/v1/approvals/{id}/decide", Level: LevelAdmin,
		Summary:     "Approve (and run) or reject a pending request - owner only",
		Description: "Only a signed-in owner can decide; API tokens cannot approve their own requests.",
		Params: []Param{{Name: "id", Type: "string", Required: true},
			{Name: "decision", Type: "string", Required: true, Enum: []string{"approve", "reject"}}},
		Run: func(c *Ctx, a Args) (any, error) {
			if c.Principal.Kind != "user" {
				return nil, errForbidden("approvals can only be decided by the owner in the web UI", "open the Approvals page")
			}
			return c.App.decideApproval(c, c.Principal, a.Str("id"), a.Str("decision") == "approve")
		}})

	r.add(&Op{Name: "audit.list", Tag: "access", Method: "GET", Path: "/api/v1/audit", Level: LevelRead,
		Summary: "Audit trail of every change: who, what, when, result",
		Params: []Param{{Name: "project", Type: "string", Desc: "filter by project"},
			{Name: "limit", Type: "integer", Default: 100}},
		Run: func(c *Ctx, a Args) (any, error) {
			q := `SELECT id,at,actor,op,project,detail,ok,error FROM audit`
			var args []any
			if p := a.Str("project"); p != "" {
				q += ` WHERE project=$1`
				args = append(args, p)
			}
			q += fmt.Sprintf(` ORDER BY at DESC LIMIT %d`, clamp(a.Int("limit", 100), 1, 1000))
			rows, err := c.App.Meta.Query(q, args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var (
					id                          int64
					at                          time.Time
					actor, op, project, errText string
					detail                      []byte
					ok                          bool
				)
				if err := rows.Scan(&id, &at, &actor, &op, &project, &detail, &ok, &errText); err != nil {
					return nil, err
				}
				if c.Principal.Kind == "token" && project != "" && !c.Principal.canProject(project) {
					continue
				}
				out = append(out, map[string]any{"id": id, "at": at, "actor": actor, "op": op, "project": project,
					"detail": json.RawMessage(detail), "ok": ok, "error": errText})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "account.change_password", Tag: "access", Method: "POST", Path: "/api/v1/me/password", Level: LevelAdmin,
		Summary: "Change the owner password (signed-in owner only)",
		Params: []Param{{Name: "current_password", Type: "string", Required: true, Secret: true},
			{Name: "new_password", Type: "string", Required: true, Secret: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			if c.Principal.Kind != "user" {
				return nil, errForbidden("only the signed-in owner can change the password", "")
			}
			var hash string
			c.App.Meta.QueryRow(`SELECT password_hash FROM users WHERE id=$1`, c.Principal.UserID).Scan(&hash)
			if !checkPassword(hash, a.Str("current_password")) {
				return nil, &APIError{Status: 400, Code: "invalid_credentials", Message: "current password is wrong"}
			}
			if len(a.Str("new_password")) < 10 {
				return nil, errBad("weak_password", "use at least 10 characters", "")
			}
			if _, err := c.App.Meta.Exec(`UPDATE users SET password_hash=$2 WHERE id=$1`, c.Principal.UserID, hashPassword(a.Str("new_password"))); err != nil {
				return nil, err
			}
			return map[string]any{"changed": true}, nil
		}})
}

func (a *App) approval(id string) (map[string]any, error) {
	var (
		created                         time.Time
		actor, op, project, status, err string
		args, preview, result           []byte
		decidedAt                       sql.NullTime
		decidedBy                       sql.NullString
	)
	e := a.Meta.QueryRow(`SELECT created_at,actor,op,project,args,coalesce(preview,'null'),status,decided_at,decided_by,coalesce(result,'null'),error
		FROM approvals WHERE id=$1`, id).Scan(&created, &actor, &op, &project, &args, &preview, &status, &decidedAt, &decidedBy, &result, &err)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, errNotFound("approval_not_found", "no approval "+id, "")
	}
	if e != nil {
		return nil, e
	}
	return map[string]any{"id": id, "created_at": created, "actor": actor, "op": op, "project": project,
		"args": redactArgs(a, op, args), "preview": json.RawMessage(preview), "status": status,
		"decided_at": nt(decidedAt), "decided_by": decidedBy.String, "result": json.RawMessage(result), "error": err}, nil
}

func redactArgs(a *App, opName string, raw []byte) map[string]any {
	var m map[string]any
	json.Unmarshal(raw, &m)
	if op := a.Ops.Get(opName); op != nil {
		for _, p := range op.Params {
			if p.Secret {
				if _, ok := m[p.Name]; ok {
					m[p.Name] = "[redacted]"
				}
			}
		}
	}
	return m
}

func nt(t sql.NullTime) any {
	if t.Valid {
		return t.Time
	}
	return nil
}

func clamp(v, lo, hi int) int {
	return max(lo, min(hi, v))
}
