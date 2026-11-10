package core

import (
	"database/sql"
	"strings"
	"time"
)

func registerMonitorOps(r *Registry) {
	r.add(&Op{Name: "monitor.overview", Tag: "monitoring", Method: "GET", Path: "/api/v1/monitor/overview", Level: LevelRead,
		Summary:     "Current host, Postgres and per-project numbers, plus firing alerts",
		Description: "Values are from the most recent one-minute sample. Per-project request totals cover the last hour.",
		Run: func(c *Ctx, a Args) (any, error) {
			out := map[string]any{"host": c.App.latest("host"), "pg": c.App.latest("pg"), "cpus": numCPU()}
			projects := []map[string]any{}
			rows, err := c.App.Meta.Query(`SELECT p.slug, p.name,
				coalesce(sum(m.value) FILTER (WHERE m.name='req_total'),0),
				coalesce(sum(m.value) FILTER (WHERE m.name='err_total'),0),
				coalesce(sum(m.value * r.value) FILTER (WHERE m.name='lat_avg') / nullif(sum(r.value) FILTER (WHERE m.name='lat_avg'),0), 0)
				FROM projects p
				LEFT JOIN metrics_samples m ON m.scope='p:'||p.slug AND m.ts > now() - interval '1 hour' AND m.name IN ('req_total','err_total','lat_avg')
				LEFT JOIN metrics_samples r ON r.scope=m.scope AND r.ts=m.ts AND r.name='req_total' AND m.name='lat_avg'
				GROUP BY p.slug, p.name ORDER BY p.slug`)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var slug, name string
				var req, errs, lat float64
				rows.Scan(&slug, &name, &req, &errs, &lat)
				if !c.Principal.canProject(slug) {
					continue
				}
				l := c.App.latest("p:" + slug)
				rate := 0.0
				if req > 0 {
					rate = 100 * errs / req
				}
				projects = append(projects, map[string]any{"slug": slug, "name": name, "requests_1h": req, "errors_1h": errs,
					"error_rate_pct": round2(rate), "avg_ms_1h": round2(lat), "db_size_mb": l["db_size_mb"], "connections": l["connections"], "tps": l["tps"]})
			}
			rows.Close()
			out["projects"] = projects
			firing := []map[string]any{}
			fr, err := c.App.Meta.Query(`SELECT r.name, s.subject, s.value, s.since, r.metric, r.op, r.threshold FROM alert_state s JOIN alert_rules r ON r.id=s.rule_id
				WHERE s.firing ORDER BY s.since`)
			if err == nil {
				for fr.Next() {
					var name, subj, metric, op string
					var v, th float64
					var since time.Time
					fr.Scan(&name, &subj, &v, &since, &metric, &op, &th)
					firing = append(firing, map[string]any{"rule": name, "subject": strings.TrimPrefix(subj, "p:"), "value": round2(v), "since": since,
						"metric": metric, "op": op, "threshold": th})
				}
				fr.Close()
			}
			out["firing"] = firing
			var first *time.Time
			c.App.Meta.QueryRow(`SELECT min(ts) FROM metrics_samples`).Scan(&first)
			out["collecting_since"] = first
			return out, nil
		}})

	r.add(&Op{Name: "monitor.series", Tag: "monitoring", Method: "GET", Path: "/api/v1/monitor/series", Level: LevelRead,
		Summary: "Time series for charts",
		Description: "scope: host | pg | <project slug>. Host metrics: cpu_pct, load1, load_per_cpu, mem_used_pct, mem_used_mb, disk_used_pct, disk_free_gb, rss_postgres_mb, rss_postgrest_mb, rss_deno_mb, rss_ivarabase_mb, health_errors. " +
			"Postgres: connections, connections_pct, tps, cache_hit_pct, db_total_mb. Project: req_rest|auth|storage|functions|total and err_* (per minute), lat_* (ms), db_size_mb, connections, tps.",
		Params: []Param{{Name: "scope", Type: "string", Required: true}, {Name: "metrics", Type: "array", Required: true},
			{Name: "range", Type: "string", Enum: []string{"1h", "6h", "24h", "7d"}, Default: "24h"}},
		Run: func(c *Ctx, a Args) (any, error) {
			scope := a.Str("scope")
			if scope != "host" && scope != "pg" {
				if !c.Principal.canProject(scope) {
					return nil, errForbidden("not allowed for this project", "")
				}
				if _, err := c.App.project(scope); err != nil {
					return nil, err
				}
				scope = "p:" + scope
			}
			names := a.StrList("metrics")
			if len(names) == 0 || len(names) > 12 {
				return nil, errBad("invalid_parameter", "send 1-12 metric names", "")
			}
			return c.App.series(c, scope, names, a.Str("range"))
		}})

	r.add(&Op{Name: "alerts.rules_list", Tag: "monitoring", Method: "GET", Path: "/api/v1/alerts/rules", Level: LevelRead,
		Summary: "Alert rules with their current state",
		Run: func(c *Ctx, a Args) (any, error) {
			rules, err := c.App.alertRules()
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, r := range rules {
				firing := []string{}
				rows, err := c.App.Meta.Query(`SELECT subject FROM alert_state WHERE rule_id=$1 AND firing`, r.ID)
				if err == nil {
					for rows.Next() {
						var s string
						rows.Scan(&s)
						firing = append(firing, strings.TrimPrefix(s, "p:"))
					}
					rows.Close()
				}
				out = append(out, map[string]any{"id": r.ID, "name": r.Name, "scope": r.Scope, "metric": r.Metric, "op": r.Op,
					"threshold": r.Threshold, "for_minutes": r.ForMinutes, "enabled": r.Enabled, "builtin": r.Builtin, "firing": firing})
			}
			return out, nil
		}})

	r.add(&Op{Name: "alerts.rule_save", Tag: "monitoring", Method: "PUT", Path: "/api/v1/alerts/rules", Level: LevelAdmin,
		Summary: "Create or update an alert rule (send id to update)",
		Params: []Param{{Name: "id", Type: "integer"}, {Name: "name", Type: "string"},
			{Name: "scope", Type: "string", Enum: []string{"host", "pg", "project"}}, {Name: "metric", Type: "string"},
			{Name: "op", Type: "string", Enum: []string{">", "<"}}, {Name: "threshold", Type: "integer"},
			{Name: "for_minutes", Type: "integer", Desc: "the average over this many minutes must breach (1-120)"},
			{Name: "enabled", Type: "boolean"}},
		Run: func(c *Ctx, a Args) (any, error) {
			id := int64(a.Int("id", 0))
			r := alertRule{Op: ">", ForMinutes: 5, Enabled: true}
			if id > 0 {
				rules, _ := c.App.alertRules()
				found := false
				for _, x := range rules {
					if x.ID == id {
						r, found = x, true
					}
				}
				if !found {
					return nil, errNotFound("rule_not_found", "no alert rule with that id", "")
				}
			}
			if v := a.Str("name"); v != "" {
				r.Name = v
			}
			if v := a.Str("scope"); v != "" {
				r.Scope = v
			}
			if v := a.Str("metric"); v != "" {
				r.Metric = v
			}
			if v := a.Str("op"); v != "" {
				r.Op = v
			}
			if _, ok := a["threshold"]; ok {
				if f, ok := a["threshold"].(float64); ok {
					r.Threshold = f
				}
			}
			if _, ok := a["for_minutes"]; ok {
				r.ForMinutes = clamp(a.Int("for_minutes", 5), 1, 120)
			}
			if _, ok := a["enabled"]; ok {
				r.Enabled = a.Bool("enabled", true)
			}
			if r.Name == "" || r.Scope == "" || r.Metric == "" {
				return nil, errBad("missing_parameter", "name, scope and metric are required", "")
			}
			var err error
			if id > 0 {
				_, err = c.App.Meta.Exec(`UPDATE alert_rules SET name=$2, scope=$3, metric=$4, op=$5, threshold=$6, for_minutes=$7, enabled=$8 WHERE id=$1`,
					id, r.Name, r.Scope, r.Metric, r.Op, r.Threshold, r.ForMinutes, r.Enabled)
				if !r.Enabled {
					c.App.Meta.Exec(`DELETE FROM alert_state WHERE rule_id=$1`, id)
				}
			} else {
				err = c.App.Meta.QueryRow(`INSERT INTO alert_rules(name, scope, metric, op, threshold, for_minutes, enabled) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
					r.Name, r.Scope, r.Metric, r.Op, r.Threshold, r.ForMinutes, r.Enabled).Scan(&id)
			}
			if err != nil {
				return nil, err
			}
			r.ID = id
			return r, nil
		}})

	r.add(&Op{Name: "alerts.rule_delete", Tag: "monitoring", Method: "DELETE", Path: "/api/v1/alerts/rules/{id}", Level: LevelAdmin,
		Summary: "Delete an alert rule",
		Params:  []Param{{Name: "id", Type: "integer", Required: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			res, err := c.App.Meta.Exec(`DELETE FROM alert_rules WHERE id=$1`, a.Int("id", 0))
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, errNotFound("rule_not_found", "no alert rule with that id", "")
			}
			c.App.Meta.Exec(`DELETE FROM alert_state WHERE rule_id=$1`, a.Int("id", 0))
			return map[string]any{"deleted": a.Int("id", 0)}, nil
		}})

	r.add(&Op{Name: "alerts.channels_get", Tag: "monitoring", Method: "GET", Path: "/api/v1/alerts/channels", Level: LevelAdmin,
		Summary: "Where alerts are sent (webhook, Discord, Slack)",
		Run: func(c *Ctx, a Args) (any, error) {
			ch := c.App.alertChannels()
			if ch == nil {
				ch = []alertChannel{}
			}
			return ch, nil
		}})

	r.add(&Op{Name: "alerts.channels_set", Tag: "monitoring", Method: "PUT", Path: "/api/v1/alerts/channels", Level: LevelAdmin,
		Summary:     "Replace the list of alert channels",
		Description: "Each channel: {name, type: webhook|discord|slack, url, enabled}. webhook receives a JSON event; discord/slack receive a text message (also works with ntfy/Mattermost-style incoming webhooks).",
		Params:      []Param{{Name: "channels", Type: "array", Items: "object", Required: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			arr, _ := a["channels"].([]any)
			out := []alertChannel{}
			for i, v := range arr {
				m, _ := v.(map[string]any)
				ch := alertChannel{ID: str(m, "id"), Name: str(m, "name"), Type: str(m, "type"), URL: strings.TrimSpace(str(m, "url")), Enabled: true}
				if e, ok := m["enabled"].(bool); ok {
					ch.Enabled = e
				}
				if ch.Type != "discord" && ch.Type != "slack" {
					ch.Type = "webhook"
				}
				if !strings.HasPrefix(ch.URL, "https://") && !strings.HasPrefix(ch.URL, "http://") {
					return nil, errBad("invalid_parameter", "channel url must start with https://", "")
				}
				if ch.ID == "" {
					ch.ID = randHex(4)
				}
				if ch.Name == "" {
					ch.Name = ch.Type + " " + string(rune('1'+i))
				}
				out = append(out, ch)
			}
			if err := c.App.setSetting("alert_channels", out); err != nil {
				return nil, err
			}
			return out, nil
		}})

	r.add(&Op{Name: "alerts.test", Tag: "monitoring", Method: "POST", Path: "/api/v1/alerts/test", Level: LevelAdmin,
		Summary: "Send a test notification to every enabled channel",
		Run: func(c *Ctx, a Args) (any, error) {
			msg := "[TEST] IvaraBase alerts are working"
			res := c.App.notify(c, map[string]any{"event": "test", "message": msg, "at": time.Now().UTC()}, msg)
			if len(res) == 0 {
				return nil, errBad("no_channels", "add an enabled channel first", "")
			}
			return map[string]any{"results": res}, nil
		}})

	r.add(&Op{Name: "alerts.events", Tag: "monitoring", Method: "GET", Path: "/api/v1/alerts/events", Level: LevelRead,
		Summary: "Alert history: when rules fired and resolved",
		Params:  []Param{{Name: "limit", Type: "integer", Default: 100}},
		Run: func(c *Ctx, a Args) (any, error) {
			rows, err := c.App.Meta.Query(`SELECT id, at, rule_name, subject, state, value, message FROM alert_events ORDER BY at DESC LIMIT $1`, clamp(a.Int("limit", 100), 1, 1000))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id int64
				var at time.Time
				var name, subj, state, msg string
				var v sql.NullFloat64
				rows.Scan(&id, &at, &name, &subj, &state, &v, &msg)
				out = append(out, map[string]any{"id": id, "at": at, "rule": name, "subject": strings.TrimPrefix(subj, "p:"), "state": state, "value": v.Float64, "message": msg})
			}
			return out, rows.Err()
		}})
}
