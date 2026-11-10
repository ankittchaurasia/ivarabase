package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// postgres_changes.
//
// Design: a trigger-based change log instead of logical decoding. The managed
// cluster runs with wal_level = replica (raising it needs a restart and costs
// WAL on every project), external clusters may not allow replication slots at
// all, and a forgotten slot retains WAL forever. Triggers need none of that:
//
//   - Each project has a real `supabase_realtime` publication, so Supabase's
//     `alter publication supabase_realtime add table x;` works unchanged and
//     stays the switch. An event trigger on CREATE/ALTER/DROP PUBLICATION adds
//     (or removes) an AFTER INSERT/UPDATE/DELETE row trigger on every table in it.
//   - The row trigger (realtime.ivb_capture, SECURITY DEFINER as the project
//     owner) writes the change - to_jsonb(NEW)/to_jsonb(OLD) - into the
//     unlogged table realtime.ivb_changes and sends an empty pg_notify. Rows
//     never travel through NOTIFY, so its 8000-byte limit doesn't matter, and
//     identical empty notifications are folded into one per transaction. Only
//     committed changes are seen. While nobody listens (no heartbeat in
//     realtime.ivb_listener for 90s) the trigger does nothing.
//   - The server LISTENs per project while clients subscribe, drains the log,
//     and checks row level security per subscriber: in a read-only transaction
//     as the subscriber's role with request.jwt.claims set (as PostgREST does),
//     it needs SELECT on the table and, when RLS is on, evaluates the table's
//     SELECT policies (permissive OR'ed, restrictive AND'ed, as Postgres
//     combines them) against the logged row via jsonb_populate_record. Checking
//     the logged values rather than re-reading the table by primary key means a
//     row that was updated or deleted again before we got to it is still judged
//     as it was, and DELETEs get the same check against the old row.
//   - Payloads follow Supabase: on tables with RLS enabled `old` carries only
//     the primary key; otherwise every column with REPLICA IDENTITY FULL, the
//     primary key without.

const (
	rtNotifyChannel = "ivarabase_realtime"
	rtDrainBatch    = 200
	rtBigRecord     = 1 << 20 // larger rows are sent with only their short fields, like Supabase
)

// ---------------------------------------------------------------- schema

var realtimeReady sync.Map // slug -> true once the schema is in place this process

// rtSyncBody (re)attaches the capture trigger to exactly the tables in the
// supabase_realtime publication. It runs inside the event trigger as whoever
// ran the DDL (the project role owns its tables and publication); failures on a
// single table are warnings so they never block the user's statement.
const rtSyncBody = `
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass AS rel FROM pg_catalog.pg_publication_tables pt
      JOIN pg_catalog.pg_namespace n ON n.nspname = pt.schemaname
      JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid AND c.relname = pt.tablename
      WHERE pt.pubname = 'supabase_realtime' AND pt.schemaname NOT IN ('realtime', 'ivarabase_internal', 'pg_catalog', 'information_schema')
        AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid = c.oid AND t.tgname = 'ivarabase_realtime')
  LOOP
    BEGIN
      EXECUTE format('CREATE TRIGGER ivarabase_realtime AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW EXECUTE FUNCTION realtime.ivb_capture()', r.rel);
    EXCEPTION WHEN OTHERS THEN RAISE WARNING 'realtime: cannot watch %: %', r.rel, SQLERRM;
    END;
  END LOOP;
  FOR r IN SELECT t.tgrelid::regclass AS rel FROM pg_catalog.pg_trigger t
      WHERE t.tgname = 'ivarabase_realtime' AND NOT t.tgisinternal
        AND t.tgrelid NOT IN (SELECT c.oid FROM pg_catalog.pg_publication_tables pt
          JOIN pg_catalog.pg_namespace n ON n.nspname = pt.schemaname
          JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid AND c.relname = pt.tablename
          WHERE pt.pubname = 'supabase_realtime')
  LOOP
    BEGIN
      EXECUTE format('DROP TRIGGER ivarabase_realtime ON %s', r.rel);
    EXCEPTION WHEN OTHERS THEN RAISE WARNING 'realtime: cannot unwatch %: %', r.rel, SQLERRM;
    END;
  END LOOP;
END`

