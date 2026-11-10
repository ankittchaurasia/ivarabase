import { useQueryClient } from "@tanstack/react-query";
import { Blocks, Gauge, RotateCcw } from "lucide-react";
import { useState } from "react";
import { useParams } from "react-router-dom";
import { Badge, Button, Callout, Card, DataTable, Empty, ErrorBox, Loading, PageHeader, Select, Switch, Tabs, toast, useRunner } from "../components/ui";
import { ApiError, isCancelled, op, useOp } from "../lib/api";

interface Ext {
  name: string;
  description: string;
  available: boolean;
  enabled: boolean;
  version?: string;
  installed_version?: string;
  schema?: string;
  install_hint?: string;
}

interface SlowQuery {
  query: string;
  calls: number;
  total_ms: number;
  mean_ms: number;
  max_ms: number;
  rows: number;
  role: string;
  cache_hit_ratio: number;
}

const ms = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(2)} s` : `${v.toFixed(v < 10 ? 2 : 0)} ms`);

export function Extensions() {
  const { slug = "" } = useParams();
  const [tab, setTab] = useState<"extensions" | "queries">("extensions");
  return (
    <>
      <PageHeader eyebrow="Database" title="Extensions" description="Turn on Postgres extensions such as pgvector, and find the queries that slow your project down." />
      <div className="mb-4">
        <Tabs
          tabs={[
            { id: "extensions", label: "Extensions" },
            { id: "queries", label: "Slow queries" },
          ]}
          value={tab}
          onChange={setTab}
        />
      </div>
      {tab === "extensions" ? <ExtensionList slug={slug} /> : <SlowQueries slug={slug} />}
    </>
  );
}

function ExtensionList({ slug }: { slug: string }) {
  const qc = useQueryClient();
  const run = useRunner();
  const exts = useOp<Ext[]>("db.extensions_list", { project: slug });
  const [busy, setBusy] = useState("");

  async function toggle(e: Ext, on: boolean) {
    setBusy(e.name);
    try {
      if (on) {
        await op("db.extension_enable", { project: slug, name: e.name });
        toast.success(`${e.name} is on`);
      } else {
        await run("db.extension_disable", { project: slug, name: e.name }, { title: `Turn off ${e.name}?`, confirmLabel: "Turn off" });
        toast.success(`${e.name} is off`);
      }
      qc.invalidateQueries({ queryKey: ["op", "db.extensions_list"] });
    } catch (err) {
      if (!isCancelled(err)) toast.error(err);
    } finally {
      setBusy("");
    }
  }

  if (exts.isLoading) return <Loading />;
  if (exts.error) return <ErrorBox error={exts.error} />;
  return (
    <>
      <Callout className="mb-4" title="Installed into the extensions schema">
        Like Supabase, extensions go into the <code>extensions</code> schema, which is on the search path for SQL and the REST API. Existing
        migrations with <code>create extension vector with schema extensions</code> work as they are.
      </Callout>
      <Card flush>
        <DataTable
          rows={exts.data ?? []}
          rowKey={(e) => e.name}
          columns={[
            {
              key: "name",
              label: "Extension",
              render: (e) => (
                <div className="min-w-0">
                  <code className="font-semibold">{e.name}</code>
                  <div className="text-[12.5px] text-muted-foreground">{e.description}</div>
                  {!e.available && e.install_hint && <div className="mt-0.5 text-[12px] text-warning">Not installed on this server: {e.install_hint}</div>}
                </div>
              ),
            },
            {
              key: "version",
              label: "Version",
              render: (e) => <span className="font-mono text-[12.5px] text-muted-foreground">{e.installed_version || e.version || "—"}</span>,
            },
            {
              key: "state",
              label: "",
              className: "text-right",
              render: (e) =>
                e.available ? (
                  <div className="flex items-center justify-end gap-2">
                    {e.enabled && e.schema && <Badge>{e.schema}</Badge>}
                    <Switch checked={e.enabled} onChange={(v) => busy !== e.name && toggle(e, v)} label={<span className="sr-only">Toggle {e.name}</span>} />
                  </div>
                ) : (
                  <Badge tone="warning">missing</Badge>
                ),
            },
          ]}
        />
      </Card>
    </>
  );
}

function SlowQueries({ slug }: { slug: string }) {
  const [order, setOrder] = useState("total");
  const run = useRunner();
  const q = useOp<SlowQuery[]>("db.slow_queries", { project: slug, order_by: order, limit: 50 });
  const unavailable = q.error instanceof ApiError && q.error.code === "query_stats_unavailable";

  return (
    <Card
      flush
      title="Slow queries"
      actions={
        <div className="flex items-center gap-2">
          <Select value={order} onChange={(e) => setOrder(e.target.value)} aria-label="Order by">
            <option value="total">Total time</option>
            <option value="mean">Average time</option>
            <option value="calls">Most calls</option>
          </Select>
          <Button
            size="sm"
            variant="ghost"
            icon={<RotateCcw className="h-3.5 w-3.5" />}
            onClick={async () => {
              try {
                await run("db.slow_queries_reset", { project: slug });
                q.refetch();
              } catch (e) {
                if (!isCancelled(e)) toast.error(e);
              }
            }}
          >
            Reset
          </Button>
        </div>
      }
    >
      {q.isLoading ? (
        <Loading />
      ) : unavailable ? (
        <Empty icon={<Gauge className="h-5 w-5" />} title="Query statistics are off">
          {(q.error as ApiError).hint}
        </Empty>
      ) : q.error ? (
        <ErrorBox error={q.error} className="m-4" />
      ) : !q.data?.length ? (
        <Empty icon={<Blocks className="h-5 w-5" />} title="No queries recorded yet">
          Statistics appear as your app and the REST API run queries.
        </Empty>
      ) : (
        <DataTable
          rows={q.data}
          rowKey={(r) => r.query + r.role}
          columns={[
            {
              key: "q",
              label: "Query",
              render: (r) => (
                <code className="line-clamp-3 block max-w-[560px] whitespace-pre-wrap break-words text-[12px]" title={r.query}>
                  {r.query}
                </code>
              ),
            },
            { key: "calls", label: "Calls", className: "text-right tabular-nums", render: (r) => r.calls.toLocaleString() },
            { key: "mean", label: "Avg", className: "text-right tabular-nums", render: (r) => ms(r.mean_ms) },
            { key: "max", label: "Max", className: "text-right tabular-nums", render: (r) => ms(r.max_ms) },
            { key: "total", label: "Total", className: "text-right tabular-nums", render: (r) => ms(r.total_ms) },
            {
              key: "cache",
              label: "Cache hit",
              className: "text-right tabular-nums",
              render: (r) => <span className={r.cache_hit_ratio < 0.9 ? "text-warning" : ""}>{(r.cache_hit_ratio * 100).toFixed(0)}%</span>,
            },
          ]}
        />
      )}
    </Card>
  );
}
