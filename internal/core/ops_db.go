package core

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// sqlIsDestructive is a deliberately conservative heuristic: statements that
// drop or empty whole objects need confirmation (and owner approval for tokens).
func sqlIsDestructive(q string) bool {
	for _, stmt := range strings.Split(stripSQLComments(q), ";") {
		s := strings.Join(strings.Fields(strings.ToLower(stmt)), " ")
		switch {
		case strings.HasPrefix(s, "drop "), strings.HasPrefix(s, "truncate "):
			return true
		case strings.HasPrefix(s, "alter table") && strings.Contains(s, " drop "):
			return true
		case strings.HasPrefix(s, "delete from") && !strings.Contains(s, " where "):
			return true
		case strings.HasPrefix(s, "update ") && !strings.Contains(s, " where "):
			return true
		}
	}
	return false
}

func stripSQLComments(q string) string {
	var b strings.Builder
	for _, line := range strings.Split(q, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func registerDBOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}
	schemaParam := Param{Name: "schema", Type: "string", Default: "public"}
	tableParam := Param{Name: "table", Type: "string", Required: true}

	r.add(&Op{Name: "db.query", Tag: "database", Method: "POST", Path: "/api/v1/projects/{project}/sql", Level: LevelRead,
		Summary:     "Run SQL as the project role (read-only by default)",
		Description: "read_only=true runs in a READ ONLY transaction and only needs a read token. read_only=false needs write; statements that DROP, TRUNCATE or DELETE without WHERE also need confirm=true. Returns the last result set, capped by limit.",
		DestructiveIf: func(a Args) bool {
			return !a.Bool("read_only", true) && sqlIsDestructive(a.Str("sql"))
		},
		Params: []Param{projectParam,
			{Name: "sql", Type: "string", Required: true, Format: "sql"},
			{Name: "read_only", Type: "boolean", Default: true},
			{Name: "limit", Type: "integer", Default: 500, Desc: "max rows returned (1-5000)"},
			{Name: "timeout_s", Type: "integer", Default: 30}},
		Preview: func(c *Ctx, a Args) (any, error) {
			return map[string]any{"sql": a.Str("sql"), "warning": "this statement drops or empties data"}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			ro := a.Bool("read_only", true)
			if !ro {
				if err := c.require(LevelWrite); err != nil {
					return nil, err
				}
			}
			return c.App.runSQL(c, a.Str("project"), a.Str("sql"), ro, clamp(a.Int("limit", 500), 1, 5000), clamp(a.Int("timeout_s", 30), 1, 600))
		}})

	r.add(&Op{Name: "db.tables", Tag: "database", Method: "GET", Path: "/api/v1/projects/{project}/tables", Level: LevelRead,
		Summary: "List tables and views with row estimates and sizes",
		Params:  []Param{projectParam, {Name: "schema", Type: "string", Desc: "only this schema (default: all user schemas)"}},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			q := `SELECT n.nspname, c.relname, CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized_view' ELSE 'foreign_table' END,
				greatest(c.reltuples,0)::bigint, pg_total_relation_size(c.oid), coalesce(obj_description(c.oid,'pg_class'),''), c.relrowsecurity
				FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
				WHERE c.relkind IN ('r','p','v','m','f') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'
				AND NOT c.relispartition`
			var args []any
			if s := a.Str("schema"); s != "" {
				q += ` AND n.nspname=$1`
				args = append(args, s)
			}
			q += ` ORDER BY 1,2`
			rows, err := db.QueryContext(c, q, args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var schema, name, kind, comment string
				var est, size int64
				var rls bool
				if err := rows.Scan(&schema, &name, &kind, &est, &size, &comment, &rls); err != nil {
					return nil, err
				}
				out = append(out, map[string]any{"schema": schema, "name": name, "kind": kind, "rows_estimate": est, "size_bytes": size, "comment": comment, "rls_enabled": rls})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "db.columns", Tag: "database", Method: "GET", Path: "/api/v1/projects/{project}/tables/{table}/columns", Level: LevelRead,
		Summary: "Columns of a table: type, nullability, default, primary key, foreign keys",
		Params:  []Param{projectParam, tableParam, schemaParam},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			return tableColumns(c, db, a.Str("schema"), a.Str("table"))
		}})

	r.add(&Op{Name: "db.schema", Tag: "database", Method: "GET", Path: "/api/v1/projects/{project}/schema", Level: LevelRead,
		Summary: "Whole schema as {schema.table: [columns]} - for autocomplete and for agents planning a migration",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			rows, err := db.QueryContext(c, `SELECT table_schema, table_name, column_name, data_type
				FROM information_schema.columns WHERE table_schema NOT IN ('pg_catalog','information_schema')
				ORDER BY table_schema, table_name, ordinal_position`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := map[string][]map[string]string{}
			for rows.Next() {
				var s, t, col, typ string
				rows.Scan(&s, &t, &col, &typ)
				key := t
				if s != "public" {
					key = s + "." + t
				}
				out[key] = append(out[key], map[string]string{"name": col, "type": typ})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "db.rows", Tag: "database", Method: "GET", Path: "/api/v1/projects/{project}/tables/{table}/rows", Level: LevelRead,
		Summary: "Browse rows with paging, sorting and one filter",
		Params: []Param{projectParam, tableParam, schemaParam,
			{Name: "limit", Type: "integer", Default: 50},
			{Name: "offset", Type: "integer", Default: 0},
			{Name: "order_by", Type: "string", Desc: "column name (default: primary key)"},
			{Name: "desc", Type: "boolean", Default: false},
			{Name: "filter_column", Type: "string"},
			{Name: "filter_op", Type: "string", Enum: []string{"eq", "neq", "lt", "lte", "gt", "gte", "like", "ilike", "is_null", "not_null"}, Default: "eq"},
			{Name: "filter_value", Type: "string"}},
		Run: func(c *Ctx, a Args) (any, error) { return c.App.browseRows(c, a) }})

	r.add(&Op{Name: "db.row_insert", Tag: "database", Method: "POST", Path: "/api/v1/projects/{project}/tables/{table}/rows", Level: LevelWrite,
		Summary: "Insert one row; returns it with generated values",
		Params:  []Param{projectParam, tableParam, schemaParam, {Name: "values", Type: "object", Required: true, Desc: "column -> value"}},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			vals := a.Obj("values")
			if len(vals) == 0 {
				return nil, errBad("missing_parameter", "values must contain at least one column", "")
			}
			cols, err := tableColumns(c, db, a.Str("schema"), a.Str("table"))
			if err != nil {
				return nil, err
			}
			var names, ph []string
			var args []any
			for k, v := range vals {
				if !hasColumn(cols, k) {
					return nil, errBad("unknown_column", "no column "+k, "")
				}
				names = append(names, pq.QuoteIdentifier(k))
				args = append(args, sqlValue(v))
				ph = append(ph, fmt.Sprintf("$%d", len(args)))
			}
			q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) RETURNING *`, qualified(a.Str("schema"), a.Str("table")), strings.Join(names, ","), strings.Join(ph, ","))
			res, err := queryRows(c, db, q, args, 1)
			if err != nil {
				return nil, sqlError(err)
			}
			return map[string]any{"row": firstRow(res)}, nil
		}})

	r.add(&Op{Name: "db.row_update", Tag: "database", Method: "PATCH", Path: "/api/v1/projects/{project}/tables/{table}/rows", Level: LevelWrite,
		Summary: "Update one row identified by its primary key",
		Params: []Param{projectParam, tableParam, schemaParam,
			{Name: "key", Type: "object", Required: true, Desc: "primary key column(s) -> value"},
			{Name: "values", Type: "object", Required: true, Desc: "columns to change"}},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			cols, err := tableColumns(c, db, a.Str("schema"), a.Str("table"))
			if err != nil {
				return nil, err
			}
			where, args, err := pkWhere(cols, a.Obj("key"), nil)
			if err != nil {
				return nil, err
			}
			var sets []string
			for k, v := range a.Obj("values") {
				if !hasColumn(cols, k) {
					return nil, errBad("unknown_column", "no column "+k, "")
				}
				args = append(args, sqlValue(v))
				sets = append(sets, fmt.Sprintf("%s=$%d", pq.QuoteIdentifier(k), len(args)))
			}
			if len(sets) == 0 {
				return nil, errBad("missing_parameter", "values is empty", "")
			}
			q := fmt.Sprintf(`UPDATE %s SET %s WHERE %s RETURNING *`, qualified(a.Str("schema"), a.Str("table")), strings.Join(sets, ","), where)
			res, err := queryRows(c, db, q, args, 2)
			if err != nil {
				return nil, sqlError(err)
			}
			if len(res.Rows) == 0 {
				return nil, errNotFound("row_not_found", "no row matches that key", "")
			}
			return map[string]any{"row": firstRow(res)}, nil
		}})

	r.add(&Op{Name: "db.row_delete", Tag: "database", Method: "DELETE", Path: "/api/v1/projects/{project}/tables/{table}/rows", Level: LevelWrite,
		Summary: "Delete one row identified by its primary key",
		Params: []Param{projectParam, tableParam, schemaParam,
			{Name: "key", Type: "object", Required: true, Desc: "primary key column(s) -> value"}},
		Run: func(c *Ctx, a Args) (any, error) {
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			cols, err := tableColumns(c, db, a.Str("schema"), a.Str("table"))
			if err != nil {
				return nil, err
			}
			where, args, err := pkWhere(cols, a.Obj("key"), nil)
			if err != nil {
				return nil, err
			}
			res, err := db.ExecContext(c, fmt.Sprintf(`DELETE FROM %s WHERE %s`, qualified(a.Str("schema"), a.Str("table")), where), args...)
			if err != nil {
				return nil, sqlError(err)
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return nil, errNotFound("row_not_found", "no row matches that key", "")
			}
			return map[string]any{"deleted": n}, nil
		}})

	r.add(&Op{Name: "db.migrations_list", Tag: "database", Method: "GET", Path: "/api/v1/projects/{project}/migrations", Level: LevelRead,
		Summary: "Applied migrations, oldest first",
		Params:  []Param{projectParam, {Name: "include_sql", Type: "boolean", Default: false}},
		Run: func(c *Ctx, a Args) (any, error) {
			rows, err := c.App.Meta.Query(`SELECT version,name,sql,checksum,applied_at,applied_by FROM migrations WHERE project=$1 ORDER BY version`, a.Str("project"))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var v, n, s, cs, by string
				var at time.Time
				rows.Scan(&v, &n, &s, &cs, &at, &by)
				m := map[string]any{"version": v, "name": n, "checksum": cs, "applied_at": at, "applied_by": by}
				if a.Bool("include_sql", false) {
					m["sql"] = s
				}
				out = append(out, m)
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "db.migrations_apply", Tag: "database", Method: "POST", Path: "/api/v1/projects/{project}/migrations", Level: LevelWrite,
		Summary:       "Apply a migration in one transaction and record it",
		Description:   "Runs as the project role inside a transaction: either all of it applies or none. The same name+SQL applied twice is a no-op, so agents can retry safely.",
		DestructiveIf: func(a Args) bool { return sqlIsDestructive(a.Str("sql")) },
		Params: []Param{projectParam,
			{Name: "name", Type: "string", Required: true, Desc: "short description, e.g. create_orders"},
			{Name: "sql", Type: "string", Required: true, Format: "sql"}},
		Preview: func(c *Ctx, a Args) (any, error) {
			return map[string]any{"name": a.Str("name"), "sql": a.Str("sql"), "warning": "this migration drops or empties data"}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			project, name, q := a.Str("project"), strings.TrimSpace(a.Str("name")), a.Str("sql")
			sum := sha256Hex(name + "\n" + q)
			var existing string
			if c.App.Meta.QueryRow(`SELECT version FROM migrations WHERE project=$1 AND checksum=$2`, project, sum).Scan(&existing) == nil {
				return map[string]any{"version": existing, "name": name, "status": "already_applied"}, nil
			}
			db, err := c.App.projectDB(project)
			if err != nil {
				return nil, err
			}
			tx, err := db.BeginTx(c, nil)
			if err != nil {
				return nil, err
			}
			defer tx.Rollback()
			tx.ExecContext(c, `SET LOCAL statement_timeout = '300s'`)
			start := time.Now()
			if _, err := tx.ExecContext(c, q); err != nil {
				return nil, sqlError(err)
			}
			if err := tx.Commit(); err != nil {
				return nil, sqlError(err)
			}
			version := time.Now().UTC().Format("20060102150405.000")
			version = strings.Replace(version, ".", "", 1)
			if _, err := c.App.Meta.Exec(`INSERT INTO migrations(project,version,name,sql,checksum,applied_by) VALUES ($1,$2,$3,$4,$5,$6)`,
				project, version, name, q, sum, c.Principal.actor()); err != nil {
				return nil, err
			}
			return map[string]any{"version": version, "name": name, "status": "applied", "ms": time.Since(start).Milliseconds()}, nil
		}})
}

// ---------------------------------------------------------------- helpers

type queryResult struct {
	Columns   []map[string]string `json:"columns"`
	Rows      [][]any             `json:"rows"`
	RowCount  int                 `json:"row_count"`
	Truncated bool                `json:"truncated"`
	Command   string              `json:"command,omitempty"`
	Affected  int64               `json:"rows_affected,omitempty"`
	Ms        int64               `json:"ms"`
}

func (a *App) runSQL(ctx context.Context, project, q string, readOnly bool, limit, timeout int) (*queryResult, error) {
	if strings.TrimSpace(q) == "" {
		return nil, errBad("missing_parameter", "sql is empty", "")
	}
	db, err := a.projectDB(project)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout+2)*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return nil, sqlError(err)
	}
	defer tx.Rollback()
	tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = '%ds'`, timeout))
	start := time.Now()
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, sqlError(err)
	}
	var last *queryResult
	for {
		res, err := collect(rows, limit)
		if err != nil {
			rows.Close()
			return nil, sqlError(err)
		}
		if len(res.Columns) > 0 || last == nil {
			last = res
		}
		if !rows.NextResultSet() {
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, sqlError(err)
	}
	rows.Close()
	if !readOnly {
		if err := tx.Commit(); err != nil {
			return nil, sqlError(err)
		}
	}
	last.Ms = time.Since(start).Milliseconds()
	if len(last.Columns) == 0 {
		last.Command = "ok"
	}
	return last, nil
}

func collect(rows *sql.Rows, limit int) (*queryResult, error) {
	res := &queryResult{Rows: [][]any{}, Columns: []map[string]string{}}
	cols, err := rows.ColumnTypes()
	if err != nil {
		return res, nil
	}
	for _, c := range cols {
		res.Columns = append(res.Columns, map[string]string{"name": c.Name(), "type": strings.ToLower(c.DatabaseTypeName())})
	}
	for rows.Next() {
		if len(res.Rows) >= limit {
			res.Truncated = true
			continue // drain to the end of this result set
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			vals[i] = jsonValue(v, res.Columns[i]["type"])
		}
		res.Rows = append(res.Rows, vals)
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

func jsonValue(v any, typ string) any {
	switch t := v.(type) {
	case []byte:
		if typ == "bytea" {
			return `\x` + hex.EncodeToString(t)
		}
		if typ == "json" || typ == "jsonb" {
			var j any
			if json.Unmarshal(t, &j) == nil {
				return j
			}
		}
		return string(t)
	case time.Time:
		if typ == "date" {
			return t.Format("2006-01-02")
		}
		return t.Format(time.RFC3339Nano)
	}
	return v
}

// sqlValue maps JSON input to a driver value (objects/arrays become JSON text).
func sqlValue(v any) any {
	switch t := v.(type) {
	case map[string]any, []any:
		b, _ := json.Marshal(t)
		return string(b)
	}
	return v
}

func queryRows(ctx context.Context, db *sql.DB, q string, args []any, limit int) (*queryResult, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res, err := collect(rows, limit)
	if err != nil {
		return nil, err
	}
	return res, rows.Err()
}

func firstRow(res *queryResult) map[string]any {
	if len(res.Rows) == 0 {
		return nil
	}
	m := map[string]any{}
	for i, c := range res.Columns {
		m[c["name"]] = res.Rows[0][i]
	}
	return m
}

func sqlError(err error) error {
	var pe *pq.Error
	if errors.As(err, &pe) {
		ae := &APIError{Status: 400, Code: "sql_error", Message: pe.Message,
			Details: map[string]any{"sqlstate": string(pe.Code), "detail": pe.Detail, "position": pe.Position}}
		if pe.Hint != "" {
			ae.Hint = pe.Hint
		}
		if pe.Code == "25006" {
			ae.Hint = "this statement writes; run it with read_only=false"
		}
		if pe.Code == "57014" {
			ae.Code, ae.Hint = "sql_timeout", "the statement hit timeout_s; raise it or add an index"
		}
		return ae
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &APIError{Status: 408, Code: "sql_timeout", Message: "query timed out"}
	}
	return err
}

type column struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Nullable   bool   `json:"nullable"`
	Default    string `json:"default,omitempty"`
	PrimaryKey bool   `json:"primary_key"`
	References string `json:"references,omitempty"`
	Identity   bool   `json:"generated"`
}

func tableColumns(ctx context.Context, db *sql.DB, schema, table string) ([]column, error) {
	if schema == "" {
		schema = "public"
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
		  coalesce(pg_get_expr(d.adbin, d.adrelid),''),
		  coalesce((SELECT true FROM pg_index i WHERE i.indrelid=c.oid AND i.indisprimary AND a.attnum = ANY(i.indkey)), false),
		  coalesce((SELECT confrelid::regclass::text || '.' || (SELECT attname FROM pg_attribute WHERE attrelid=f.confrelid AND attnum=f.confkey[1])
		     FROM pg_constraint f WHERE f.conrelid=c.oid AND f.contype='f' AND f.conkey[1]=a.attnum LIMIT 1),''),
		  (a.attidentity <> '' OR a.attgenerated <> '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum
		WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped
		ORDER BY a.attnum`, schema, table)
	if err != nil {
		return nil, sqlError(err)
	}
	defer rows.Close()
	var out []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &c.PrimaryKey, &c.References, &c.Identity); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errNotFound("table_not_found", fmt.Sprintf("table %s.%s not found", schema, table), "call db.tables to list tables")
	}
	return out, rows.Err()
}

func hasColumn(cols []column, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

func qualified(schema, table string) string {
	if schema == "" {
		schema = "public"
	}
	return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table)
}

func pkWhere(cols []column, key map[string]any, args []any) (string, []any, error) {
	if len(key) == 0 {
		return "", nil, errBad("missing_parameter", "key must name the primary key column(s)", "")
	}
	var pks []string
	for _, c := range cols {
		if c.PrimaryKey {
			pks = append(pks, c.Name)
		}
	}
	if len(pks) == 0 {
		return "", nil, errBad("no_primary_key", "this table has no primary key, so single-row edits are not possible", "use db.query with read_only=false and a WHERE clause")
	}
	var parts []string
	for _, pk := range pks {
		v, ok := key[pk]
		if !ok {
			return "", nil, errBad("missing_parameter", "key must include primary key column "+pk, "")
		}
		args = append(args, sqlValue(v))
		parts = append(parts, fmt.Sprintf("%s=$%d", pq.QuoteIdentifier(pk), len(args)))
	}
	return strings.Join(parts, " AND "), args, nil
}

func (a *App) browseRows(c *Ctx, args Args) (any, error) {
	db, err := a.projectDB(args.Str("project"))
	if err != nil {
		return nil, err
	}
	schema, table := args.Str("schema"), args.Str("table")
	cols, err := tableColumns(c, db, schema, table)
	if err != nil {
		return nil, err
	}
	tbl := qualified(schema, table)
	where := ""
	var qargs []any
	if fc := args.Str("filter_column"); fc != "" {
		if !hasColumn(cols, fc) {
			return nil, errBad("unknown_column", "no column "+fc, "")
		}
		col := pq.QuoteIdentifier(fc)
		ops := map[string]string{"eq": "=", "neq": "<>", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}
		switch op := args.Str("filter_op"); op {
		case "is_null":
			where = " WHERE " + col + " IS NULL"
		case "not_null":
			where = " WHERE " + col + " IS NOT NULL"
		case "like", "ilike":
			qargs = append(qargs, args.Str("filter_value"))
			where = fmt.Sprintf(" WHERE %s::text %s $1", col, strings.ToUpper(op))
		default:
			qargs = append(qargs, args.Str("filter_value"))
			where = fmt.Sprintf(" WHERE %s::text %s $1", col, ops[op])
			if op == "eq" || op == "neq" {
				// compare in the column's own type so numbers and uuids match exactly
				where = fmt.Sprintf(" WHERE %s %s $1", col, ops[op])
			}
		}
	}
	order := ""
	ob := args.Str("order_by")
	if ob == "" {
		for _, col := range cols {
			if col.PrimaryKey {
				ob = col.Name
				break
			}
		}
	}
	if ob != "" {
		if !hasColumn(cols, ob) {
			return nil, errBad("unknown_column", "no column "+ob, "")
		}
		order = " ORDER BY " + pq.QuoteIdentifier(ob)
		if args.Bool("desc", false) {
			order += " DESC"
		}
	}
	limit := clamp(args.Int("limit", 50), 1, 1000)
	offset := max(0, args.Int("offset", 0))
	ctx, cancel := context.WithTimeout(c, 20*time.Second)
	defer cancel()
	res, err := queryRows(ctx, db, fmt.Sprintf(`SELECT * FROM %s%s%s LIMIT %d OFFSET %d`, tbl, where, order, limit, offset), qargs, limit)
	if err != nil {
		return nil, sqlError(err)
	}
	var total int64 = -1
	cctx, ccancel := context.WithTimeout(c, 3*time.Second)
	defer ccancel()
	db.QueryRowContext(cctx, fmt.Sprintf(`SELECT count(*) FROM %s%s`, tbl, where), qargs...).Scan(&total)
	return map[string]any{"columns": cols, "rows": res.Rows, "total": total, "limit": limit, "offset": offset}, nil
}
