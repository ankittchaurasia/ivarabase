import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, Bell, Cpu, Database, HardDrive, MemoryStick, Plus, Send, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { LineChart, type ChartSeries } from "../components/Chart";
import { Badge, Button, Callout, Card, cn, DataTable, Dialog, Empty, ErrorBox, Field, Input, Loading, PageHeader, Select, Stat, Switch, Tabs, toast } from "../components/ui";
import { op, useOp } from "../lib/api";
import { ago, date } from "../lib/format";

export type Range = "1h" | "6h" | "24h" | "7d";

export function RangePicker({ value, onChange }: { value: Range; onChange: (r: Range) => void }) {
  return (
    <div className="inline-flex rounded-md border bg-card p-0.5">
      {(["1h", "6h", "24h", "7d"] as Range[]).map((r) => (
        <button
          key={r}
          onClick={() => onChange(r)}
          className={cn("rounded-[5px] px-2.5 py-1 font-mono text-[12px] transition-colors", value === r ? "bg-accent font-semibold text-foreground" : "text-muted-foreground hover:text-foreground")}
        >
          {r}
        </button>
      ))}
    </div>
  );
}

type SeriesResp = { series: Record<string, [number, number | null][]> };

/** Fetches series and renders one chart. */
export function MetricChart({
  scope,
  range,
  metrics,
  unit,
  yMax,
  height,
}: {
  scope: string;
  range: Range;
  metrics: { name: string; label: string; slot?: 1 | 2 | 3 | 4 | "primary" }[];
  unit?: string;
  yMax?: number;
  height?: number;
}) {
  const q = useOp<SeriesResp>("monitor.series", { scope, metrics: metrics.map((m) => m.name), range }, { refetchInterval: 60_000, placeholderData: (p) => p });
  if (q.error) return <ErrorBox error={q.error} />;
  if (!q.data) return <div style={{ height: (height ?? 170) + 20 }} className="grid place-items-center"><Loading /></div>;
  const series: ChartSeries[] = metrics.map((m, i) => ({
    key: m.name,
    label: m.label,
    slot: m.slot ?? (metrics.length === 1 ? "primary" : (((i % 4) + 1) as 1 | 2 | 3 | 4)),
    points: q.data!.series[m.name] ?? [],
  }));
  return <LineChart series={series} unit={unit} yMax={yMax} height={height} />;
}

const pct = (v?: number) => (v === undefined ? "–" : `${v.toFixed(0)}%`);

export function Monitoring() {
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") as "overview" | "alerts") || "overview";
  const [range, setRange] = useState<Range>("6h");
  const ov = useOp<any>("monitor.overview", {}, { refetchInterval: 30_000 });

  return (
    <>
      <PageHeader
        eyebrow="Platform"
        title="Monitoring"
        description="Server, database and per-project traffic, sampled every minute and kept for 7 days."
        actions={tab === "overview" ? <RangePicker value={range} onChange={setRange} /> : undefined}
      />
      <Tabs tabs={[{ id: "overview", label: "Overview" }, { id: "alerts", label: "Alerts" }]} value={tab} onChange={(t) => setParams({ tab: t }, { replace: true })} />
      {ov.data?.firing?.length > 0 && (
        <Callout tone="danger" className="mb-5" title={`${ov.data.firing.length} alert${ov.data.firing.length === 1 ? "" : "s"} firing`}>
          <ul className="space-y-0.5">
            {ov.data.firing.map((f: any, i: number) => (
              <li key={i}>
                <strong>{f.rule}</strong> {f.subject !== "host" && f.subject !== "pg" ? `· ${f.subject}` : ""} - {f.metric} {f.value} ({f.op} {f.threshold}) since {ago(f.since)}
              </li>
            ))}
          </ul>
        </Callout>
      )}
      {tab === "overview" ? <Overview ov={ov} range={range} /> : <Alerts />}
    </>
  );
}

