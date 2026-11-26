package core

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ---------------------------------------------------------------- log partitions
//
// The meta database's log tables are partitioned by day. Old data is removed
// by dropping whole partitions, which gives the disk space back at once
// (a DELETE only leaves dead rows behind until VACUUM, and the file never
// shrinks). Each table also has a DEFAULT partition as a safety net for rows
// outside the daily ranges.

type logTable struct {
	name string
	col  string // partition key
	keep int    // days kept; 0 means the configurable audit retention
}

var logTables = []logTable{
	{"function_logs", "at", 14},
	{"function_console", "at", 7},
	{"audit", "at", 0},
	{"auth_mails", "at", 30},
	{"auth_sms", "at", 30},
	{"metrics_samples", "ts", 7},
}

const defaultAuditRetentionDays = 30

func (a *App) auditRetentionDays() int {
	n := defaultAuditRetentionDays
	a.getSetting("audit_retention_days", &n)
	return clamp(n, 1, 3650)
}

func (t logTable) retention(a *App) int {
	if t.keep == 0 {
		return a.auditRetentionDays()
	}
	return t.keep
}

var partDayRe = regexp.MustCompile(`_p(\d{8})$`)

// maintainLogPartitions creates the partitions for today and the next three
// days and drops the ones that are entirely older than each table's retention.
func (a *App) maintainLogPartitions() {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, t := range logTables {
		for d := 0; d <= 3; d++ {
			day := today.AddDate(0, 0, d).Format("2006-01-02")
			if _, err := a.Meta.Exec(`SELECT ivb_log_partition($1, $2::date)`, t.name, day); err != nil {
				log.Printf("log partitions: create %s %s: %v", t.name, day, err)
			}
		}
		cutoff := today.AddDate(0, 0, -t.retention(a))
		rows, err := a.Meta.Query(`SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = $1::regclass`, t.name)
		if err != nil {
			log.Printf("log partitions: list %s: %v", t.name, err)
			continue
		}
		var drop []string
		for rows.Next() {
			var rel string
			rows.Scan(&rel)
			m := partDayRe.FindStringSubmatch(rel)
			if m == nil || !strings.HasPrefix(rel, t.name+"_p") {
				continue
			}
			day, err := time.Parse("20060102", m[1])
			if err != nil {
				continue
			}
			if !day.AddDate(0, 0, 1).After(cutoff) { // the whole day is before the cutoff
				drop = append(drop, rel)
			}
		}
		rows.Close()
		for _, rel := range drop {
			if _, err := a.Meta.Exec(`DROP TABLE IF EXISTS ` + pq.QuoteIdentifier(rel)); err != nil {
				log.Printf("log partitions: drop %s: %v", rel, err)
			}
		}
		a.Meta.Exec(fmt.Sprintf(`DELETE FROM %s WHERE %s < $1`, pq.QuoteIdentifier(t.name+"_default"), pq.QuoteIdentifier(t.col)), cutoff)
	}
}

// partitionLogsMigration converts the log tables into tables partitioned by
// day. Rows still inside each table's retention are copied over; older rows
// were due for deletion anyway.
func partitionLogsMigration() string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE FUNCTION ivb_log_partition(tbl text, day date) RETURNS void LANGUAGE plpgsql AS $f$
DECLARE pname text := tbl || '_p' || to_char(day, 'YYYYMMDD');
BEGIN
	IF to_regclass(quote_ident(pname)) IS NULL THEN
		EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)', pname, tbl,
			to_char(day, 'YYYY-MM-DD') || ' 00:00:00+00', to_char(day + 1, 'YYYY-MM-DD') || ' 00:00:00+00');
	END IF;
