package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- errors

// APIError is the single error shape for every surface (REST, MCP, CLI, UI).
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Details any    `json:"details,omitempty"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

func errBad(code, msg, hint string) *APIError {
	return &APIError{Status: 400, Code: code, Message: msg, Hint: hint}
}

func errNotFound(code, msg, hint string) *APIError {
	return &APIError{Status: 404, Code: code, Message: msg, Hint: hint}
}

func errForbidden(msg, hint string) *APIError {
	return &APIError{Status: 403, Code: "forbidden", Message: msg, Hint: hint}
}

// AsAPIError converts any error to the public shape.
func AsAPIError(err error) *APIError {
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.Status == 0 {
			ae.Status = 400
		}
		return ae
	}
	return &APIError{Status: 500, Code: "internal", Message: err.Error()}
}

// ApprovalPending is returned when a token may not run a destructive
// operation on its own. It is not a failure: the owner decides.
type ApprovalPending struct {
	ID      string `json:"approval_id"`
	Op      string `json:"op"`
	Preview any    `json:"preview,omitempty"`
}

func (e *ApprovalPending) Error() string { return "approval required: " + e.ID }

// ---------------------------------------------------------------- principals

type Level int

const (
	LevelRead Level = iota + 1
	LevelWrite
	LevelAdmin
)

func (l Level) String() string {
	switch l {
	case LevelRead:
		return "read"
	case LevelWrite:
		return "write"
	case LevelAdmin:
		return "admin"
	}
	return "none"
}

func parseLevel(s string) (Level, bool) {
	switch s {
	case "read":
		return LevelRead, true
	case "write":
		return LevelWrite, true
	case "admin":
		return LevelAdmin, true
	}
	return 0, false
}

// Principal is who is calling: the owner (a browser session) or an API token.
type Principal struct {
	Kind             string   `json:"kind"` // "user" | "token" | "system"
	Name             string   `json:"name"`
	UserID           int64    `json:"user_id,omitempty"`
	TokenID          int64    `json:"token_id,omitempty"`
	Level            Level    `json:"-"`
	LevelName        string   `json:"level"`
	Projects         []string `json:"projects,omitempty"` // empty = all
	AllowDestructive bool     `json:"allow_destructive"`
}

func (p *Principal) actor() string {
	if p.Kind == "token" {
		return "token:" + p.Name
	}
	return p.Kind + ":" + p.Name
}

func (p *Principal) canProject(slug string) bool {
	if len(p.Projects) == 0 {
		return true
	}
	for _, s := range p.Projects {
		if s == slug {
			return true
		}
	}
	return false
}

var systemPrincipal = &Principal{Kind: "system", Name: "ivarabase", Level: LevelAdmin, LevelName: "admin", AllowDestructive: true}

// ---------------------------------------------------------------- operations

type Param struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // string | integer | boolean | object | array | any
	Required bool     `json:"required,omitempty"`
	Desc     string   `json:"description,omitempty"`
	Enum     []string `json:"enum,omitempty"`
	Default  any      `json:"default,omitempty"`
	Items    string   `json:"items,omitempty"` // element type for arrays
	Secret   bool     `json:"secret,omitempty"`
	Format   string   `json:"format,omitempty"` // "sql" | "typescript" | "json" (UI hint)
}

type Op struct {
	Name        string
	Summary     string
	Description string
	Tag         string
	Method      string
	Path        string // e.g. /api/v1/projects/{project}
	Level       Level
	Destructive bool
	// DestructiveIf marks a call destructive based on its arguments
	// (e.g. SQL containing DROP, restore in replace mode).
	DestructiveIf func(a Args) bool
	Params        []Param
	Run           func(c *Ctx, a Args) (any, error)
	Preview       func(c *Ctx, a Args) (any, error)
}

func (o *Op) projectScoped() bool {
	for _, p := range o.Params {
		if p.Name == "project" {
			return true
		}
	}
	return false
}

func (o *Op) isDestructive(a Args) bool {
	if o.Destructive {
		return true
	}
	return o.DestructiveIf != nil && o.DestructiveIf(a)
}

// allParams includes the implicit confirm flag on operations that can be destructive.
func (o *Op) allParams() []Param {
	ps := append([]Param{}, o.Params...) // never null in JSON
	if o.Destructive || o.DestructiveIf != nil {
		ps = append(ps, Param{Name: "confirm", Type: "boolean",
			Desc: "Set true to actually perform a destructive action. Without it the call returns a preview."})
	}
	return ps
}

// Ctx carries the caller and request context into handlers.
type Ctx struct {
	context.Context
	App       *App
	Principal *Principal
}

func (c *Ctx) require(l Level) error {
	if c.Principal.Level < l {
		return errForbidden(fmt.Sprintf("this call needs %s access; the token has %s", l, c.Principal.Level),
			"create a token with a higher level in the UI (Access tokens)")
	}
	return nil
}

// ---------------------------------------------------------------- args

type Args map[string]any

func (a Args) Str(k string) string {
	switch v := a[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (a Args) Int(k string, def int) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return def
}

func (a Args) Bool(k string, def bool) bool {
	if v, ok := a[k].(bool); ok {
		return v
	}
	return def
}

func (a Args) Obj(k string) map[string]any {
	if v, ok := a[k].(map[string]any); ok {
		return v
	}
	return nil
}

func (a Args) StrList(k string) []string {
	var out []string
	if arr, ok := a[k].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	if arr, ok := a[k].([]string); ok {
		out = append(out, arr...)
	}
	return out
}

// coerce validates args against the declared params, converting strings that
// came from URLs or the CLI into the declared types.
func (o *Op) coerce(in Args) (Args, error) {
	out := Args{}
	known := map[string]Param{}
	for _, p := range o.allParams() {
		known[p.Name] = p
	}
	for k, v := range in {
		p, ok := known[k]
		if !ok {
			names := make([]string, 0, len(known))
			for n := range known {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, errBad("unknown_parameter", fmt.Sprintf("%s does not accept %q", o.Name, k),
				"accepted parameters: "+strings.Join(names, ", "))
		}
		cv, err := coerceValue(p, v)
		if err != nil {
			return nil, errBad("invalid_parameter", fmt.Sprintf("parameter %q: %v", k, err), "")
		}
		out[k] = cv
	}
	for _, p := range o.allParams() {
		if _, ok := out[p.Name]; !ok {
			if p.Required {
				return nil, errBad("missing_parameter", fmt.Sprintf("%s requires %q", o.Name, p.Name), p.Desc)
			}
			if p.Default != nil {
				out[p.Name] = p.Default
			}
		}
		if len(p.Enum) > 0 {
			if s, ok := out[p.Name].(string); ok && s != "" {
				valid := false
				for _, e := range p.Enum {
					if e == s {
						valid = true
					}
				}
				if !valid {
					return nil, errBad("invalid_parameter", fmt.Sprintf("%q must be one of %s", p.Name, strings.Join(p.Enum, ", ")), "")
				}
			}
		}
	}
	return out, nil
}

func coerceValue(p Param, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	s, isStr := v.(string)
	switch p.Type {
	case "string":
		if isStr {
			return s, nil
		}
		switch n := v.(type) {
		case float64:
			return strconv.FormatFloat(n, 'f', -1, 64), nil
		case bool:
			return strconv.FormatBool(n), nil
		}
		return nil, errors.New("expected a string")
	case "integer":
		switch n := v.(type) {
		case float64:
			return n, nil
		case json.Number:
			f, err := n.Float64()
			return f, err
		}
		if isStr {
			f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return nil, errors.New("expected an integer")
			}
			return f, nil
		}
		return nil, errors.New("expected an integer")
	case "boolean":
		if b, ok := v.(bool); ok {
			return b, nil
		}
		if isStr {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true", "1", "yes", "on":
				return true, nil
			case "false", "0", "no", "off", "":
				return false, nil
			}
		}
		return nil, errors.New("expected true or false")
	case "object":
		if m, ok := v.(map[string]any); ok {
			return m, nil
		}
		if isStr {
			var m map[string]any
			if err := json.Unmarshal([]byte(s), &m); err != nil {
				return nil, errors.New("expected a JSON object")
			}
			return m, nil
		}
		return nil, errors.New("expected an object")
	case "array":
		if arr, ok := v.([]any); ok {
			return arr, nil
		}
		if isStr {
			s = strings.TrimSpace(s)
			if strings.HasPrefix(s, "[") {
				var arr []any
				if err := json.Unmarshal([]byte(s), &arr); err != nil {
					return nil, errors.New("expected a JSON array")
				}
				return arr, nil
			}
			var arr []any
			for _, part := range strings.Split(s, ",") {
				if part = strings.TrimSpace(part); part != "" {
					arr = append(arr, part)
				}
			}
			return arr, nil
		}
		return nil, errors.New("expected an array")
	}
	return v, nil
}

// ---------------------------------------------------------------- registry

type Registry struct {
	app   *App
	ops   map[string]*Op
	order []*Op
}

func NewRegistry(a *App) *Registry {
	r := &Registry{app: a, ops: map[string]*Op{}}
	registerSystemOps(r)
	registerProjectOps(r)
	registerDBOps(r)
	registerExtensionOps(r)
	registerFunctionOps(r)
	registerBackupOps(r)
	registerAccessOps(r)
	registerProjectAPIOps(r)
	registerAuthOps(r)
	registerStorageOps(r)
	registerMonitorOps(r)
	registerTypesOps(r)
	registerConfigOps(r)
	registerRealtimeOps(r)
	registerWebhookOps(r)
	registerLogOps(r)
	return r
}

func (r *Registry) add(o *Op) {
	if _, dup := r.ops[o.Name]; dup {
		panic("duplicate op " + o.Name)
	}
	r.ops[o.Name] = o
	r.order = append(r.order, o)
}

func (r *Registry) Get(name string) *Op { return r.ops[name] }

func (r *Registry) All() []*Op { return r.order }

// Execute is the single entry point used by REST, the ops endpoint, MCP,
// approvals and the CLI. Authorization, confirmation, approvals and audit
// all happen here, so no surface can skip them.
func (r *Registry) Execute(ctx context.Context, p *Principal, name string, in Args) (any, error) {
	op := r.ops[name]
	if op == nil {
		return nil, errNotFound("unknown_operation", fmt.Sprintf("no operation named %q", name), "GET /api/v1/ops lists all operations")
	}
	args, err := op.coerce(in)
	if err != nil {
		return nil, err
	}
	c := &Ctx{Context: ctx, App: r.app, Principal: p}
	if err := c.require(op.Level); err != nil {
		return nil, err
	}
	project := args.Str("project")
	if op.projectScoped() && project != "" && !p.canProject(project) {
		return nil, errForbidden(fmt.Sprintf("this token is not allowed to access project %q", project), "")
	}
	if op.isDestructive(args) {
		if !args.Bool("confirm", false) {
			var preview any
			if op.Preview != nil {
				preview, err = op.Preview(c, args)
				if err != nil {
					return nil, err
				}
			}
			return nil, &APIError{Status: 409, Code: "confirmation_required",
				Message: fmt.Sprintf("%s is destructive and was not run", op.Name),
				Hint:    "review the preview, then repeat the same call with confirm=true", Details: preview}
		}
		if p.Kind == "token" && !p.AllowDestructive {
			var preview any
			if op.Preview != nil {
				preview, _ = op.Preview(c, args)
			}
			id, err := r.app.createApproval(p, op, args, preview)
			if err != nil {
				return nil, err
			}
			return nil, &ApprovalPending{ID: id, Op: op.Name, Preview: preview}
		}
	}
	start := time.Now()
	res, err := op.Run(c, args)
	if op.Level >= LevelWrite || op.isDestructive(args) {
		r.app.audit(p, op, args, err, time.Since(start))
	}
	return res, err
}

// audit records every mutation. Secret and bulky params are redacted.
func (a *App) audit(p *Principal, op *Op, args Args, runErr error, d time.Duration) {
	detail := map[string]any{"ms": d.Milliseconds()}
	for _, prm := range op.allParams() {
		v, ok := args[prm.Name]
		if !ok || prm.Name == "project" {
			continue
		}
		switch {
		case prm.Secret:
			detail[prm.Name] = "[redacted]"
		case prm.Format == "sql" || prm.Format == "typescript":
			s := fmt.Sprint(v)
			if len(s) > 300 {
				s = s[:300] + "..."
			}
			detail[prm.Name] = s
		default:
			detail[prm.Name] = v
		}
	}
	b, _ := json.Marshal(detail)
	errText := ""
	if runErr != nil {
		errText = runErr.Error()
	}
	if _, err := a.Meta.Exec(`INSERT INTO audit(actor,op,project,detail,ok,error) VALUES ($1,$2,$3,$4,$5,$6)`,
		p.actor(), op.Name, args.Str("project"), b, runErr == nil, errText); err != nil {
		log.Printf("audit write failed: %v", err)
	}
}

// ---------------------------------------------------------------- schemas

func paramSchema(p Param) map[string]any {
	s := map[string]any{}
	switch p.Type {
	case "any":
	case "array":
		s["type"] = "array"
		it := p.Items
		if it == "" {
			it = "string"
		}
		s["items"] = map[string]any{"type": it}
	default:
		s["type"] = p.Type
	}
	if p.Desc != "" {
		s["description"] = p.Desc
	}
	if len(p.Enum) > 0 {
		s["enum"] = p.Enum
	}
	if p.Default != nil {
		s["default"] = p.Default
	}
	return s
}

// InputSchema is the JSON Schema of an operation's arguments (used by MCP and OpenAPI).
func (o *Op) InputSchema() map[string]any {
	props := map[string]any{}
	var req []string
	for _, p := range o.allParams() {
		props[p.Name] = paramSchema(p)
		if p.Required {
			req = append(req, p.Name)
		}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		s["required"] = req
	}
	return s
}

// Describe is the machine-readable catalog entry (GET /api/v1/ops).
func (o *Op) Describe() map[string]any {
	return map[string]any{
		"name": o.Name, "summary": o.Summary, "description": o.Description, "tag": o.Tag,
		"method": o.Method, "path": o.Path, "level": o.Level.String(),
		"destructive": o.Destructive, "may_be_destructive": o.DestructiveIf != nil,
		"params": o.allParams(), "input_schema": o.InputSchema(),
	}
}

func pathParams(path string) map[string]bool {
	out := map[string]bool{}
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out[seg[1:len(seg)-1]] = true
		}
	}
	return out
}

// OpenAPI renders an OpenAPI 3.1 document for every operation.
func (r *Registry) OpenAPI(serverURL string) map[string]any {
	paths := map[string]map[string]any{}
	for _, o := range r.order {
		pp := pathParams(o.Path)
		var params []any
		bodyProps := map[string]any{}
		var bodyReq []string
		for _, p := range o.allParams() {
			if pp[p.Name] {
				params = append(params, map[string]any{"name": p.Name, "in": "path", "required": true, "schema": paramSchema(p)})
				continue
			}
			if o.Method == "GET" {
				params = append(params, map[string]any{"name": p.Name, "in": "query", "required": p.Required, "schema": paramSchema(p)})
				continue
			}
			bodyProps[p.Name] = paramSchema(p)
			if p.Required {
				bodyReq = append(bodyReq, p.Name)
			}
		}
		desc := o.Description
		if o.Destructive || o.DestructiveIf != nil {
			desc += "\n\nDestructive: without `confirm: true` returns 409 `confirmation_required` with a preview. Tokens without allow_destructive get 202 `approval_required`."
		}
		entry := map[string]any{
			"operationId": o.Name, "summary": o.Summary, "description": strings.TrimSpace(desc),
			"tags": []string{o.Tag}, "x-ivarabase-level": o.Level.String(),
			"responses": map[string]any{
				"200":     map[string]any{"description": "OK", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{}}}},
				"default": map[string]any{"description": "Error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}},
			},
		}
		if len(params) > 0 {
			entry["parameters"] = params
		}
		if len(bodyProps) > 0 {
			schema := map[string]any{"type": "object", "properties": bodyProps}
			if len(bodyReq) > 0 {
				schema["required"] = bodyReq
			}
			entry["requestBody"] = map[string]any{"required": len(bodyReq) > 0,
				"content": map[string]any{"application/json": map[string]any{"schema": schema}}}
		}
		if paths[o.Path] == nil {
			paths[o.Path] = map[string]any{}
		}
		paths[o.Path][strings.ToLower(o.Method)] = entry
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{"title": "IvaraBase API", "version": Version,
			"contact":     map[string]any{"name": "IvaraBase", "url": RepoURL},
			"license":     map[string]any{"name": "AGPL-3.0-only", "url": RepoURL + "/blob/main/LICENSE"},
			"description": "Every operation is also callable as POST /api/v1/ops/{operationId} with a JSON body, and as an MCP tool at /mcp."},
		"servers":  []any{map[string]any{"url": serverURL}},
		"security": []any{map[string]any{"bearer": []any{}}},
		"paths":    paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}},
			"schemas": map[string]any{"Error": map[string]any{"type": "object", "properties": map[string]any{
				"error": map[string]any{"type": "object", "properties": map[string]any{
					"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
					"hint": map[string]any{"type": "string"}, "details": map[string]any{}}}}}},
		},
	}
}
