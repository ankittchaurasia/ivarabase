import { useQueryClient } from "@tanstack/react-query";
import { Radio } from "lucide-react";
import { useState } from "react";
import { Badge, Button, Card, Dot, Loading, toast } from "../components/ui";
import { op, useOp } from "../lib/api";

interface RealtimeStatus {
  connections: number;
  max_connections: number;
  channels: { topic: string; clients: number; presence: number; postgres_changes: number }[];
  postgres_changes: { listening: boolean; changes?: number; delivered?: number; last_error?: string };
  tables: { schema: string; table: string; rls_enabled: boolean; replica_identity_full: boolean; watched: boolean }[];
}

/** Realtime: live connections and the tables in the supabase_realtime publication. */
export function RealtimeCard({ slug, tables }: { slug: string; tables: { name: string; kind: string }[] }) {
  const qc = useQueryClient();
  const st = useOp<RealtimeStatus>("realtime.status", { project: slug }, { refetchInterval: 10_000 });
  const [busy, setBusy] = useState("");
  const d = st.data;
  const published = new Set((d?.tables ?? []).filter((t) => t.schema === "public").map((t) => t.table));
  const candidates = tables.filter((t) => t.kind === "table" && !published.has(t.name));

  async function setTable(table: string, enabled: boolean) {
    setBusy(table);
    try {
      await op("realtime.set_table", { project: slug, table, enabled });
      toast.success(`${table}: postgres_changes ${enabled ? "on" : "off"}`);
      qc.invalidateQueries({ queryKey: ["op", "realtime.status"] });
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy("");
    }
  }

  return (
    <Card
      title={<span className="flex items-center gap-2"><Radio className="h-4 w-4 text-primary" /> Realtime</span>}
      description="supabase.channel(...) for broadcast, presence and postgres_changes. Tables send changes only while they are in the supabase_realtime publication; row level security applies to every subscriber."
      className="mt-5"
      flush
    >
      {!d ? (
        <Loading />
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-x-6 gap-y-2 px-5 pt-4 text-[13px]">
            <span className="flex items-center gap-2">
              <Dot tone={d.connections > 0 ? "success" : "neutral"} />
              {d.connections} / {d.max_connections} connections
            </span>
            <span>{d.channels.length} channel{d.channels.length === 1 ? "" : "s"}</span>
            <span className="flex items-center gap-2">
              <Dot tone={d.postgres_changes.listening ? (d.postgres_changes.last_error ? "warning" : "success") : "neutral"} />
              {d.postgres_changes.listening ? `change feed on (${d.postgres_changes.delivered ?? 0} delivered)` : "change feed idle"}
            </span>
          </div>
          {d.channels.length > 0 && (
            <div className="flex flex-wrap gap-2 px-5 pt-3">
              {d.channels.map((c) => (
                <Badge key={c.topic} title={`${c.clients} clients, ${c.presence} present, ${c.postgres_changes} postgres_changes bindings`}>
                  {c.topic} · {c.clients}
                </Badge>
              ))}
            </div>
          )}
          <ul className="mt-3 divide-y border-t">
            {d.tables.map((t) => (
              <li key={t.schema + "." + t.table} className="flex items-center gap-3 px-5 py-2.5">
                <span className="flex-1 font-mono text-[13px]">{t.schema === "public" ? t.table : `${t.schema}.${t.table}`}</span>
                {t.rls_enabled ? <Badge tone="success">RLS</Badge> : <Badge tone="warning">no RLS</Badge>}
                {t.replica_identity_full && <Badge>old rows</Badge>}
                <Button size="sm" loading={busy === t.table} onClick={() => setTable(t.table, false)}>Remove</Button>
              </li>
            ))}
            {d.tables.length === 0 && <li className="px-5 py-4 text-[13px] text-muted-foreground">No tables in the publication yet.</li>}
          </ul>
          {candidates.length > 0 && (
            <div className="flex flex-wrap items-center gap-2 border-t px-5 py-3 text-[12.5px]">
              <span className="text-muted-foreground">Add:</span>
              {candidates.map((t) => (
                <Button key={t.name} size="sm" variant="ghost" loading={busy === t.name} onClick={() => setTable(t.name, true)}>
                  {t.name}
                </Button>
              ))}
            </div>
          )}
          <div className="border-t px-5 py-3 text-[12.5px] text-muted-foreground">
            Or in SQL: <code>alter publication supabase_realtime add table todos;</code>
          </div>
        </>
      )}
    </Card>
  );
}
