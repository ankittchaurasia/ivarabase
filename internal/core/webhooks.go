package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lib/pq"
)

// Database webhooks (like Supabase Database Webhooks).
//
// Everything lives in the project's own database, in the schema
// ivarabase_hooks owned by the project role (like the auth schema) and with no
// grants to anon / authenticated / service_role, so the API can't read it:
//
//	hooks       configuration (one row per webhook, including its signing secret)
//	events      the queue: an AFTER ROW trigger inserts the Supabase-style payload
//	deliveries  one row per attempt (status, duration, response excerpt, error)
//
// Because it is ordinary project data, webhooks travel with pg_dump into
// backups and come back with restores. The trigger only inserts a row and sends
// a NOTIFY, so the user's transaction never waits for an HTTP call; an event
// that rolls back with the transaction is never sent.
//
// A dispatcher goroutine per project LISTENs for the NOTIFY (and polls as a
// fallback), delivers at least once, one event at a time per hook (best-effort
// order), and retries failures with exponential backoff: 5 attempts over about
// an hour. All SQL on the project database runs as the project role, never as
// the superuser: the project owns these tables and could attach triggers to
// them, which must not run with superuser rights.
//
// Signing. Every request carries
//
//	X-Ivarabase-Timestamp: <unix seconds>
//	X-Ivarabase-Signature: sha256=<hex HMAC-SHA256(secret, "<timestamp>.<raw body>")>
//	X-Ivarabase-Event-Id:  <id>   (stable across retries: use it to deduplicate)
//
// Receivers recompute the HMAC over the timestamp, a dot and the exact body
// bytes, compare in constant time, and reject timestamps older than a few
// minutes (replay protection; that's why the timestamp is signed too).
//
// SSRF. URL targets that resolve to loopback, private, link-local, CGNAT or
// other non-public addresses are refused when the hook is saved and again at
// connect time (the dialer checks the address it actually connects to, so DNS
// rebinding doesn't help). Admins can allow them per server with
// "webhooks_allow_private_targets": true in config.json or
// IVARA_WEBHOOKS_ALLOW_PRIVATE=1. Function targets call the local function
// runtime directly with the project's service_role key.

const (
	hooksNS          = "ivarabase_hooks"
	hookChannel      = "ivarabase_hooks"
	hookMaxAttempts  = 5
	hookKeepDays     = 7
	hookKeepPerHook  = 1000
	hookMaxInflight  = 8 // per project; at most one per hook
	hookPollInterval = 10 * time.Second
)

// delays after failed attempt 1..4 (then the event is marked failed): ~61 min in all.
var hookRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 40 * time.Minute}

// hookRetryScale shrinks the retry delays for tests (IVARA_DEV_WEBHOOK_RETRY_SCALE=0.02).
func hookRetryScale() float64 {
	if v, err := strconv.ParseFloat(os.Getenv("IVARA_DEV_WEBHOOK_RETRY_SCALE"), 64); err == nil && v > 0 && v <= 1 {
		return v
	}
	return 1
}

func hookRetryDelay(attempt int) time.Duration {
	d := hookRetryDelays[min(max(attempt, 1), len(hookRetryDelays))-1]
	d = time.Duration(float64(d) * hookRetryScale())
	return d + time.Duration(rand.Int63n(int64(d)/10+1)) // jitter so a recovering receiver isn't hit all at once
}

const hooksSchemaSQL = `
CREATE SCHEMA IF NOT EXISTS ivarabase_hooks;
REVOKE ALL ON SCHEMA ivarabase_hooks FROM PUBLIC;
CREATE TABLE IF NOT EXISTS ivarabase_hooks.hooks (
	id bigserial PRIMARY KEY,
	name text NOT NULL UNIQUE,
	schema_name text NOT NULL,
	table_name text NOT NULL,
	events text[] NOT NULL,
	target_type text NOT NULL CHECK (target_type IN ('url','function')),
	url text NOT NULL DEFAULT '',
	method text NOT NULL DEFAULT 'POST',
	headers jsonb NOT NULL DEFAULT '{}',
	function_name text NOT NULL DEFAULT '',
	timeout_ms int NOT NULL DEFAULT 10000,
	secret text NOT NULL,
	enabled boolean NOT NULL DEFAULT true,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS ivarabase_hooks.events (
	id bigserial PRIMARY KEY,
	hook_id bigint NOT NULL REFERENCES ivarabase_hooks.hooks(id) ON DELETE CASCADE,
	payload jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	status text NOT NULL DEFAULT 'pending',
	attempts int NOT NULL DEFAULT 0,
	next_attempt_at timestamptz NOT NULL DEFAULT now(),
	last_error text NOT NULL DEFAULT '',
	test boolean NOT NULL DEFAULT false);
CREATE INDEX IF NOT EXISTS events_due ON ivarabase_hooks.events (next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS events_hook ON ivarabase_hooks.events (hook_id, id DESC);
CREATE TABLE IF NOT EXISTS ivarabase_hooks.deliveries (
	id bigserial PRIMARY KEY,
	event_id bigint NOT NULL REFERENCES ivarabase_hooks.events(id) ON DELETE CASCADE,
	hook_id bigint NOT NULL,
	attempt int NOT NULL,
	at timestamptz NOT NULL DEFAULT now(),
	status_code int NOT NULL DEFAULT 0,
	duration_ms int NOT NULL DEFAULT 0,
	ok boolean NOT NULL,
	error text NOT NULL DEFAULT '',
	response text NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS deliveries_hook ON ivarabase_hooks.deliveries (hook_id, id DESC);
CREATE INDEX IF NOT EXISTS deliveries_event ON ivarabase_hooks.deliveries (event_id);
-- SECURITY DEFINER (as the project role) so rows written through the API by
-- anon/authenticated can enqueue without those roles seeing the queue. The
-- EXISTS check keeps a stale trigger (hook deleted or disabled) harmless.
CREATE OR REPLACE FUNCTION ivarabase_hooks.enqueue() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $fn$
DECLARE hid bigint := TG_ARGV[0]::bigint;
BEGIN
	INSERT INTO ivarabase_hooks.events (hook_id, payload)
	SELECT hid, jsonb_build_object('type', TG_OP, 'table', TG_TABLE_NAME, 'schema', TG_TABLE_SCHEMA,
		'record', CASE WHEN TG_OP = 'DELETE' THEN NULL ELSE to_jsonb(NEW) END,
		'old_record', CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE to_jsonb(OLD) END)
	WHERE EXISTS (SELECT 1 FROM ivarabase_hooks.hooks WHERE id = hid AND enabled);
	IF FOUND THEN PERFORM pg_notify('ivarabase_hooks', ''); END IF;
	RETURN NULL;
END $fn$;
REVOKE ALL ON FUNCTION ivarabase_hooks.enqueue() FROM PUBLIC;
`