// ensureRealtimeSchema is idempotent. Objects the project can modify are
// created as the project role (never as the superuser, which would then run
// project-controlled code); only the event trigger needs the superuser, and
// its function lives in the superuser-owned internal schema.
func (a *App) ensureRealtimeSchema(slug string) error {
	owner, err := a.projectDB(slug)
	if err != nil {
		return err
	}
	ownerStmts := []string{
		`CREATE SCHEMA IF NOT EXISTS realtime`,
		`CREATE UNLOGGED TABLE IF NOT EXISTS realtime.ivb_changes (
			id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now(),
			schema_name text NOT NULL, table_name text NOT NULL, op text NOT NULL, new_rec jsonb, old_rec jsonb)`,
		`CREATE TABLE IF NOT EXISTS realtime.ivb_listener (id int PRIMARY KEY DEFAULT 1 CHECK (id = 1), seen timestamptz)`,
		`INSERT INTO realtime.ivb_listener (id) VALUES (1) ON CONFLICT DO NOTHING`,
		`CREATE OR REPLACE FUNCTION realtime.ivb_capture() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM realtime.ivb_listener WHERE seen > now() - interval '90 seconds') THEN
				RETURN NULL;
			END IF;
			INSERT INTO realtime.ivb_changes (schema_name, table_name, op, new_rec, old_rec) VALUES (TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP,
				CASE WHEN TG_OP <> 'DELETE' THEN to_jsonb(NEW) END, CASE WHEN TG_OP <> 'INSERT' THEN to_jsonb(OLD) END);
			PERFORM pg_notify('` + rtNotifyChannel + `', '');
			RETURN NULL;
		END $$`,
		`REVOKE ALL ON FUNCTION realtime.ivb_capture() FROM PUBLIC`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = 'supabase_realtime') THEN
				CREATE PUBLICATION supabase_realtime;
			END IF;
		END $$`,
	}
	for _, q := range ownerStmts {
		if _, err := owner.Exec(q); err != nil {
			return fmt.Errorf("realtime setup (%s): %w", strings.SplitN(strings.TrimSpace(q), "\n", 2)[0], err)
		}
	}
	super, err := a.PG.Super(slug)
	if err != nil {
		return err
	}
	defer a.PG.ClosePool(slug)
	for _, q := range []string{
		`CREATE SCHEMA IF NOT EXISTS ` + internalNS,
		`REVOKE ALL ON SCHEMA ` + internalNS + ` FROM PUBLIC`,
		`CREATE OR REPLACE FUNCTION ` + internalNS + `.realtime_watch() RETURNS event_trigger LANGUAGE plpgsql SET search_path = pg_catalog AS $$` + rtSyncBody + ` $$`,
		`DROP EVENT TRIGGER IF EXISTS ivarabase_realtime_watch`,
		`CREATE EVENT TRIGGER ivarabase_realtime_watch ON ddl_command_end
			WHEN TAG IN ('CREATE PUBLICATION', 'ALTER PUBLICATION', 'DROP PUBLICATION') EXECUTE FUNCTION ` + internalNS + `.realtime_watch()`,
	} {
		if _, err := super.Exec(q); err != nil {
			return fmt.Errorf("realtime setup (%s): %w", strings.SplitN(strings.TrimSpace(q), "\n", 2)[0], err)
		}
	}
	// a no-op ALTER PUBLICATION, run as the owner, fires the event trigger and
	// attaches triggers for tables published before this existed (or restored)
	if _, err := owner.Exec(`ALTER PUBLICATION supabase_realtime SET (publish = 'insert, update, delete, truncate')`); err != nil {
		return fmt.Errorf("realtime setup (sync publication): %w", err)
	}
	realtimeReady.Store(slug, true)
	return nil
}

// ---------------------------------------------------------------- subscriptions

type rtPGSub struct {
	id                         int64
	event, schema, table, rawF string
	filter                     *rtFilter
	ch                         *rtChan
}

type rtFilter struct {
	col, op string
	vals    []string
}

var rtSubSeq struct {
	sync.Mutex
	n int64
}

func nextSubID() int64 {
	rtSubSeq.Lock()
	defer rtSubSeq.Unlock()
	rtSubSeq.n++
	return rtSubSeq.n
}

// parsePGSubs validates the join's postgres_changes config and returns the
// subscriptions plus the reply entries supabase-js matches them against
// (same event/schema/table/filter, in order, with our id).
func parsePGSubs(cfg []map[string]any) ([]*rtPGSub, []map[string]any, error) {
	if len(cfg) > rtMaxPGSubsPerChan {
		return nil, nil, fmt.Errorf("too many postgres_changes bindings on one channel (max %d)", rtMaxPGSubsPerChan)
	}
	subs := []*rtPGSub{}
	reply := []map[string]any{}
	for _, c := range cfg {
		ev, _ := c["event"].(string)
		schema, _ := c["schema"].(string)
		table, hasTable := c["table"].(string)
		filter, hasFilter := c["filter"].(string)
		switch ev {
		case "*", "INSERT", "UPDATE", "DELETE":
		default:
			return nil, nil, fmt.Errorf("postgres_changes event must be INSERT, UPDATE, DELETE or *, got %q", ev)
		}
		if schema == "" {
			return nil, nil, fmt.Errorf("postgres_changes needs a schema")
		}
		s := &rtPGSub{id: nextSubID(), event: ev, schema: schema, table: table, rawF: filter}
		if hasFilter && filter != "" {
			f, err := parseRTFilter(filter)
			if err != nil {
				return nil, nil, err
			}
			s.filter = f
		}
		subs = append(subs, s)
		r := map[string]any{"id": s.id, "event": ev, "schema": schema}
		if hasTable {
			r["table"] = table
		}
		if hasFilter {
			r["filter"] = filter
		}
		reply = append(reply, r)
	}
	return subs, reply, nil
}

// parseRTFilter parses Supabase's "column=op.value" filter syntax.
func parseRTFilter(s string) (*rtFilter, error) {
	col, rest, ok := strings.Cut(s, "=")
	op, val, ok2 := strings.Cut(rest, ".")
	col = strings.TrimSpace(col)
	if !ok || !ok2 || col == "" {
		return nil, fmt.Errorf("invalid filter %q: use column=op.value, e.g. id=eq.5", s)
	}
	f := &rtFilter{col: col, op: op}
	switch op {
	case "eq", "neq", "lt", "lte", "gt", "gte":
		f.vals = []string{val}
	case "in":
		if !strings.HasPrefix(val, "(") || !strings.HasSuffix(val, ")") {
			return nil, fmt.Errorf("invalid filter %q: in takes a list, e.g. id=in.(1,2,3)", s)
		}
		for _, v := range strings.Split(val[1:len(val)-1], ",") {
			f.vals = append(f.vals, strings.Trim(strings.TrimSpace(v), `"`))
		}
		if len(f.vals) > 100 {
			return nil, fmt.Errorf("invalid filter %q: at most 100 values in a list", s)
		}
	default:
		return nil, fmt.Errorf("invalid filter %q: operator must be eq, neq, lt, lte, gt, gte or in", s)
	}
	return f, nil
}