function Overview({ ov, range }: { ov: ReturnType<typeof useOp<any>>; range: Range }) {
  if (ov.error) return <ErrorBox error={ov.error} />;
  if (!ov.data) return <Loading />;
  const h = ov.data.host ?? {};
  const pg = ov.data.pg ?? {};
  return (
    <>
      {!ov.data.collecting_since && (
        <Callout tone="info" className="mb-5">The first sample is taken at the start of the next minute.</Callout>
      )}
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="CPU" value={pct(h.cpu_pct)} hint={h.load1 !== undefined ? `load ${h.load1} on ${ov.data.cpus} cores` : ""} icon={<Cpu className="h-4 w-4" />} />
        <Stat label="Memory" value={pct(h.mem_used_pct)} hint={h.mem_total_mb ? `${(h.mem_used_mb / 1000).toFixed(1)} of ${(h.mem_total_mb / 1000).toFixed(1)} GB` : ""} icon={<MemoryStick className="h-4 w-4" />} />
        <Stat label="Disk" value={pct(h.disk_used_pct)} hint={h.disk_free_gb !== undefined ? `${h.disk_free_gb} GB free` : ""} icon={<HardDrive className="h-4 w-4" />} />
        <Stat label="Postgres" value={pg.connections ?? "–"} hint={pg.tps !== undefined ? `connections · ${pg.tps} tx/s · ${pg.cache_hit_pct ?? "–"}% cache hits` : "connections"} icon={<Database className="h-4 w-4" />} />
      </div>

      <div className="mt-5 grid gap-5 lg:grid-cols-2">
        <Card title="CPU usage" description="Percent of all cores">
          <MetricChart scope="host" range={range} metrics={[{ name: "cpu_pct", label: "CPU" }]} unit="%" yMax={100} />
        </Card>
        <Card title="Memory used" description="Excluding reclaimable cache">
          <MetricChart scope="host" range={range} metrics={[{ name: "mem_used_pct", label: "Memory" }]} unit="%" yMax={100} />
        </Card>
        <Card title="Memory by component" description="Resident memory, MB">
          <MetricChart
            scope="host"
            range={range}
            unit=" MB"
            metrics={[
              { name: "rss_postgres_mb", label: "Postgres" },
              { name: "rss_postgrest_mb", label: "PostgREST" },
              { name: "rss_deno_mb", label: "Functions" },
              { name: "rss_ivarabase_mb", label: "IvaraBase" },
            ]}
          />
        </Card>
        <Card title="Postgres" description="Transactions per second">
          <MetricChart scope="pg" range={range} metrics={[{ name: "tps", label: "Transactions/s" }]} />
        </Card>
        <Card title="Disk used" description="Data directory volume">
          <MetricChart scope="host" range={range} metrics={[{ name: "disk_used_pct", label: "Disk" }]} unit="%" yMax={100} />
        </Card>
        <Card title="Postgres connections" description={`Client connections${pg.connections_pct !== undefined ? ` · ${pg.connections_pct}% of max` : ""}`}>
          <MetricChart scope="pg" range={range} metrics={[{ name: "connections", label: "Connections" }]} />
        </Card>
      </div>

      <Card title="Projects" description="Traffic in the last hour (also the table view of the project charts)" className="mt-5" flush>
        <DataTable
          rows={ov.data.projects ?? []}
          rowKey={(p: any) => p.slug}
          empty="No projects yet."
          columns={[
            { key: "slug", label: "Project", render: (p: any) => <Link to={`/p/${p.slug}`} className="font-medium hover:text-primary">{p.name}</Link> },
            { key: "req", label: "Requests", render: (p: any) => <span className="font-mono">{p.requests_1h}</span> },
            {
              key: "err",
              label: "Errors",
              render: (p: any) =>
                p.errors_1h > 0 ? <Badge tone={p.error_rate_pct > 5 ? "danger" : "warning"}>{p.errors_1h} · {p.error_rate_pct}%</Badge> : <span className="text-muted-foreground">0</span>,
            },
            { key: "lat", label: "Avg latency", render: (p: any) => <span className="font-mono">{p.requests_1h ? `${p.avg_ms_1h} ms` : "–"}</span> },
            { key: "db", label: "Database", render: (p: any) => <span className="font-mono">{p.db_size_mb !== undefined ? `${p.db_size_mb} MB` : "–"}</span> },
            { key: "conn", label: "Connections", render: (p: any) => <span className="font-mono">{p.connections ?? "–"}</span> },
          ]}
        />
      </Card>
    </>
  );
}

// ------------------------------------------------------------------ alerts

const METRICS: Record<string, [string, string][]> = {
  host: [
    ["disk_used_pct", "Disk used %"], ["mem_used_pct", "Memory used %"], ["cpu_pct", "CPU %"], ["load_per_cpu", "Load per core"],
    ["disk_free_gb", "Disk free GB"], ["health_errors", "Failing health checks"], ["rss_postgres_mb", "Postgres memory MB"],
  ],
  pg: [["connections_pct", "Connections % of max"], ["connections", "Connections"], ["tps", "Transactions/s"], ["cache_hit_pct", "Cache hit %"], ["db_total_mb", "Total size MB"]],
  project: [["error_rate_pct", "Error rate % (needs 20+ requests)"], ["lat_avg", "Avg latency ms"], ["req_total", "Requests per minute"], ["db_size_mb", "Database size MB"], ["connections", "Connections"]],
};