var hookEventNames = []string{"INSERT", "UPDATE", "DELETE"}

func hookTrigger(id int64) string { return "ivarabase_hook_" + strconv.FormatInt(id, 10) }

// ---------------------------------------------------------------- config rows

type hookRow struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Schema    string            `json:"schema"`
	Table     string            `json:"table"`
	Events    []string          `json:"events"`
	Target    string            `json:"target"` // url | function
	URL       string            `json:"url,omitempty"`
	Method    string            `json:"method,omitempty"`
	Headers   map[string]string `json:"headers"`
	Function  string            `json:"function,omitempty"`
	TimeoutMS int               `json:"timeout_ms"`
	Secret    string            `json:"-"`
	Enabled   bool              `json:"enabled"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

const hookCols = `id, name, schema_name, table_name, events, target_type, url, method, headers, function_name, timeout_ms, secret, enabled, created_at, updated_at`

func scanHook(s rowScanner, extra ...any) (*hookRow, error) {
	var h hookRow
	var hdr []byte
	dst := append([]any{&h.ID, &h.Name, &h.Schema, &h.Table, pq.Array(&h.Events), &h.Target, &h.URL, &h.Method, &hdr,
		&h.Function, &h.TimeoutMS, &h.Secret, &h.Enabled, &h.CreatedAt, &h.UpdatedAt}, extra...)
	if err := s.Scan(dst...); err != nil {
		return nil, err
	}
	h.Headers = map[string]string{}
	json.Unmarshal(hdr, &h.Headers)
	return &h, nil
}

func (h *hookRow) public(revealSecret bool) map[string]any {
	b, _ := json.Marshal(h)
	var m map[string]any
	json.Unmarshal(b, &m)
	m["table"] = h.Schema + "." + h.Table
	m["secret_hint"] = h.Secret[:min(len(h.Secret), 12)] + "…"
	if revealSecret {
		m["secret"] = h.Secret
	}
	return m
}

func hooksInstalled(ctx context.Context, db *sql.DB) bool {
	var ok bool
	db.QueryRowContext(ctx, `SELECT to_regclass('ivarabase_hooks.hooks') IS NOT NULL`).Scan(&ok)
	return ok
}

func (a *App) ensureHooksSchema(ctx context.Context, slug string) (*sql.DB, error) {
	db, err := a.projectDB(slug)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, hooksSchemaSQL); err != nil {
		return nil, fmt.Errorf("webhooks schema: %w", err)
	}
	return db, nil
}

func (a *App) hook(ctx context.Context, db *sql.DB, name string) (*hookRow, error) {
	if !hooksInstalled(ctx, db) {
		return nil, errNotFound("webhook_not_found", fmt.Sprintf("no webhook named %q", name), "call webhooks.list")
	}
	h, err := scanHook(db.QueryRowContext(ctx, `SELECT `+hookCols+` FROM ivarabase_hooks.hooks WHERE name=$1`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound("webhook_not_found", fmt.Sprintf("no webhook named %q", name), "call webhooks.list")
	}
	return h, err
}

// syncHookTrigger drops the hook's trigger wherever it is (the table may have
// been renamed or changed) and creates it again on the configured table when
// the hook is enabled. Runs as the project role, which owns the tables.
func syncHookTrigger(ctx context.Context, db *sql.DB, h *hookRow) error {
	rows, err := db.QueryContext(ctx, `SELECT tgrelid::regclass::text FROM pg_trigger WHERE tgname=$1 AND NOT tgisinternal`, hookTrigger(h.ID))
	if err != nil {
		return err
	}
	var on []string
	for rows.Next() {
		var t string
		rows.Scan(&t)
		on = append(on, t)
	}
	rows.Close()
	for _, t := range on {
		if _, err := db.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+pq.QuoteIdentifier(hookTrigger(h.ID))+` ON `+t); err != nil {
			return err
		}
	}
	if !h.Enabled {
		return nil
	}
	if err := checkHookTable(ctx, db, h.Schema, h.Table); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER %s AFTER %s ON %s FOR EACH ROW EXECUTE FUNCTION ivarabase_hooks.enqueue('%d')`,
		pq.QuoteIdentifier(hookTrigger(h.ID)), strings.Join(h.Events, " OR "), qualified(h.Schema, h.Table), h.ID))
	if err != nil {
		return &APIError{Status: 400, Code: "trigger_failed", Message: "could not create the webhook trigger: " + err.Error(),
			Hint: "the project role must own the table"}
	}
	return nil
}

func checkHookTable(ctx context.Context, db *sql.DB, schema, table string) error {
	var kind string
	err := db.QueryRowContext(ctx, `SELECT c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2`,
		schema, table).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return errNotFound("table_not_found", fmt.Sprintf("table %s.%s does not exist", schema, table), "call db.tables")
	}
	if err != nil {
		return err
	}
	if kind != "r" && kind != "p" {
		return errBad("not_a_table", fmt.Sprintf("%s.%s is not a table", schema, table), "webhooks watch tables, not views")
	}
	return nil
}

// ---------------------------------------------------------------- validation

var (
	hookNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	headerNameRe = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,128}$")
)

func parseHookTable(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	schema, table, ok := strings.Cut(s, ".")
	if !ok {
		schema, table = "public", s
	}
	if schema == "" || table == "" {
		return "", "", errBad("invalid_parameter", "table must be schema.table or table", "e.g. public.orders")
	}
	if strings.HasPrefix(schema, "pg_") || schema == "information_schema" || strings.HasPrefix(schema, "ivarabase_") {
		return "", "", errBad("invalid_parameter", fmt.Sprintf("tables in schema %q can't have webhooks", schema), "")
	}
	return schema, table, nil
}

