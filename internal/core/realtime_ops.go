package core

import (
	"fmt"
	"sort"
)

func registerRealtimeOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}

	r.add(&Op{Name: "realtime.status", Tag: "realtime", Method: "GET", Path: "/api/v1/projects/{project}/realtime", Level: LevelRead,
		Summary: "Realtime for a project: connected clients, channels, presence, the postgres_changes feed and the tables in the supabase_realtime publication",
		Description: "Clients connect with supabase-js to <project url>/realtime/v1/websocket. Tables emit postgres_changes only while they are in the " +
			"supabase_realtime publication (realtime.set_table, or `alter publication supabase_realtime add table x`). Row level security applies to every subscriber.",
		Params: []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			if _, err := c.App.project(slug); err != nil {
				return nil, err
			}
			out := map[string]any{"url": c.App.projectURL(slug) + "/realtime/v1/websocket"}
			if p := rtHubs.get(slug); p != nil {
				for k, v := range p.status() {
					out[k] = v
				}
			} else {
				out["connections"] = 0
				out["max_connections"] = rtMaxConnsPerProject
				out["channels"] = []any{}
				out["clients"] = []any{}
				out["postgres_changes"] = map[string]any{"listening": false}
			}
			tables, err := c.App.realtimeTables(c, slug)
			if err != nil {
				return nil, sqlError(err)
			}
			out["tables"] = tables
			return out, nil
		}})

	r.add(&Op{Name: "realtime.overview", Tag: "realtime", Method: "GET", Path: "/api/v1/realtime", Level: LevelRead,
		Summary: "Realtime connections and channels per project",
		Run: func(c *Ctx, a Args) (any, error) {
			rtHubs.mu.Lock()
			var slugs []string
			for s := range rtHubs.projects {
				slugs = append(slugs, s)
			}
			total := rtHubs.total
			rtHubs.mu.Unlock()
			sort.Strings(slugs)
			projects := []map[string]any{}
			for _, s := range slugs {
				if !c.Principal.canProject(s) {
					continue
				}
				p := rtHubs.get(s)
				if p == nil {
					continue
				}
				st := p.status()
				projects = append(projects, map[string]any{"project": s, "connections": st["connections"], "channels": len(st["channels"].([]rtTopicInfo)),
					"postgres_changes": st["postgres_changes"]})
			}
			return map[string]any{"connections": total, "max_connections": rtMaxConnsTotal, "projects": projects}, nil
		}})

	r.add(&Op{Name: "realtime.set_table", Tag: "realtime", Method: "POST", Path: "/api/v1/projects/{project}/realtime/tables", Level: LevelWrite,
		Summary:     "Turn postgres_changes on or off for a table (adds it to / drops it from the supabase_realtime publication)",
		Description: "Same as `alter publication supabase_realtime add table x`. replica_identity_full=true makes UPDATE/DELETE events carry every old column (as in Supabase).",
		Params: []Param{projectParam, {Name: "table", Type: "string", Required: true}, {Name: "schema", Type: "string", Default: "public"},
			{Name: "enabled", Type: "boolean", Required: true}, {Name: "replica_identity_full", Type: "boolean", Desc: "also set REPLICA IDENTITY FULL (true) or DEFAULT (false)"}},
		Run: func(c *Ctx, a Args) (any, error) {
			slug := a.Str("project")
			if _, ok := realtimeReady.Load(slug); !ok {
				if err := c.App.ensureRealtimeSchema(slug); err != nil {
					return nil, &APIError{Status: 500, Code: "realtime_setup_failed", Message: err.Error()}
				}
			}
			db, err := c.App.projectDB(slug)
			if err != nil {
				return nil, err
			}
			tbl := qualified(a.Str("schema"), a.Str("table"))
			tables, err := c.App.realtimeTables(c, slug)
			if err != nil {
				return nil, sqlError(err)
			}
			in := false
			for _, t := range tables {
				if t["table"] == a.Str("table") && t["schema"] == schemaOr(a.Str("schema")) {
					in = true
				}
			}
			on := a.Bool("enabled", true)
			if on && !in {
				_, err = db.ExecContext(c, `ALTER PUBLICATION supabase_realtime ADD TABLE `+tbl)
			} else if !on && in {
				_, err = db.ExecContext(c, `ALTER PUBLICATION supabase_realtime DROP TABLE `+tbl)
			}
			if err != nil {
				return nil, sqlError(err)
			}
			if _, ok := a["replica_identity_full"]; ok {
				mode := "DEFAULT"
				if a.Bool("replica_identity_full", false) {
					mode = "FULL"
				}
				if _, err := db.ExecContext(c, fmt.Sprintf(`ALTER TABLE %s REPLICA IDENTITY %s`, tbl, mode)); err != nil {
					return nil, sqlError(err)
				}
			}
			tables, err = c.App.realtimeTables(c, slug)
			if err != nil {
				return nil, sqlError(err)
			}
			return map[string]any{"table": a.Str("table"), "enabled": on, "tables": tables}, nil
		}})
}

func schemaOr(s string) string {
	if s == "" {
		return "public"
	}
	return s
}