function Alerts() {
  const qc = useQueryClient();
  const rules = useOp<any[]>("alerts.rules_list", {}, { refetchInterval: 30_000 });
  const events = useOp<any[]>("alerts.events", { limit: 50 }, { refetchInterval: 30_000 });
  const [editing, setEditing] = useState<any | null>(null);
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["op", "alerts.rules_list"] });
    qc.invalidateQueries({ queryKey: ["op", "monitor.overview"] });
  };
  const save = async (r: any, msg = "Saved") => {
    try {
      await op("alerts.rule_save", r);
      toast.success(msg);
      refresh();
    } catch (e) {
      toast.error(e);
    }
  };
  return (
    <div className="space-y-5">
      <Card
        title="Rules"
        description="A rule fires when the metric's average over its window crosses the threshold, and resolves when it no longer does."
        actions={<Button size="sm" variant="primary" icon={<Plus className="h-3.5 w-3.5" />} onClick={() => setEditing({ name: "", scope: "host", metric: "disk_used_pct", op: ">", threshold: 80, for_minutes: 5, enabled: true })}>New rule</Button>}
        flush
      >
        {rules.isLoading ? (
          <Loading />
        ) : (
          <DataTable
            rows={rules.data ?? []}
            rowKey={(r: any) => r.id}
            onRowClick={(r: any) => setEditing(r)}
            columns={[
              { key: "on", label: "On", render: (r: any) => <Switch checked={r.enabled} onChange={(v) => save({ id: r.id, enabled: v }, v ? "Rule enabled" : "Rule disabled")} /> },
              { key: "name", label: "Rule", render: (r: any) => <span className="font-medium">{r.name}</span> },
              { key: "cond", label: "Condition", render: (r: any) => <code className="text-[12px]">{r.scope}.{r.metric} {r.op} {r.threshold} for {r.for_minutes}m</code> },
              {
                key: "state",
                label: "State",
                render: (r: any) =>
                  !r.enabled ? <Badge>off</Badge> : r.firing.length ? <Badge tone="danger"><AlertTriangle className="h-3 w-3" /> firing{r.scope === "project" ? `: ${r.firing.join(", ")}` : ""}</Badge> : <Badge tone="success">ok</Badge>,
              },
            ]}
          />
        )}
      </Card>
      <Channels />
      <Card title="History" flush>
        {events.isLoading ? (
          <Loading />
        ) : !events.data?.length ? (
          <Empty icon={<Bell className="h-5 w-5" />} title="No alerts yet">Fired and resolved alerts appear here.</Empty>
        ) : (
          <DataTable
            rows={events.data}
            rowKey={(e: any) => e.id}
            columns={[
              { key: "at", label: "When", render: (e: any) => <span className="whitespace-nowrap text-muted-foreground" title={date(e.at)}>{ago(e.at)}</span> },
              { key: "state", label: "", render: (e: any) => <Badge tone={e.state === "firing" ? "danger" : "success"}>{e.state}</Badge> },
              { key: "rule", label: "Rule", render: (e: any) => <span className="font-medium">{e.rule}</span> },
              { key: "subj", label: "Subject", render: (e: any) => e.subject },
              { key: "val", label: "Value", render: (e: any) => <span className="font-mono">{Number(e.value).toFixed(2)}</span> },
            ]}
          />
        )}
      </Card>
      {editing && <RuleDialog rule={editing} onClose={() => setEditing(null)} onSaved={refresh} />}
    </div>
  );
}