func parseHookEvents(list []string) ([]string, error) {
	seen := map[string]bool{}
	for _, e := range list {
		seen[strings.ToUpper(strings.TrimSpace(e))] = true
	}
	var out []string
	for _, e := range hookEventNames {
		if seen[e] {
			out = append(out, e)
			delete(seen, e)
		}
	}
	if len(out) == 0 || len(seen) > 0 {
		return nil, errBad("invalid_parameter", "events must be a non-empty list of INSERT, UPDATE, DELETE", "")
	}
	return out, nil
}

func parseHookHeaders(m map[string]any) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m {
		s := fmt.Sprint(v)
		lk := strings.ToLower(k)
		if !headerNameRe.MatchString(k) || strings.ContainsAny(s, "\r\n\x00") {
			return nil, errBad("invalid_header", fmt.Sprintf("header %q is not valid", k), "")
		}
		if strings.HasPrefix(lk, "x-ivarabase-") || lk == "host" || lk == "content-length" || lk == "transfer-encoding" || lk == "connection" {
			return nil, errBad("invalid_header", fmt.Sprintf("header %q is set by IvaraBase", k), "")
		}
		out[k] = s
	}
	return out, nil
}

var errHookPrivate = errors.New("private address refused")

func privateTargetHint() string {
	return `webhook URLs may not point at private, loopback or link-local addresses; an admin can allow them for this server with "webhooks_allow_private_targets": true in config.json or IVARA_WEBHOOKS_ALLOW_PRIVATE=1`
}

// extraBlocked are non-public ranges net.IP's helpers don't cover.
var extraBlocked = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2001:db8::/32"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// blockedIP reports whether a webhook may not connect to ip (unless allowed).
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4 // also catches ::ffff:127.0.0.1
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.Equal(net.IPv4bcast) {
		return true
	}
	for _, n := range extraBlocked {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// checkHookURL validates a target URL when a hook is saved. The dialer checks
// again at delivery time, since DNS answers can change.
func checkHookURL(ctx context.Context, raw string, allowPrivate bool, lookup func(context.Context, string) ([]net.IPAddr, error)) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errBad("invalid_url", fmt.Sprintf("%q is not an http(s) URL", raw), "e.g. https://example.com/hooks/orders")
	}
	if allowPrivate {
		return nil
	}
	host := u.Hostname()
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, err := lookup(cctx, host)
		cancel()
		if err != nil || len(addrs) == 0 {
			return errBad("webhook_url_unresolvable", fmt.Sprintf("could not resolve %s", host), "check the host name")
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if blockedIP(ip) {
			return &APIError{Status: 400, Code: "webhook_url_private", Message: fmt.Sprintf("%s resolves to %s, a non-public address", host, ip), Hint: privateTargetHint()}
		}
	}
	return nil
}