END $f$;
`)
	type spec struct {
		name, col string
		keep      int
		hasID     bool
		columns   string   // column definitions, same order as the old table
		oldIdx    []string // indexes to move out of the way
		newIdx    []string
	}
	specs := []spec{
		{"function_logs", "at", 14, true, `id bigserial, project text NOT NULL, name text NOT NULL, at timestamptz NOT NULL DEFAULT now(),
			status int NOT NULL, ms int NOT NULL, error text NOT NULL DEFAULT ''`,
			[]string{"function_logs_p"}, []string{`CREATE INDEX function_logs_p ON function_logs(project, at DESC)`}},
		{"function_console", "at", 7, true, `id bigserial, project text NOT NULL, name text NOT NULL, at timestamptz NOT NULL,
			stream text NOT NULL, line text NOT NULL`,
			[]string{"function_console_p"}, []string{`CREATE INDEX function_console_p ON function_console(project, name, id DESC)`}},
		{"audit", "at", defaultAuditRetentionDays, true, `id bigserial, at timestamptz NOT NULL DEFAULT now(), actor text NOT NULL, op text NOT NULL,
			project text NOT NULL DEFAULT '', detail jsonb NOT NULL DEFAULT '{}', ok boolean NOT NULL, error text NOT NULL DEFAULT ''`,
			[]string{"audit_at"}, []string{`CREATE INDEX audit_at ON audit(at DESC)`}},
		{"auth_mails", "at", 30, true, `id bigserial, project text NOT NULL, at timestamptz NOT NULL DEFAULT now(), to_addr text NOT NULL,
			subject text NOT NULL, body text NOT NULL, kind text NOT NULL, delivered boolean NOT NULL, error text NOT NULL DEFAULT '',
			link text NOT NULL DEFAULT ''`,
			[]string{"auth_mails_p"}, []string{`CREATE INDEX auth_mails_p ON auth_mails(project, at DESC)`}},
		{"auth_sms", "at", 30, true, `id bigserial, project text NOT NULL, at timestamptz NOT NULL DEFAULT now(), to_phone text NOT NULL,
			body text NOT NULL, kind text NOT NULL, delivered boolean NOT NULL, error text NOT NULL DEFAULT '',
			message_id text NOT NULL DEFAULT ''`,
			[]string{"auth_sms_p"}, []string{`CREATE INDEX auth_sms_p ON auth_sms(project, at DESC)`}},
		{"metrics_samples", "ts", 7, false, `ts timestamptz NOT NULL, scope text NOT NULL, name text NOT NULL, value double precision NOT NULL`,
			[]string{"metrics_samples_i", "metrics_samples_ts"},
			[]string{`CREATE INDEX metrics_samples_i ON metrics_samples(scope, name, ts)`, `CREATE INDEX metrics_samples_ts ON metrics_samples(ts)`}},
	}
	for _, s := range specs {
		old := s.name + "_old"
		fmt.Fprintf(&b, "ALTER TABLE %s RENAME TO %s;\n", s.name, old)
		for _, ix := range s.oldIdx {
			fmt.Fprintf(&b, "ALTER INDEX %s RENAME TO %s_old;\n", ix, ix)
		}
		cols := s.columns
		if s.hasID {
			fmt.Fprintf(&b, "ALTER TABLE %s RENAME CONSTRAINT %s_pkey TO %s_pkey;\n", old, s.name, old)
			fmt.Fprintf(&b, "ALTER SEQUENCE %s_id_seq RENAME TO %s_id_seq;\n", s.name, old)
			cols += ", PRIMARY KEY (id, " + s.col + ")"
		}
		fmt.Fprintf(&b, "CREATE TABLE %s (%s) PARTITION BY RANGE (%s);\n", s.name, cols, s.col)
		for _, ix := range s.newIdx {
			b.WriteString(ix + ";\n")
		}
		fmt.Fprintf(&b, "CREATE TABLE %s_default PARTITION OF %s DEFAULT;\n", s.name, s.name)
		fmt.Fprintf(&b, `SELECT ivb_log_partition('%s', d::date) FROM generate_series((now() AT TIME ZONE 'UTC')::date - %d, (now() AT TIME ZONE 'UTC')::date + 3, interval '1 day') d;`+"\n", s.name, s.keep)
		fmt.Fprintf(&b, `INSERT INTO %s SELECT * FROM %s WHERE %s >= ((now() AT TIME ZONE 'UTC')::date - %d)::timestamp AT TIME ZONE 'UTC';`+"\n", s.name, old, s.col, s.keep)
		if s.hasID {
			fmt.Fprintf(&b, "SELECT setval('%s_id_seq', coalesce((SELECT max(id) FROM %s), 0) + 1, false);\n", s.name, old)
		}
		fmt.Fprintf(&b, "DROP TABLE %s;\n", old)
	}
	return b.String()
}

// ---------------------------------------------------------------- ops

func registerLogOps(r *Registry) {
	r.add(&Op{Name: "system.audit_retention_get", Tag: "system", Method: "GET", Path: "/api/v1/system/audit-retention", Level: LevelAdmin,
		Summary: "How many days the audit log is kept",
		Run: func(c *Ctx, a Args) (any, error) {
			return map[string]any{"days": c.App.auditRetentionDays()}, nil
		}})
	r.add(&Op{Name: "system.audit_retention_set", Tag: "system", Method: "PUT", Path: "/api/v1/system/audit-retention", Level: LevelAdmin,
		Summary: "Change how many days the audit log is kept (1-3650, default 30). Older days are dropped within the hour.",
		Params:  []Param{{Name: "days", Type: "integer", Required: true, Desc: "days to keep (1-3650)"}},
		Run: func(c *Ctx, a Args) (any, error) {
			n := clamp(a.Int("days", defaultAuditRetentionDays), 1, 3650)
			if err := c.App.setSetting("audit_retention_days", n); err != nil {
				return nil, err
			}
			return map[string]any{"days": n, "saved": true}, nil
		}})
}