function RuleDialog({ rule, onClose, onSaved }: { rule: any; onClose: () => void; onSaved: () => void }) {
  const [r, setR] = useState(rule);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const opts: [string, string][] = METRICS[r.scope] ?? [];
  return (
    <Dialog
      open
      onClose={onClose}
      title={rule.id ? "Edit rule" : "New alert rule"}
      footer={
        <>
          {rule.id && !rule.builtin && (
            <Button
              variant="danger-outline"
              icon={<Trash2 className="h-3.5 w-3.5" />}
              onClick={async () => {
                try {
                  await op("alerts.rule_delete", { id: rule.id });
                  toast.success("Rule deleted");
                  onSaved();
                  onClose();
                } catch (e) {
                  toast.error(e);
                }
              }}
            >
              Delete
            </Button>
          )}
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            loading={busy}
            onClick={async () => {
              setBusy(true);
              setError(null);
              try {
                await op("alerts.rule_save", { id: r.id || undefined, name: r.name, scope: r.scope, metric: r.metric, op: r.op, threshold: Number(r.threshold), for_minutes: Number(r.for_minutes), enabled: r.enabled });
                toast.success("Rule saved");
                onSaved();
                onClose();
              } catch (e) {
                setError(e);
              } finally {
                setBusy(false);
              }
            }}
          >
            Save rule
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <Field label="Name"><Input value={r.name} onChange={(e) => setR({ ...r, name: e.target.value })} placeholder="Disk almost full" /></Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Watch">
            <Select value={r.scope} onChange={(e) => setR({ ...r, scope: e.target.value, metric: METRICS[e.target.value][0][0] })}>
              <option value="host">Server</option>
              <option value="pg">Postgres</option>
              <option value="project">Every project</option>
            </Select>
          </Field>
          <Field label="Metric">
            <Select value={r.metric} onChange={(e) => setR({ ...r, metric: e.target.value })}>
              {opts.map(([k, l]) => <option key={k} value={k}>{l}</option>)}
              {!opts.some(([k]) => k === r.metric) && <option value={r.metric}>{r.metric}</option>}
            </Select>
          </Field>
        </div>
        <div className="grid grid-cols-3 gap-4">
          <Field label="Condition">
            <Select value={r.op} onChange={(e) => setR({ ...r, op: e.target.value })}>
              <option value=">">above</option>
              <option value="<">below</option>
            </Select>
          </Field>
          <Field label="Threshold"><Input type="number" step="any" value={r.threshold} onChange={(e) => setR({ ...r, threshold: e.target.value })} /></Field>
          <Field label="For (minutes)"><Input type="number" min={1} max={120} value={r.for_minutes} onChange={(e) => setR({ ...r, for_minutes: e.target.value })} /></Field>
        </div>
        <ErrorBox error={error} />
      </div>
    </Dialog>
  );
}

function Channels() {
  const qc = useQueryClient();
  const ch = useOp<any[]>("alerts.channels_get");
  const [list, setList] = useState<any[] | null>(null);
  const [busy, setBusy] = useState("");
  useEffect(() => {
    if (ch.data && list === null) setList(ch.data);
  }, [ch.data, list]);
  if (ch.error) return <ErrorBox error={ch.error} />;
  if (!list) return <Loading />;
  const set = (i: number, patch: any) => setList(list.map((c, j) => (j === i ? { ...c, ...patch } : c)));
  return (
    <Card title="Notification channels" description="Every enabled channel receives fired and resolved alerts.">
      {list.length === 0 && <p className="mb-3 text-[13px] text-muted-foreground">No channels yet - alerts only show here and on the dashboard.</p>}
      <div className="space-y-3">
        {list.map((c, i) => (
          <div key={c.id ?? i} className="grid gap-2 sm:grid-cols-[auto_150px_130px_1fr_auto] sm:items-center">
            <Switch checked={c.enabled} onChange={(v) => set(i, { enabled: v })} />
            <Input value={c.name} placeholder="Name" onChange={(e) => set(i, { name: e.target.value })} />
            <Select value={c.type} onChange={(e) => set(i, { type: e.target.value })}>
              <option value="discord">Discord</option>
              <option value="slack">Slack</option>
              <option value="webhook">Webhook (JSON)</option>
            </Select>
            <Input value={c.url} placeholder="https://discord.com/api/webhooks/…" onChange={(e) => set(i, { url: e.target.value })} className="font-mono text-[12px]" />
            <Button variant="ghost" size="icon" aria-label="Remove channel" onClick={() => setList(list.filter((_, j) => j !== i))}>
              <Trash2 className="h-3.5 w-3.5" />
            </Button>
          </div>
        ))}
      </div>
      <div className="mt-4 flex flex-wrap gap-2">
        <Button size="sm" icon={<Plus className="h-3.5 w-3.5" />} onClick={() => setList([...list, { name: "", type: "discord", url: "", enabled: true }])}>Add channel</Button>
        <span className="flex-1" />
        <Button
          size="sm"
          icon={<Send className="h-3.5 w-3.5" />}
          loading={busy === "test"}
          onClick={async () => {
            setBusy("test");
            try {
              const r = await op("alerts.test");
              const bad = r.results.filter((x: any) => !x.ok);
              bad.length ? toast.error(new Error(bad.map((x: any) => `${x.channel}: ${x.error ?? x.status}`).join("; "))) : toast.success("Test sent to every channel");
            } catch (e) {
              toast.error(e);
            } finally {
              setBusy("");
            }
          }}
        >
          Send test
        </Button>
        <Button
          size="sm"
          variant="primary"
          loading={busy === "save"}
          onClick={async () => {
            setBusy("save");
            try {
              setList(await op("alerts.channels_set", { channels: list }));
              qc.invalidateQueries({ queryKey: ["op", "alerts.channels_get"] });
              toast.success("Channels saved");
            } catch (e) {
              toast.error(e);
            } finally {
              setBusy("");
            }
          }}
        >
          Save channels
        </Button>
      </div>
    </Card>
  );
}