// hookHTTPClient connects only to public addresses unless allowPrivate. The
// check is on the address actually dialed, after DNS, so rebinding can't
// sneak a private address in. Redirects are not followed and the environment
// proxy is ignored, for the same reason.
func hookHTTPClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if blockedIP(net.ParseIP(host)) {
				return fmt.Errorf("%w: %s", errHookPrivate, host)
			}
			return nil
		}
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 4,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 60 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// signHook is the X-Ivarabase-Signature value (see the comment at the top).
func signHook(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.", ts)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// ---------------------------------------------------------------- delivery

type hookResult struct {
	Status int
	MS     int64
	OK     bool
	Err    string
	Resp   string
}

func (a *App) deliverHook(ctx context.Context, slug string, h *hookRow, eventID int64, attempt int, payload []byte, test bool) hookResult {
	start := time.Now()
	timeout := time.Duration(clamp(h.TimeoutMS, 500, 60000)) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ts := time.Now().Unix()
	setHeaders := func(hd http.Header) {
		for k, v := range h.Headers {
			if headerNameRe.MatchString(k) && !strings.ContainsAny(v, "\r\n") && !strings.HasPrefix(strings.ToLower(k), "x-ivarabase-") {
				hd.Set(k, v)
			}
		}
		hd.Set("Content-Type", "application/json")
		hd.Set("User-Agent", "IvaraBase-Webhooks/"+Version)
		hd.Set("X-Ivarabase-Hook", h.Name)
		hd.Set("X-Ivarabase-Event-Id", strconv.FormatInt(eventID, 10))
		hd.Set("X-Ivarabase-Attempt", strconv.Itoa(attempt))
		hd.Set("X-Ivarabase-Timestamp", strconv.FormatInt(ts, 10))
		hd.Set("X-Ivarabase-Signature", signHook(h.Secret, ts, payload))
		if test {
			hd.Set("X-Ivarabase-Test", "1")
		}
	}
	res := hookResult{}
	finish := func() hookResult {
		res.MS = time.Since(start).Milliseconds()
		if res.Err == "" && ctx.Err() == context.DeadlineExceeded {
			res.Err = fmt.Sprintf("timed out after %s", timeout)
		}
		res.OK = res.Err == "" && res.Status >= 200 && res.Status < 300
		if !res.OK && res.Err == "" {
			res.Err = fmt.Sprintf("HTTP %d", res.Status)
		}
		return res
	}

	if h.Target == "function" {
		fn, err := a.function(slug, h.Function)
		if err != nil {
			res.Err = AsAPIError(err).Message
			return finish()
		}
		st, err := a.projectAPIState(slug)
		if err != nil {
			res.Err = err.Error()
			return finish()
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://function/", bytes.NewReader(payload))
		setHeaders(req.Header)
		req.Header.Set("Authorization", "Bearer "+apiKey(st.Secret, slug, roleService))
		req.RemoteAddr = "127.0.0.1:0"
		rec := newMemResponse()
		a.serveFunction(rec, req, fn, "/", true)
		res.Status = rec.status
		res.Resp = excerpt(rec.body.Bytes())
		return finish()
	}

	method := strings.ToUpper(h.Method)
	if method != "POST" && method != "PUT" && method != "PATCH" {
		method = "POST"
	}
	u, err := url.Parse(h.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		res.Err = "invalid URL"
		return finish()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
	if err != nil {
		res.Err = err.Error()
		return finish()
	}
	setHeaders(req.Header)
	resp, err := a.hookClient().Do(req)
	if err != nil {
		res.Err = err.Error()
		if errors.Is(err, errHookPrivate) {
			res.Err += " (" + privateTargetHint() + ")"
		}
		return finish()
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // let the connection be reused
	resp.Body.Close()
	res.Status = resp.StatusCode
	res.Resp = excerpt(b)
	return finish()
}

func excerpt(b []byte) string {
	s := strings.ToValidUTF8(string(b), "?")
	if len(s) > 1000 {
		s = s[:1000] + "…"
	}
	return s
}

// recordAttempt logs an attempt and moves the event on: delivered, retried
// later, or failed for good after the last attempt (or at once for tests).
func recordAttempt(ctx context.Context, db *sql.DB, hookID, eventID int64, attempt int, r hookResult, noRetry bool) error {
	status, next := "delivered", time.Now()
	if !r.OK {
		if noRetry || attempt >= hookMaxAttempts {
			status = "failed"
		} else {
			status, next = "pending", time.Now().Add(hookRetryDelay(attempt))
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO ivarabase_hooks.deliveries (event_id, hook_id, attempt, status_code, duration_ms, ok, error, response)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, eventID, hookID, attempt, r.Status, r.MS, r.OK, r.Err, r.Resp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ivarabase_hooks.events SET status=$2, next_attempt_at=$3, last_error=$4 WHERE id=$1`,
		eventID, status, next, r.Err); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- dispatcher

type hookManager struct {
	app    *App
	mu     sync.Mutex
	ctx    context.Context // set once the server's background loops run
	ds     map[string]*hookDispatcher
	client *http.Client
	once   sync.Once
}

type hookDispatcher struct {
	slug   string
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
}

func newHookManager(a *App) *hookManager {
	return &hookManager{app: a, ds: map[string]*hookDispatcher{}}
}

func (a *App) hookClient() *http.Client {
	m := a.Hooks
	if m == nil {
		return hookHTTPClient(a.Cfg.WebhooksAllowPrivate)
	}
	m.once.Do(func() {
		m.client = hookHTTPClient(a.Cfg.WebhooksAllowPrivate)
		if a.Cfg.WebhooksAllowPrivate {
			log.Printf("WARNING: webhooks may call private and loopback addresses (webhooks_allow_private_targets)")
		}
	})
	return m.client
}

// run starts a dispatcher for every project that has webhooks. Called from
// App.Background, so one-off commands (dr restore, token create) never deliver.
func (m *hookManager) run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	rows, err := m.app.Meta.Query(`SELECT slug FROM projects ORDER BY slug`)
	if err == nil {
		var slugs []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			slugs = append(slugs, s)
		}
		rows.Close()
		for _, s := range slugs {
			m.app.refreshHooks(ctx, s, false)
		}
	}
	<-ctx.Done()
	m.mu.Lock()
	ds := m.ds
	m.ds = map[string]*hookDispatcher{}
	m.mu.Unlock()
	for _, d := range ds {
		d.cancel()
	}
}

// refreshHooks brings a project's webhooks up to date (schema, triggers on
// re-created tables) and starts its dispatcher. After a restore, events that
// were still queued in the backup are cancelled: they describe the past and
// were most likely delivered by the original project already.
func (a *App) refreshHooks(ctx context.Context, slug string, restored bool) {
	db, err := a.projectDB(slug)
	if err != nil || !hooksInstalled(ctx, db) {
		return
	}
	if _, err := a.ensureHooksSchema(ctx, slug); err != nil {
		log.Printf("webhooks %s: %v", slug, err)
		return
	}
	if restored {
		db.ExecContext(ctx, `UPDATE ivarabase_hooks.events SET status='cancelled', last_error='restored from a backup' WHERE status='pending'`)
	}
	rows, err := db.QueryContext(ctx, `SELECT `+hookCols+` FROM ivarabase_hooks.hooks WHERE enabled`)
	if err != nil {
		return
	}
	var hooks []*hookRow
	for rows.Next() {
		if h, err := scanHook(rows); err == nil {
			hooks = append(hooks, h)
		}
	}
	rows.Close()
	for _, h := range hooks {
		var n int
		db.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname=$1 AND tgrelid = to_regclass($2)`, hookTrigger(h.ID), qualified(h.Schema, h.Table)).Scan(&n)
		if n == 0 || restored {
			if err := syncHookTrigger(ctx, db, h); err != nil && AsAPIError(err).Code != "table_not_found" {
				log.Printf("webhooks %s/%s: %v", slug, h.Name, err)
			}
		}
	}
	a.Hooks.start(slug)
}

func (m *hookManager) start(slug string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil || m.ctx.Err() != nil {
		return
	}
	if d, ok := m.ds[slug]; ok {
		select {
		case d.wake <- struct{}{}:
		default:
		}
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	d := &hookDispatcher{slug: slug, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
	m.ds[slug] = d
	go m.loop(ctx, d)
}

// stop ends a project's dispatcher and waits for it (before its database is
// dropped or swapped by a restore).
func (m *hookManager) stop(slug string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	d := m.ds[slug]
	delete(m.ds, slug)
	m.mu.Unlock()
	if d != nil {
		d.cancel()
		select {
		case <-d.done:
		case <-time.After(10 * time.Second):
		}
	}
}

func (m *hookManager) wake(slug string) { m.start(slug) }

type claimedEvent struct {
	id, hookID int64
	attempt    int
	payload    []byte
}

func (m *hookManager) loop(ctx context.Context, d *hookDispatcher) {
	defer close(d.done)
	a := m.app
	l := pq.NewListener(a.PG.dsn(a.PG.User, a.PG.Password, d.slug), time.Second, time.Minute, nil)
	defer l.Close()
	go l.Listen(hookChannel) // LISTEN executes no project code, so the superuser connection is fine
	inflight := map[int64]bool{}
	finished := make(chan int64, hookMaxInflight)
	var wg sync.WaitGroup
	defer wg.Wait()
	lastPrune := time.Time{}
	lastErr := time.Time{}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.Notify:
		case <-d.wake:
		case id := <-finished:
			delete(inflight, id)
		case <-timer.C:
		}
		if _, err := a.project(d.slug); err != nil {
			if AsAPIError(err).Code == "project_not_found" {
				m.mu.Lock()
				if m.ds[d.slug] == d {
					delete(m.ds, d.slug)
				}
				m.mu.Unlock()
				return
			}
		}
		db, err := a.projectDB(d.slug)
		if err == nil && time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			pruneHooks(ctx, db)
		}
		next := hookPollInterval
		if err == nil {
			var evs []claimedEvent
			evs, err = claimEvents(ctx, db, inflight)
			for _, ev := range evs {
				inflight[ev.hookID] = true
				wg.Add(1)
				go func(ev claimedEvent) {
					defer wg.Done()
					m.deliverEvent(ctx, d.slug, ev)
					select {
					case finished <- ev.hookID:
					case <-ctx.Done():
					}
				}(ev)
			}
			var due sql.NullTime
			if err == nil {
				db.QueryRowContext(ctx, `SELECT min(next_attempt_at) FROM ivarabase_hooks.events WHERE status='pending'`).Scan(&due)
				if due.Valid {
					next = min(next, max(time.Until(due.Time), 50*time.Millisecond))
				}
			}
		}
		if err != nil && ctx.Err() == nil {
			var pe *pq.Error
			if errors.As(err, &pe) && pe.Code == "42P01" { // the schema was dropped: nothing to deliver any more
				m.mu.Lock()
				if m.ds[d.slug] == d {
					delete(m.ds, d.slug)
				}
				m.mu.Unlock()
				return
			}
			if time.Since(lastErr) > 5*time.Minute {
				lastErr = time.Now()
				log.Printf("webhooks %s: %v", d.slug, err)
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

// claimEvents takes the oldest due event of every enabled hook that has
// nothing in flight, so each hook delivers in order while hooks run in parallel.
func claimEvents(ctx context.Context, db *sql.DB, inflight map[int64]bool) ([]claimedEvent, error) {
	free := hookMaxInflight - len(inflight)
	if free <= 0 {
		return nil, nil
	}
	busy := []int64{}
	for id := range inflight {
		busy = append(busy, id)
	}
	rows, err := db.QueryContext(ctx, `UPDATE ivarabase_hooks.events e SET attempts = e.attempts + 1
		FROM (SELECT id FROM (SELECT DISTINCT ON (ev.hook_id) ev.id FROM ivarabase_hooks.events ev
				JOIN ivarabase_hooks.hooks h ON h.id = ev.hook_id AND h.enabled
				WHERE ev.status = 'pending' AND ev.next_attempt_at <= now() AND NOT (ev.hook_id = ANY($1::bigint[]))
				ORDER BY ev.hook_id, ev.id) x ORDER BY id LIMIT $2) n
		WHERE e.id = n.id RETURNING e.id, e.hook_id, e.attempts, e.payload::text`, pq.Array(busy), free)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimedEvent
	for rows.Next() {
		var ev claimedEvent
		var p string
		if err := rows.Scan(&ev.id, &ev.hookID, &ev.attempt, &p); err != nil {
			return nil, err
		}
		ev.payload = []byte(p)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (m *hookManager) deliverEvent(ctx context.Context, slug string, ev claimedEvent) {
	a := m.app
	db, err := a.projectDB(slug)
	if err != nil {
		return
	}
	h, err := scanHook(db.QueryRowContext(ctx, `SELECT `+hookCols+` FROM ivarabase_hooks.hooks WHERE id=$1`, ev.hookID))
	if err != nil {
		return
	}
	r := a.deliverHook(ctx, slug, h, ev.id, ev.attempt, ev.payload, false)
	if ctx.Err() != nil {
		return // shutting down: the event stays pending and is retried after restart
	}
	if db, err = a.projectDB(slug); err == nil {
		if err := recordAttempt(context.Background(), db, h.ID, ev.id, ev.attempt, r, false); err != nil {
			log.Printf("webhooks %s/%s: record attempt: %v", slug, h.Name, err)
		}
	}
}

func pruneHooks(ctx context.Context, db *sql.DB) {
	db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM ivarabase_hooks.events WHERE status <> 'pending' AND created_at < now() - interval '%d days'`, hookKeepDays))
	db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM ivarabase_hooks.events WHERE id IN (SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY hook_id ORDER BY id DESC) AS rn
		FROM ivarabase_hooks.events WHERE status <> 'pending') x WHERE rn > %d)`, hookKeepPerHook))
	db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM ivarabase_hooks.deliveries WHERE at < now() - interval '%d days'`, hookKeepDays))
}

// ---------------------------------------------------------------- operations

func registerWebhookOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}
	nameParam := Param{Name: "name", Type: "string", Required: true, Desc: "webhook name"}
	targetParams := []Param{
		{Name: "table", Type: "string", Desc: "watched table: schema.table or table (public)"},
		{Name: "events", Type: "array", Items: "string", Desc: "any of INSERT, UPDATE, DELETE"},
		{Name: "url", Type: "string", Desc: "HTTP(S) endpoint to call (or use function)"},
		{Name: "method", Type: "string", Enum: []string{"POST", "PUT", "PATCH"}, Desc: "HTTP method for url targets"},
		{Name: "headers", Type: "object", Desc: "extra request headers, e.g. {\"Authorization\": \"Bearer ...\"}"},
		{Name: "function", Type: "string", Desc: "name of a function in this project to call instead of a URL (gets the service_role key)"},
		{Name: "timeout_ms", Type: "integer", Desc: "request timeout, 500-60000 ms"},
		{Name: "enabled", Type: "boolean"},
	}
	payloadDoc := "Payload (like Supabase): {\"type\":\"INSERT\",\"table\":\"orders\",\"schema\":\"public\",\"record\":{...},\"old_record\":null}. " +
		"Requests are signed: X-Ivarabase-Signature: sha256=HMAC-SHA256(secret, \"<X-Ivarabase-Timestamp>.<body>\"). Delivery is at least once (deduplicate on X-Ivarabase-Event-Id), retried 5 times over about an hour."

	r.add(&Op{Name: "webhooks.list", Tag: "webhooks", Method: "GET", Path: "/api/v1/projects/{project}/webhooks", Level: LevelRead,
		Summary: "List database webhooks with their delivery status",
		Params:  []Param{projectParam, {Name: "reveal_secrets", Type: "boolean", Default: false, Desc: "include signing secrets (needs write access)"}},
		Run: func(c *Ctx, a Args) (any, error) {
			reveal := a.Bool("reveal_secrets", false)
			if reveal {
				if err := c.require(LevelWrite); err != nil {
					return nil, err
				}
			}
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			if !hooksInstalled(c, db) {
				return out, nil
			}
			rows, err := db.QueryContext(c, `SELECT `+hookCols+`,
				to_regclass(quote_ident(schema_name)||'.'||quote_ident(table_name)) IS NOT NULL,
				EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgname = 'ivarabase_hook_'||h.id AND t.tgrelid = to_regclass(quote_ident(schema_name)||'.'||quote_ident(table_name))),
				(SELECT count(*) FROM ivarabase_hooks.events e WHERE e.hook_id=h.id AND e.status='pending'),
				(SELECT count(*) FROM ivarabase_hooks.events e WHERE e.hook_id=h.id AND e.status='delivered' AND e.created_at > now()-interval '24 hours'),
				(SELECT count(*) FROM ivarabase_hooks.events e WHERE e.hook_id=h.id AND e.status='failed' AND e.created_at > now()-interval '24 hours'),
				d.at, d.status_code, d.ok, d.error
				FROM ivarabase_hooks.hooks h LEFT JOIN LATERAL (SELECT at, status_code, ok, error FROM ivarabase_hooks.deliveries WHERE hook_id=h.id ORDER BY id DESC LIMIT 1) d ON true
				ORDER BY name`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			for rows.Next() {
				var tableOK, trigOK bool
				var pending, delivered, failed int
				var lastAt sql.NullTime
				var lastStatus sql.NullInt64
				var lastOK sql.NullBool
				var lastErr sql.NullString
				h, err := scanHook(rows, &tableOK, &trigOK, &pending, &delivered, &failed, &lastAt, &lastStatus, &lastOK, &lastErr)
				if err != nil {
					return nil, err
				}
				m := h.public(reveal)
				m["table_exists"], m["trigger_installed"] = tableOK, trigOK
				m["pending"], m["delivered_24h"], m["failed_24h"] = pending, delivered, failed
				if lastAt.Valid {
					m["last_delivery"] = map[string]any{"at": lastAt.Time, "status": lastStatus.Int64, "ok": lastOK.Bool, "error": lastErr.String}
				}
				switch {
				case !h.Enabled:
					m["state"] = "disabled"
				case !tableOK:
					m["state"] = "table_missing"
				case !trigOK:
					m["state"] = "trigger_missing"
				default:
					m["state"] = "active"
				}
				out = append(out, m)
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "webhooks.create", Tag: "webhooks", Method: "POST", Path: "/api/v1/projects/{project}/webhooks", Level: LevelWrite,
		Summary:     "Call a URL or a function when rows of a table are inserted, updated or deleted",
		Description: payloadDoc + " The response includes the signing secret.",
		Params: append([]Param{projectParam, nameParam,
			{Name: "table", Type: "string", Required: true, Desc: "watched table: schema.table or table (public)"},
			{Name: "events", Type: "array", Items: "string", Default: []any{"INSERT", "UPDATE", "DELETE"}, Desc: "any of INSERT, UPDATE, DELETE"},
		}, targetParams[2:]...),
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			name := strings.TrimSpace(a.Str("name"))
			if !hookNameRe.MatchString(name) {
				return nil, errBad("invalid_name", "webhook names are 1-63 letters, digits, - and _", "")
			}
			h := &hookRow{Name: name, Method: "POST", TimeoutMS: 10000, Enabled: a.Bool("enabled", true), Secret: "whsec_" + randHex(24)}
			if err := applyHookArgs(c, h, a, true); err != nil {
				return nil, err
			}
			db, err := c.App.ensureHooksSchema(c, slug)
			if err != nil {
				return nil, err
			}
			if err := checkHookTable(c, db, h.Schema, h.Table); err != nil {
				return nil, err
			}
			hdr, _ := json.Marshal(h.Headers)
			err = db.QueryRowContext(c, `INSERT INTO ivarabase_hooks.hooks (name, schema_name, table_name, events, target_type, url, method, headers, function_name, timeout_ms, secret, enabled)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id, created_at, updated_at`,
				h.Name, h.Schema, h.Table, pq.Array(h.Events), h.Target, h.URL, h.Method, hdr, h.Function, h.TimeoutMS, h.Secret, h.Enabled).
				Scan(&h.ID, &h.CreatedAt, &h.UpdatedAt)
			var pe *pq.Error
			if errors.As(err, &pe) && pe.Code == "23505" {
				return nil, &APIError{Status: 409, Code: "webhook_exists", Message: fmt.Sprintf("a webhook named %q already exists", name), Hint: "use webhooks.update"}
			}
			if err != nil {
				return nil, err
			}
			if err := syncHookTrigger(c, db, h); err != nil {
				db.ExecContext(c, `DELETE FROM ivarabase_hooks.hooks WHERE id=$1`, h.ID)
				return nil, err
			}
			c.App.Hooks.start(slug)
			return h.public(true), nil
		}})

	r.add(&Op{Name: "webhooks.update", Tag: "webhooks", Method: "PATCH", Path: "/api/v1/projects/{project}/webhooks/{name}", Level: LevelWrite,
		Summary:     "Change a webhook (only the given fields), enable/disable it, or rotate its secret",
		Description: "Disabling cancels events that are still queued. Setting url clears function and vice versa.",
		Params: append([]Param{projectParam, nameParam, {Name: "new_name", Type: "string"},
			{Name: "rotate_secret", Type: "boolean", Desc: "generate a new signing secret (returned once)"}}, targetParams...),
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			db, err := c.App.projectDB(slug)
			if err != nil {
				return nil, err
			}
			h, err := c.App.hook(c, db, a.Str("name"))
			if err != nil {
				return nil, err
			}
			if _, ok := a["new_name"]; ok {
				if n := strings.TrimSpace(a.Str("new_name")); n != h.Name {
					if !hookNameRe.MatchString(n) {
						return nil, errBad("invalid_name", "webhook names are 1-63 letters, digits, - and _", "")
					}
					h.Name = n
				}
			}
			if _, ok := a["enabled"]; ok {
				h.Enabled = a.Bool("enabled", h.Enabled)
			}
			if err := applyHookArgs(c, h, a, false); err != nil {
				return nil, err
			}
			rotated := a.Bool("rotate_secret", false)
			if rotated {
				h.Secret = "whsec_" + randHex(24)
			}
			if h.Enabled {
				if err := checkHookTable(c, db, h.Schema, h.Table); err != nil {
					return nil, err
				}
			}
			hdr, _ := json.Marshal(h.Headers)
			_, err = db.ExecContext(c, `UPDATE ivarabase_hooks.hooks SET name=$2, schema_name=$3, table_name=$4, events=$5, target_type=$6, url=$7, method=$8, headers=$9,
				function_name=$10, timeout_ms=$11, secret=$12, enabled=$13, updated_at=now() WHERE id=$1`,
				h.ID, h.Name, h.Schema, h.Table, pq.Array(h.Events), h.Target, h.URL, h.Method, hdr, h.Function, h.TimeoutMS, h.Secret, h.Enabled)
			var pe *pq.Error
			if errors.As(err, &pe) && pe.Code == "23505" {
				return nil, &APIError{Status: 409, Code: "webhook_exists", Message: fmt.Sprintf("a webhook named %q already exists", h.Name)}
			}
			if err != nil {
				return nil, err
			}
			if !h.Enabled {
				db.ExecContext(c, `UPDATE ivarabase_hooks.events SET status='cancelled', last_error='webhook disabled' WHERE hook_id=$1 AND status='pending'`, h.ID)
			}
			if err := syncHookTrigger(c, db, h); err != nil {
				return nil, err
			}
			c.App.Hooks.start(slug)
			return h.public(rotated), nil
		}})

	r.add(&Op{Name: "webhooks.delete", Tag: "webhooks", Method: "DELETE", Path: "/api/v1/projects/{project}/webhooks/{name}", Level: LevelWrite,
		Destructive: true,
		Summary:     "Delete a webhook, its trigger, queued events and delivery log",
		Params:      []Param{projectParam, nameParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			h, err := c.App.hook(c, db, a.Str("name"))
			if err != nil {
				return nil, err
			}
			var pending, events int
			db.QueryRowContext(c, `SELECT count(*) FILTER (WHERE status='pending'), count(*) FROM ivarabase_hooks.events WHERE hook_id=$1`, h.ID).Scan(&pending, &events)
			return map[string]any{"webhook": h.Name, "table": h.Schema + "." + h.Table, "target": firstNonEmpty(h.URL, "function "+h.Function),
				"pending_events_dropped": pending, "logged_events_deleted": events}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			h, err := c.App.hook(c, db, a.Str("name"))
			if err != nil {
				return nil, err
			}
			h.Enabled = false
			if err := syncHookTrigger(c, db, h); err != nil {
				return nil, err
			}
			if _, err := db.ExecContext(c, `DELETE FROM ivarabase_hooks.hooks WHERE id=$1`, h.ID); err != nil {
				return nil, err
			}
			return map[string]any{"deleted": h.Name}, nil
		}})

	r.add(&Op{Name: "webhooks.deliveries", Tag: "webhooks", Method: "GET", Path: "/api/v1/projects/{project}/webhook-deliveries", Level: LevelRead,
		Summary: "Delivery log: every attempt with status code, duration, response excerpt and error",
		Params: []Param{projectParam, {Name: "name", Type: "string", Desc: "only this webhook"},
			{Name: "failed_only", Type: "boolean", Default: false}, {Name: "include_payload", Type: "boolean", Default: false},
			{Name: "limit", Type: "integer", Default: 50}},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			if !hooksInstalled(c, db) {
				return out, nil
			}
			q := `SELECT d.id, d.event_id, h.name, d.attempt, d.at, d.status_code, d.duration_ms, d.ok, d.error, d.response,
				e.payload->>'type', e.status, e.test, e.attempts, e.next_attempt_at, CASE WHEN $2 THEN e.payload::text ELSE '' END
				FROM ivarabase_hooks.deliveries d JOIN ivarabase_hooks.events e ON e.id = d.event_id JOIN ivarabase_hooks.hooks h ON h.id = d.hook_id
				WHERE ($1 = '' OR h.name = $1)`
			if a.Bool("failed_only", false) {
				q += ` AND NOT d.ok`
			}
			q += fmt.Sprintf(` ORDER BY d.id DESC LIMIT %d`, clamp(a.Int("limit", 50), 1, 1000))
			rows, err := db.QueryContext(c, q, a.Str("name"), a.Bool("include_payload", false))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			for rows.Next() {
				var id, ev int64
				var name, errText, resp, typ, status, payload string
				var attempt, code, ms, attempts int
				var at, next time.Time
				var ok, test bool
				if err := rows.Scan(&id, &ev, &name, &attempt, &at, &code, &ms, &ok, &errText, &resp, &typ, &status, &test, &attempts, &next, &payload); err != nil {
					return nil, err
				}
				m := map[string]any{"id": id, "event_id": ev, "webhook": name, "attempt": attempt, "at": at, "status_code": code, "duration_ms": ms,
					"ok": ok, "error": errText, "response": resp, "type": typ, "event_status": status, "test": test}
				if status == "pending" {
					m["next_attempt_at"] = next
				}
				if payload != "" {
					m["payload"] = json.RawMessage(payload)
				}
				out = append(out, m)
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "webhooks.test", Tag: "webhooks", Method: "POST", Path: "/api/v1/projects/{project}/webhooks/{name}/test", Level: LevelWrite,
		Summary:     "Send a sample payload now and return the result (logged, not retried)",
		Description: "The sample uses a real row of the table when there is one. The request carries X-Ivarabase-Test: 1.",
		Params:      []Param{projectParam, nameParam, {Name: "type", Type: "string", Default: "INSERT", Enum: hookEventNames}},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			db, err := c.App.projectDB(slug)
			if err != nil {
				return nil, err
			}
			h, err := c.App.hook(c, db, a.Str("name"))
			if err != nil {
				return nil, err
			}
			payload := sampleHookPayload(c, db, h, a.Str("type"))
			var eventID int64
			if err := db.QueryRowContext(c, `INSERT INTO ivarabase_hooks.events (hook_id, payload, attempts, test, next_attempt_at) VALUES ($1,$2,1,true,'infinity') RETURNING id`,
				h.ID, payload).Scan(&eventID); err != nil {
				return nil, err
			}
			res := c.App.deliverHook(c, slug, h, eventID, 1, payload, true)
			if err := recordAttempt(context.Background(), db, h.ID, eventID, 1, res, true); err != nil {
				return nil, err
			}
			var sent any
			json.Unmarshal(payload, &sent)
			return map[string]any{"ok": res.OK, "status_code": res.Status, "duration_ms": res.MS, "error": res.Err, "response": res.Resp,
				"event_id": eventID, "payload": sent}, nil
		}})

	r.add(&Op{Name: "webhooks.redeliver", Tag: "webhooks", Method: "POST", Path: "/api/v1/projects/{project}/webhook-events/{event_id}/redeliver", Level: LevelWrite,
		Summary: "Queue an event again (with a fresh set of retries), e.g. after fixing the receiver",
		Params:  []Param{projectParam, {Name: "event_id", Type: "integer", Required: true, Desc: "event_id from webhooks.deliveries"}},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			db, err := c.App.projectDB(slug)
			if err != nil {
				return nil, err
			}
			if !hooksInstalled(c, db) {
				return nil, errNotFound("event_not_found", "no such event", "")
			}
			id := int64(a.Int("event_id", 0))
			var status, name string
			var enabled bool
			err = db.QueryRowContext(c, `SELECT e.status, h.name, h.enabled FROM ivarabase_hooks.events e JOIN ivarabase_hooks.hooks h ON h.id=e.hook_id WHERE e.id=$1`, id).
				Scan(&status, &name, &enabled)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errNotFound("event_not_found", fmt.Sprintf("no event %d", id), "event ids are in webhooks.deliveries (old events are pruned after 7 days)")
			}
			if err != nil {
				return nil, err
			}
			if !enabled {
				return nil, errBad("webhook_disabled", fmt.Sprintf("webhook %q is disabled", name), "enable it with webhooks.update first")
			}
			if status == "pending" {
				return nil, &APIError{Status: 409, Code: "event_pending", Message: "this event is still queued for delivery", Hint: "wait for it, or check webhooks.deliveries"}
			}
			if _, err := db.ExecContext(c, `UPDATE ivarabase_hooks.events SET status='pending', attempts=0, next_attempt_at=now(), last_error='' WHERE id=$1`, id); err != nil {
				return nil, err
			}
			c.App.Hooks.wake(slug)
			return map[string]any{"queued": true, "event_id": id, "webhook": name}, nil
		}})
}

// applyHookArgs copies table/events/target fields from the call onto h.
func applyHookArgs(c *Ctx, h *hookRow, a Args, create bool) error {
	if _, ok := a["table"]; ok || create {
		s, t, err := parseHookTable(a.Str("table"))
		if err != nil {
			return err
		}
		h.Schema, h.Table = s, t
	}
	if _, ok := a["events"]; ok || create {
		ev, err := parseHookEvents(a.StrList("events"))
		if err != nil {
			return err
		}
		h.Events = ev
	}
	_, hasURL := a["url"]
	_, hasFn := a["function"]
	u, fn := strings.TrimSpace(a.Str("url")), strings.TrimSpace(a.Str("function"))
	switch {
	case u != "" && fn != "":
		return errBad("invalid_parameter", "give either url or function, not both", "")
	case u != "":
		if err := checkHookURL(c, u, c.App.Cfg.WebhooksAllowPrivate, net.DefaultResolver.LookupIPAddr); err != nil {
			return err
		}
		h.Target, h.URL, h.Function = "url", u, ""
	case fn != "":
		if _, err := c.App.function(a.Str("project"), fn); err != nil {
			return err
		}
		h.Target, h.Function, h.URL = "function", fn, ""
	case create || (hasURL && h.Target == "url") || (hasFn && h.Target == "function"):
		return errBad("missing_parameter", "a webhook needs a url or a function to call", "")
	}
	if m := a.Str("method"); m != "" {
		h.Method = strings.ToUpper(m)
	}
	if _, ok := a["headers"]; ok {
		hd, err := parseHookHeaders(a.Obj("headers"))
		if err != nil {
			return err
		}
		h.Headers = hd
	}
	if h.Headers == nil {
		h.Headers = map[string]string{}
	}
	if _, ok := a["timeout_ms"]; ok {
		h.TimeoutMS = clamp(a.Int("timeout_ms", 10000), 500, 60000)
	}
	return nil
}

func sampleHookPayload(ctx context.Context, db *sql.DB, h *hookRow, typ string) []byte {
	var rec []byte
	if checkHookTable(ctx, db, h.Schema, h.Table) == nil {
		db.QueryRowContext(ctx, `SELECT to_jsonb(t) FROM `+qualified(h.Schema, h.Table)+` t LIMIT 1`).Scan(&rec)
		if rec == nil {
			db.QueryRowContext(ctx, `SELECT jsonb_object_agg(attname, NULL) FROM pg_attribute WHERE attrelid = to_regclass($1) AND attnum > 0 AND NOT attisdropped`,
				qualified(h.Schema, h.Table)).Scan(&rec)
		}
	}
	if rec == nil {
		rec = []byte("{}")
	}
	p := map[string]any{"type": typ, "table": h.Table, "schema": h.Schema, "record": json.RawMessage(rec), "old_record": nil}
	switch typ {
	case "UPDATE":
		p["old_record"] = json.RawMessage(rec)
	case "DELETE":
		p["record"], p["old_record"] = nil, json.RawMessage(rec)
	}
	b, _ := json.Marshal(p)
	return b
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