// rtText renders a JSON value the way Postgres prints it, for comparisons.
func rtText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return x.String(), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	default:
		b, _ := json.Marshal(x)
		return string(b), true
	}
}

func rtCompare(a, b string) int {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea == nil && eb == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

// match: SQL semantics, so a NULL (or missing) column matches nothing.
func (f *rtFilter) match(rec map[string]any) bool {
	if f == nil {
		return true
	}
	v, ok := rtText(rec[f.col])
	if !ok {
		return false
	}
	switch f.op {
	case "eq":
		return rtCompare(v, f.vals[0]) == 0
	case "neq":
		return rtCompare(v, f.vals[0]) != 0
	case "lt":
		return rtCompare(v, f.vals[0]) < 0
	case "lte":
		return rtCompare(v, f.vals[0]) <= 0
	case "gt":
		return rtCompare(v, f.vals[0]) > 0
	case "gte":
		return rtCompare(v, f.vals[0]) >= 0
	case "in":
		for _, x := range f.vals {
			if rtCompare(v, x) == 0 {
				return true
			}
		}
	}
	return false
}

func (s *rtPGSub) wants(schema, table, op string) bool {
	if s.schema != "*" && s.schema != schema {
		return false
	}
	if s.table != "" && s.table != "*" && s.table != table {
		return false
	}
	return s.event == "*" || s.event == op
}

// ---------------------------------------------------------------- pipeline

type rtPipeline struct {
	p        *rtProject
	listener *pq.Listener
	stop     chan struct{}
	done     chan struct{}
	started  time.Time

	mu        sync.Mutex
	lastErr   string
	delivered int64
	changes   int64
	idleSince time.Time
}

func (p *rtProject) hasPGSubsLocked() bool {
	for _, set := range p.topics {
		for ch := range set {
			if len(ch.pg) > 0 {
				return true
			}
		}
	}
	return false
}

// ensurePipeline starts the project's change feed (LISTEN + drain loop) and
// returns once it is listening.
func (p *rtProject) ensurePipeline() error {
	p.pipeMu.Lock()
	defer p.pipeMu.Unlock()
	if p.pipe != nil {
		select {
		case <-p.pipe.done:
			p.pipe = nil
		default:
			p.pipe.mu.Lock()
			p.pipe.idleSince = time.Time{}
			p.pipe.mu.Unlock()
			return nil
		}
	}
	a := p.app
	if _, ok := realtimeReady.Load(p.slug); !ok {
		if err := a.ensureRealtimeSchema(p.slug); err != nil {
			logRealtime("project %s: %v", p.slug, err)
			return fmt.Errorf("realtime is not set up in this database")
		}
	}
	db, err := a.projectDB(p.slug)
	if err != nil {
		return err
	}
	prow, err := a.project(p.slug)
	if err != nil {
		return err
	}
	pw, err := a.projectPassword(prow)
	if err != nil {
		return err
	}
	l := pq.NewListener(a.PG.dsn(prow.Role, pw, p.slug)+" application_name=ivarabase-realtime", time.Second, 30*time.Second, nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = l.Listen(rtNotifyChannel)
		if err == nil || err == pq.ErrChannelAlreadyOpen {
			if perr := l.Ping(); perr == nil {
				break
			} else {
				err = perr
			}
		}
		if time.Now().After(deadline) {
			l.Close()
			return fmt.Errorf("cannot LISTEN for changes: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// start clean: anything logged before is from before these subscribers
	if _, err := db.Exec(`UPDATE realtime.ivb_listener SET seen = now()`); err != nil {
		l.Close()
		return err
	}
	db.Exec(`DELETE FROM realtime.ivb_changes`)
	pl := &rtPipeline{p: p, listener: l, stop: make(chan struct{}), done: make(chan struct{}), started: time.Now()}
	p.pipe = pl
	go pl.run()
	return nil
}

func (p *rtProject) stopPipeline() {
	p.pipeMu.Lock()
	defer p.pipeMu.Unlock()
	if pl := p.pipe; pl != nil {
		p.pipe = nil
		close(pl.stop)
		<-pl.done
	}
}

func (p *rtProject) pipelineStatus() map[string]any {
	p.pipeMu.Lock()
	pl := p.pipe
	p.pipeMu.Unlock()
	if pl == nil {
		return map[string]any{"listening": false}
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return map[string]any{"listening": true, "since": pl.started, "changes": pl.changes, "delivered": pl.delivered, "last_error": pl.lastErr}
}

func (pl *rtPipeline) run() {
	defer close(pl.done)
	defer pl.listener.Close()
	p := pl.p
	poll := time.NewTicker(3 * time.Second)
	defer poll.Stop()
	lastBeat := time.Now()
	for {
		select {
		case <-pl.stop:
			// project deleted or its secret rotated: don't touch the database
			// (that would reopen a pool for a dropped project); the listener
			// heartbeat simply lapses and the trigger stops logging
			return
		case <-pl.listener.Notify:
		case <-poll.C:
		}
		if time.Since(lastBeat) > 30*time.Second {
			if db, err := p.app.projectDB(p.slug); err == nil {
				db.Exec(`UPDATE realtime.ivb_listener SET seen = now()`)
			}
			lastBeat = time.Now()
		}
		pl.drain()
		// stop a minute after the last postgres_changes subscriber left
		p.mu.Lock()
		has := p.hasPGSubsLocked()
		p.mu.Unlock()
		pl.mu.Lock()
		if has {
			pl.idleSince = time.Time{}
		} else if pl.idleSince.IsZero() {
			pl.idleSince = time.Now()
		}
		idle := !pl.idleSince.IsZero() && time.Since(pl.idleSince) > time.Minute
		pl.mu.Unlock()
		// release under pipeMu, so a concurrent ensurePipeline can't mark
		// the feed live and then have it switched off here; TryLock because
		// stopPipeline holds pipeMu while it waits for this loop to end
		if idle && p.pipeMu.TryLock() {
			if p.pipe == pl {
				p.pipe = nil
			}
			pl.release()
			p.pipeMu.Unlock()
			return
		}
	}
}

// release tells the capture trigger nobody listens any more.
func (pl *rtPipeline) release() {
	if db, err := pl.p.app.projectDB(pl.p.slug); err == nil {
		db.Exec(`UPDATE realtime.ivb_listener SET seen = NULL`)
		db.Exec(`DELETE FROM realtime.ivb_changes`)
	}
}

func (pl *rtPipeline) setErr(err error) {
	pl.mu.Lock()
	pl.lastErr = err.Error()
	pl.mu.Unlock()
	logRealtime("project %s: %v", pl.p.slug, err)
}

type rtChange struct {
	id            int64
	at            time.Time
	schema, table string
	op            string
	newRec        map[string]any
	oldRec        map[string]any
	big           bool
}

type rtTableMeta struct {
	rls      bool
	full     bool // REPLICA IDENTITY FULL
	columns  []map[string]any
	pk       []string
	qualName string
	alias    string
	policies []rtPolicy
}

// rtPolicy is a SELECT (or ALL) policy; roles holds role names, "public" for PUBLIC.
type rtPolicy struct {
	permissive bool
	qual       sql.NullString
	roles      []string
}

// visibilityQuery returns SQL that, run as role, tells whether the row in $1
// (a JSON object) is visible to it, and whether it uses $1.
func (m *rtTableMeta) visibilityQuery(role string, bypassRLS bool) (string, bool) {
	priv := `has_table_privilege(` + pq.QuoteLiteral(m.qualName) + `, 'SELECT')`
	if !m.rls || bypassRLS {
		return `SELECT ` + priv, false
	}
	var perm, restr []string
	for _, pol := range m.policies {
		applies := false
		for _, r := range pol.roles {
			if r == "public" || r == role {
				applies = true
			}
		}
		if !applies {
			continue
		}
		q := "false" // a policy without USING grants nothing for SELECT
		if pol.qual.Valid {
			q = "(" + pol.qual.String + ")"
		}
		if pol.permissive {
			perm = append(perm, q)
		} else {
			restr = append(restr, q)
		}
	}
	if len(perm) == 0 {
		return `SELECT false`, false // RLS on and no permissive policy: nothing is visible
	}
	where := "(" + strings.Join(perm, " OR ") + ")"
	for _, r := range restr {
		where += " AND " + r
	}
	return fmt.Sprintf(`SELECT %s AND EXISTS (SELECT 1 FROM jsonb_populate_record(NULL::%s, $1::jsonb) AS %s WHERE %s)`,
		priv, m.qualName, m.alias, where), true
}

func (pl *rtPipeline) drain() {
	db, err := pl.p.app.projectDB(pl.p.slug)
	if err != nil {
		pl.setErr(err)
		return
	}
	for i := 0; i < 50; i++ { // bounded per wake-up; the next poll continues
		changes, err := fetchChanges(db)
		if err != nil {
			pl.setErr(err)
			return
		}
		if len(changes) == 0 {
			return
		}
		pl.process(db, changes)
		if len(changes) < rtDrainBatch {
			return
		}
	}
}

func fetchChanges(db *sql.DB) ([]rtChange, error) {
	rows, err := db.Query(fmt.Sprintf(`DELETE FROM realtime.ivb_changes WHERE id IN (SELECT id FROM realtime.ivb_changes ORDER BY id LIMIT %d)
		RETURNING id, at, schema_name, table_name, op,
			CASE WHEN pg_column_size(new_rec) > %[2]d THEN (SELECT jsonb_object_agg(k, v) FROM jsonb_each(new_rec) AS e(k, v) WHERE length(v::text) < 64) ELSE new_rec END,
			CASE WHEN pg_column_size(old_rec) > %[2]d THEN (SELECT jsonb_object_agg(k, v) FROM jsonb_each(old_rec) AS e(k, v) WHERE length(v::text) < 64) ELSE old_rec END,
			coalesce(pg_column_size(new_rec), 0) > %[2]d OR coalesce(pg_column_size(old_rec), 0) > %[2]d`, rtDrainBatch, rtBigRecord))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rtChange
	for rows.Next() {
		var c rtChange
		var nr, or []byte
		if err := rows.Scan(&c.id, &c.at, &c.schema, &c.table, &c.op, &nr, &or, &c.big); err != nil {
			return nil, err
		}
		if nr != nil {
			c.newRec = decodeRecord(nr)
		}
		if or != nil {
			c.oldRec = decodeRecord(or)
		}
		out = append(out, c)
	}
	sortChanges(out)
	return out, rows.Err()
}

func decodeRecord(b []byte) map[string]any {
	var m map[string]any
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber() // keep bigint / numeric exact
	d.Decode(&m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func sortChanges(c []rtChange) {
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j].id < c[j-1].id; j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}

func tableMeta(db *sql.DB, schema, table string) (*rtTableMeta, error) {
	var cols []byte
	m := &rtTableMeta{qualName: qualified(schema, table), alias: pq.QuoteIdentifier(table)}
	var ident string
	err := db.QueryRow(`SELECT c.relrowsecurity, c.relreplident::text,
		coalesce((SELECT json_agg(json_build_object('name', a.attname, 'type', t.typname, 'pk', coalesce(a.attnum = ANY(i.indkey), false)) ORDER BY a.attnum)
			FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
			LEFT JOIN pg_index i ON i.indrelid = a.attrelid AND i.indisprimary
			WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped), '[]')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2`, schema, table).
		Scan(&m.rls, &ident, &cols)
	if err != nil {
		return nil, err
	}
	m.full = ident == "f"
	var cs []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		PK   bool   `json:"pk"`
	}
	json.Unmarshal(cols, &cs)
	for _, c := range cs {
		m.columns = append(m.columns, map[string]any{"name": c.Name, "type": c.Type})
		if c.PK {
			m.pk = append(m.pk, c.Name)
		}
	}
	if !m.rls {
		return m, nil
	}
	rows, err := db.Query(`SELECT pol.polpermissive, pg_get_expr(pol.polqual, pol.polrelid),
			ARRAY(SELECT CASE WHEN r = 0 THEN 'public' ELSE r::regrole::text END FROM unnest(pol.polroles) AS r)
		FROM pg_policy pol WHERE pol.polrelid = $1::regclass AND pol.polcmd IN ('r', '*')`, m.qualName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pol rtPolicy
		if err := rows.Scan(&pol.permissive, &pol.qual, pq.Array(&pol.roles)); err != nil {
			return nil, err
		}
		for i, r := range pol.roles {
			pol.roles[i] = strings.Trim(r, `"`)
		}
		m.policies = append(m.policies, pol)
	}
	return m, rows.Err()
}

func publishedTables(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT schemaname, tablename FROM pg_publication_tables WHERE pubname = 'supabase_realtime'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s, t string
		rows.Scan(&s, &t)
		out[s+"."+t] = true
	}
	return out, rows.Err()
}

// rtSnap is a subscriber as seen by one drain pass.
type rtSnap struct {
	ch     *rtChan
	sub    *rtPGSub
	claims map[string]any
	role   string
	group  string
}

func (pl *rtPipeline) process(db *sql.DB, changes []rtChange) {
	p := pl.p
	pub, err := publishedTables(db)
	if err != nil {
		pl.setErr(err)
		return
	}
	p.mu.Lock()
	var snaps []rtSnap
	for _, set := range p.topics {
		for ch := range set {
			if len(ch.pg) == 0 {
				continue
			}
			cj, _ := json.Marshal(ch.claims)
			for _, s := range ch.pg {
				snaps = append(snaps, rtSnap{ch: ch, sub: s, claims: ch.claims, role: ch.role, group: ch.role + "|" + string(cj)})
			}
		}
	}
	p.mu.Unlock()

	metas := map[string]*rtTableMeta{}
	type delivery struct {
		data    map[string]any
		targets []rtSnap
		meta    *rtTableMeta
		rec     map[string]any // the row RLS is judged on: new, or old for DELETE
	}
	var dels []*delivery
	for i := range changes {
		c := &changes[i]
		key := c.schema + "." + c.table
		if !pub[key] {
			continue
		}
		var targets []rtSnap
		for _, s := range snaps {
			if s.sub.wants(c.schema, c.table, c.op) {
				targets = append(targets, s)
			}
		}
		if len(targets) == 0 {
			continue
		}
		m := metas[key]
		if m == nil {
			if m, err = tableMeta(db, c.schema, c.table); err != nil {
				continue // dropped meanwhile
			}
			metas[key] = m
		}
		data := map[string]any{"schema": c.schema, "table": c.table, "type": c.op, "columns": m.columns,
			"commit_timestamp": c.at.UTC().Format("2006-01-02T15:04:05.000Z"), "errors": nil}
		if c.big {
			data["errors"] = []string{"Error 413: Payload Too Large"}
		}
		var old map[string]any
		if c.op != "INSERT" {
			old = map[string]any{}
			if m.full && !m.rls {
				old = c.oldRec
			} else {
				for _, k := range m.pk {
					if v, ok := c.oldRec[k]; ok {
						old[k] = v
					}
				}
			}
			data["old_record"] = old
		}
		filterRec := old
		if c.op != "DELETE" {
			data["record"] = c.newRec
			filterRec = c.newRec
		}
		var matched []rtSnap
		for _, s := range targets {
			if s.sub.filter.match(filterRec) {
				matched = append(matched, s)
			}
		}
		if len(matched) == 0 {
			continue
		}
		d := &delivery{data: data, targets: matched, meta: m, rec: c.newRec}
		if c.op == "DELETE" {
			d.rec = c.oldRec
		}
		dels = append(dels, d)
	}
	if len(dels) == 0 {
		return
	}

	// visibility: one read-only transaction per distinct (role, claims)
	type groupJob struct {
		claims map[string]any
		idx    []int
	}
	groups := map[string]*groupJob{}
	for i, d := range dels {
		for _, s := range d.targets {
			g := groups[s.group]
			if g == nil {
				g = &groupJob{claims: s.claims}
				groups[s.group] = g
			}
			if n := len(g.idx); n == 0 || g.idx[n-1] != i {
				g.idx = append(g.idx, i)
			}
		}
	}
	visible := map[string]map[int]bool{}
	for key, g := range groups {
		vis := map[int]bool{}
		role, _ := g.claims["role"].(string)
		err := p.app.rtAsRole(p.slug, g.claims, func(tx *sql.Tx) error {
			var bypass bool
			if err := tx.QueryRow(`SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
				return err
			}
			for _, i := range g.idx {
				d := dels[i]
				vis[i] = rtCanSee(tx, d.meta, role, bypass, d.rec)
			}
			return nil
		})
		if err != nil {
			pl.setErr(fmt.Errorf("checking row visibility: %w", err))
		}
		visible[key] = vis
	}

	var delivered, total int64
	for i, d := range dels {
		total++
		perChan := map[*rtChan][]int64{}
		var order []*rtChan
		for _, s := range d.targets {
			if !visible[s.group][i] {
				continue
			}
			if _, ok := perChan[s.ch]; !ok {
				order = append(order, s.ch)
			}
			perChan[s.ch] = append(perChan[s.ch], s.sub.id)
		}
		p.mu.Lock()
		for _, ch := range order {
			if ch.c.chans[ch.topic] != ch {
				continue // left meanwhile
			}
			ch.c.send(rtEncode(ch.topic, "postgres_changes", map[string]any{"ids": perChan[ch], "data": d.data}, nil))
			delivered++
		}
		p.mu.Unlock()
	}
	pl.mu.Lock()
	pl.changes += total
	pl.delivered += delivered
	pl.lastErr = ""
	pl.mu.Unlock()
}

// rtCanSee: may the current (SET LOCAL ROLE) role see this row? Each check
// runs in a savepoint so one failure (e.g. a policy subquery on a table the
// role can't read) counts as "no" without aborting the others.
func rtCanSee(tx *sql.Tx, m *rtTableMeta, role string, bypassRLS bool, rec map[string]any) bool {
	q, usesRow := m.visibilityQuery(role, bypassRLS)
	var args []any
	if usesRow {
		rj, _ := json.Marshal(rec)
		args = []any{string(rj)}
	}
	if _, err := tx.Exec(`SAVEPOINT rt_check`); err != nil {
		return false
	}
	var ok bool
	if err := tx.QueryRow(q, args...).Scan(&ok); err != nil {
		tx.Exec(`ROLLBACK TO SAVEPOINT rt_check`)
		return false
	}
	tx.Exec(`RELEASE SAVEPOINT rt_check`)
	return ok
}

// rtAsRole is asRole for visibility checks: read-only, short timeout.
func (a *App) rtAsRole(slug string, claims map[string]any, fn func(tx *sql.Tx) error) error {
	role, _ := claims["role"].(string)
	switch role {
	case roleAnon, roleAuthed, roleService:
	default:
		return fmt.Errorf("invalid role %q", role)
	}
	db, err := a.apiDB(slug)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cj, _ := json.Marshal(claims)
	sub, _ := claims["sub"].(string)
	if _, err := tx.Exec(`SET LOCAL statement_timeout = '5s'`); err != nil {
		return err
	}
	if _, err := tx.Exec(`SET LOCAL ROLE ` + pq.QuoteIdentifier(role)); err != nil {
		return err
	}
	if _, err := tx.Exec(`SELECT set_config('request.jwt.claims', $1, true), set_config('request.jwt.claim.sub', $2, true), set_config('request.jwt.claim.role', $3, true)`,
		string(cj), sub, role); err != nil {
		return err
	}
	return fn(tx)
}

// realtimeTables lists the tables in the project's supabase_realtime publication.
func (a *App) realtimeTables(ctx context.Context, slug string) ([]map[string]any, error) {
	db, err := a.projectDB(slug)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT pt.schemaname, pt.tablename, c.relrowsecurity, c.relreplident::text = 'f',
			EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgrelid = c.oid AND t.tgname = 'ivarabase_realtime')
		FROM pg_publication_tables pt JOIN pg_namespace n ON n.nspname = pt.schemaname
		JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = pt.tablename
		WHERE pt.pubname = 'supabase_realtime' ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var s, t string
		var rls, full, watched bool
		if err := rows.Scan(&s, &t, &rls, &full, &watched); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"schema": s, "table": t, "rls_enabled": rls, "replica_identity_full": full, "watched": watched})
	}
	return out, rows.Err()
}
