import { useQueryClient } from "@tanstack/react-query";
import { GitBranch, Play } from "lucide-react";
import { useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { Editor } from "../components/Editor";
import { Button, Card, Dialog, ErrorBox, Field, Input, Kbd, Loading, PageHeader, toast, useRunner } from "../components/ui";
import { isCancelled, useOp } from "../lib/api";
import { ago, date } from "../lib/format";

const TEMPLATE = `create table orders (
  id bigint generated always as identity primary key,
  item text not null,
  quantity int not null default 1,
  created_at timestamptz not null default now()
);
`;

interface Migration {
  version: string;
  name: string;
  sql: string;
  applied_at: string;
  applied_by: string;
}

export function Migrations() {
  const { slug = "" } = useParams();
  const qc = useQueryClient();
  const run = useRunner();
  const migs = useOp<Migration[]>("db.migrations_list", { project: slug, include_sql: true });
  const schema = useOp<Record<string, { name: string }[]>>("db.schema", { project: slug });
  const [name, setName] = useState("");
  const [sql, setSql] = useState(TEMPLATE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [view, setView] = useState<Migration | null>(null);
  const cmSchema = useMemo(() => (schema.data ? Object.fromEntries(Object.entries(schema.data).map(([t, c]) => [t, c.map((x) => x.name)])) : undefined), [schema.data]);

  async function apply() {
    if (!name.trim()) {
      setError(new Error("Give the migration a short name, e.g. create_orders"));
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const r = await run("db.migrations_apply", { project: slug, name: name.trim(), sql }, { title: "This migration drops or empties data", confirmLabel: "Apply anyway" });
      toast.success(r.status === "applied" ? `Applied ${r.name}` : "Already applied - nothing to do");
      setName("");
      qc.invalidateQueries();
    } catch (e) {
      if (!isCancelled(e)) setError(e);
    } finally {
      setBusy(false);
    }
  }

  const list = (migs.data ?? []).slice().reverse();

  return (
    <>
      <PageHeader eyebrow="Database" title="Migrations" description="Each migration runs in one transaction and is recorded. Applying the same one twice is a no-op, so agents can retry safely." />
      <div className="grid gap-5 xl:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
        <Card title="New migration">
          <Field label="Name">
            <Input placeholder="create_orders" value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Editor lang="sql" value={sql} onChange={setSql} onRun={apply} schema={cmSchema} className="mt-4 h-[42vh] min-h-[240px]" />
          <ErrorBox error={error} className="mt-3" />
          <div className="mt-3 flex items-center gap-2">
            <Button variant="primary" icon={<Play className="h-3.5 w-3.5" />} loading={busy} onClick={apply}>
              Apply migration
            </Button>
            <Kbd>⌘ ↵</Kbd>
          </div>
        </Card>

        <Card title="History" description={`${list.length} applied`} flush>
          {migs.isLoading ? (
            <Loading />
          ) : !list.length ? (
            <div className="px-5 py-10 text-center text-[13px] text-muted-foreground">No migrations yet.</div>
          ) : (
            <ol className="relative px-5 py-4">
              {list.map((m, i) => (
                <li key={m.version} className="relative flex gap-3 pb-5 last:pb-0">
                  {i < list.length - 1 && <span className="absolute left-[11px] top-6 h-full w-px bg-border" />}
                  <span className="relative z-[1] mt-0.5 grid h-[23px] w-[23px] shrink-0 place-items-center rounded-full border bg-card text-primary">
                    <GitBranch className="h-3 w-3" />
                  </span>
                  <button onClick={() => setView(m)} className="min-w-0 flex-1 rounded-md px-2 py-1 text-left hover:bg-accent/50">
                    <div className="truncate font-medium">{m.name}</div>
                    <div className="truncate font-mono text-[11px] text-muted-foreground" title={date(m.applied_at)}>
                      {m.version} · {ago(m.applied_at)} · {m.applied_by}
                    </div>
                  </button>
                </li>
              ))}
            </ol>
          )}
        </Card>
      </div>
      <Dialog open={!!view} onClose={() => setView(null)} title={view?.name ?? ""} description={view ? `${view.version} · applied ${date(view.applied_at)} by ${view.applied_by}` : ""} size="lg">
        {view && <Editor lang="sql" value={view.sql} readOnly className="h-[50vh]" />}
      </Dialog>
    </>
  );
}
