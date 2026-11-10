import { useQueryClient } from "@tanstack/react-query";
import { Clock, FilePlus2, FileCode2, Globe, Lock, Pencil, Plus, Rocket, Send, Star, Terminal, Trash2, Zap } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Editor, type Lang } from "../components/Editor";
import { Badge, Button, Card, cn, CopyField, DataTable, Dot, Empty, ErrorBox, Field, Input, Kbd, Loading, PageHeader, Select, Switch, Tabs, toast, useRunner } from "../components/ui";
import { Code } from "../components/Code";
import { ApiError, op, useOp } from "../lib/api";
import { ago, date, pretty } from "../lib/format";

interface Fn {
  name: string;
  verify_jwt: boolean;
  schedule: string;
  next_run_at?: string;
  last_run_at?: string | null;
  files: number;
  timeout_s: number;
  memory_mb: number;
  version: number;
  updated_at: string;
  calls_24h: number;
  errors_24h: number;
  avg_ms_24h: number;
  warm: boolean;
  url: string;
}

const CRON_PRESETS: [string, string][] = [
  ["", "No schedule"],
  ["* * * * *", "Every minute"],
  ["*/5 * * * *", "Every 5 minutes"],
  ["*/15 * * * *", "Every 15 minutes"],
  ["0 * * * *", "Every hour"],
  ["0 3 * * *", "Daily at 03:00 UTC"],
  ["0 9 * * 1", "Mondays at 09:00 UTC"],
];

export function Functions() {
  const { slug = "" } = useParams();
  const nav = useNavigate();
  const fns = useOp<Fn[]>("functions.list", { project: slug }, { refetchInterval: 15_000 });
  const [name, setName] = useState("");

  return (
    <>
      <PageHeader
        eyebrow="Compute"
        title="Functions"
        description={<>TypeScript on Deno, kept warm. Call them with <code>supabase.functions.invoke()</code>, from webhooks, or on a schedule.</>}
      />
      <Card className="mb-5">
        <form
          className="flex flex-col gap-3 sm:flex-row sm:items-end"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim()) nav(`/p/${slug}/functions/${encodeURIComponent(name.trim())}`);
          }}
        >
          <Field label="New function" hint="a-z, 0-9, - and _ · or deploy a folder: ivarabase functions deploy --project … --dir ./my-fn" className="flex-1">
            <Input placeholder="send-welcome-email" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} pattern="[a-z][a-z0-9_-]{1,40}" autoCapitalize="off" />
          </Field>
          <Button variant="primary" type="submit" icon={<Plus className="h-4 w-4" />} className="sm:mb-[22px]">
            Create function
          </Button>
        </form>
      </Card>
      {fns.isLoading ? (
        <Loading />
      ) : fns.error ? (
        <ErrorBox error={fns.error} />
      ) : !fns.data?.length ? (
        <Card>
          <Empty icon={<Zap className="h-5 w-5" />} title="No functions yet">
            Create one above - you get a starter that reads a JSON body and returns JSON.
          </Empty>
        </Card>
      ) : (
        <Card flush>
          <DataTable
            rows={fns.data}
            rowKey={(f) => f.name}
            onRowClick={(f) => nav(`/p/${slug}/functions/${f.name}`)}
            columns={[
              {
                key: "name",
                label: "Function",
                render: (f) => (
                  <Link to={`/p/${slug}/functions/${f.name}`} className="flex items-center gap-2 font-medium hover:text-primary">
                    <Dot tone={f.warm ? "success" : "neutral"} /> {f.name}
                    {f.files > 1 && <span className="font-mono text-[11px] text-muted-foreground">{f.files} files</span>}
                  </Link>
                ),
              },
              {
                key: "auth",
                label: "Callers",
                render: (f) => (f.verify_jwt ? <Badge tone="success"><Lock className="h-3 w-3" /> JWT</Badge> : <Badge tone="warning"><Globe className="h-3 w-3" /> anyone</Badge>),
              },
              {
                key: "sched",
                label: "Schedule",
                render: (f) =>
                  f.schedule ? (
                    <span className="flex items-center gap-1.5 text-[12px]" title={f.next_run_at ? `next ${date(f.next_run_at)}` : ""}>
                      <Clock className="h-3.5 w-3.5 text-primary" />
                      <code>{f.schedule}</code>
                    </span>
                  ) : (
                    <span className="text-muted-foreground">–</span>
                  ),
              },
              { key: "calls", label: "Calls 24h", render: (f) => <span className="flex items-center gap-2">{f.calls_24h}{f.errors_24h > 0 && <Badge tone="danger">{f.errors_24h} err</Badge>}</span> },
              { key: "avg", label: "Avg", render: (f) => <span className="font-mono text-[12px]">{f.avg_ms_24h} ms</span> },
              { key: "updated", label: "Deployed", render: (f) => <span className="text-muted-foreground">v{f.version} · {ago(f.updated_at)}</span> },
            ]}
          />
        </Card>
      )}
    </>
  );
}

const langOf = (p: string): Lang => (p.endsWith(".json") || p.endsWith(".jsonc") ? "json" : "ts");

export function FunctionEditor() {
  const { slug = "", name = "" } = useParams();
  const qc = useQueryClient();
  const nav = useNavigate();
  const run = useRunner();
  const fn = useOp<any>("functions.get", { project: slug, name }, { retry: false });
  const isNew = fn.error instanceof ApiError && fn.error.code === "function_not_found";
  const tpl = useOp<{ code: string }>("functions.template", {}, { enabled: isNew });

  const [files, setFiles] = useState<Record<string, string> | null>(null);
  const [entry, setEntry] = useState("index.ts");
  const [active, setActive] = useState("index.ts");
  const [settings, setSettings] = useState({ verify_jwt: true, schedule: "", timeout_s: 30, memory_mb: 128 });
  const [dirty, setDirty] = useState(false);
  const [deploying, setDeploying] = useState(false);
  const [tab, setTab] = useState<"test" | "console" | "logs">("test");
  const loadedVersion = useRef<number | null>(null);

  useEffect(() => {
    if (fn.data && loadedVersion.current !== fn.data.version) {
      loadedVersion.current = fn.data.version;
      setFiles(fn.data.files);
      setEntry(fn.data.entrypoint);
      setActive((a) => (fn.data.files[a] !== undefined ? a : fn.data.entrypoint));
      setSettings({ verify_jwt: fn.data.verify_jwt, schedule: fn.data.schedule, timeout_s: fn.data.timeout_s, memory_mb: fn.data.memory_mb });
      setDirty(false);
    } else if (isNew && tpl.data && files === null) {
      setFiles({ "index.ts": tpl.data.code });
      setDirty(true);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fn.data, isNew, tpl.data]);

  const deploy = useCallback(async () => {
    if (!files) return;
    setDeploying(true);
    try {
      const r = await op("functions.deploy", { project: slug, name, files, entrypoint: entry, ...settings });
      toast.success(`Deployed v${r.version}${r.next_run_at ? ` · next run ${date(r.next_run_at)}` : ""}`);
      setDirty(false);
      qc.invalidateQueries({ queryKey: ["op", "functions.get"] });
      qc.invalidateQueries({ queryKey: ["op", "functions.list"] });
    } catch (e) {
      toast.error(e);
    } finally {
      setDeploying(false);
    }
  }, [files, entry, settings, slug, name, qc]);

  if (fn.error && !isNew) return <ErrorBox error={fn.error} />;
  if (!files) return <Loading />;

  const paths = Object.keys(files).sort((a, b) => (a === entry ? -1 : b === entry ? 1 : a.localeCompare(b)));
  const change = (patch: Record<string, string | null>) => {
    const next = { ...files };
    for (const [k, v] of Object.entries(patch)) v === null ? delete next[k] : (next[k] = v);
    setFiles(next);
    setDirty(true);
  };
  const addFile = () => {
    const p = prompt("New file path", "lib/utils.ts")?.trim().replace(/^\.?\//, "");
    if (!p) return;
    if (files[p] !== undefined) return toast.info("That file already exists");
    change({ [p]: p.endsWith(".json") ? "{}\n" : "export {};\n" });
    setActive(p);
  };
  const renameFile = (p: string) => {
    const n = prompt("Rename to", p)?.trim().replace(/^\.?\//, "");
    if (!n || n === p) return;
    change({ [n]: files[p], [p]: null });
    if (entry === p) setEntry(n);
    setActive(n);
  };
  const deleteFile = (p: string) => {
    if (p === entry) return toast.info("Pick another entrypoint before deleting this file");
    if (!confirm(`Delete ${p}?`)) return;
    change({ [p]: null });
    setActive(entry);
  };
  const url = fn.data?.url ?? `${location.origin}/proj/${slug}/functions/v1/${name}`;
  const nextRun = fn.data?.next_run_at;

  return (
    <>
      <PageHeader
        eyebrow="Function"
        title={<span className="font-mono text-[26px]">{name}</span>}
        description={
          isNew ? "Not deployed yet." : (
            <>
              v{fn.data.version} · deployed {ago(fn.data.updated_at)}
              {dirty && <Badge tone="warning" className="ml-2">unsaved changes</Badge>}
            </>
          )
        }
        actions={
          <>
            {!isNew && (
              <Button
                variant="danger-outline"
                icon={<Trash2 className="h-3.5 w-3.5" />}
                onClick={async () => {
                  try {
                    await run("functions.delete", { project: slug, name }, { title: `Delete ${name}?`, confirmLabel: "Delete function" });
                    toast.success("Deleted");
                    qc.invalidateQueries();
                    nav(`/p/${slug}/functions`);
                  } catch (e) {
                    toast.error(e);
                  }
                }}
              >
                Delete
              </Button>
            )}
            <Button variant="primary" icon={<Rocket className="h-3.5 w-3.5" />} loading={deploying} onClick={deploy}>
              {isNew ? "Deploy" : "Deploy changes"}
            </Button>
          </>
        }
      />

      <Card className="mb-5" flush>
        <div className="flex flex-wrap items-end gap-x-6 gap-y-3 px-5 py-4">
          <div className="flex flex-col gap-1.5">
            <span className="text-[12.5px] font-medium text-foreground/80">Callers</span>
            <Switch
              checked={settings.verify_jwt}
              onChange={(v) => { setSettings({ ...settings, verify_jwt: v }); setDirty(true); }}
              label={<span className="text-[12.5px]">{settings.verify_jwt ? "Require JWT (anon key or user)" : "Anyone - for webhooks"}</span>}
            />
          </div>
          <Field label="Schedule (cron, UTC)" className="min-w-[230px]">
            <div className="flex gap-2">
              <Input className="w-[150px] font-mono text-[12.5px]" placeholder="none" value={settings.schedule} onChange={(e) => { setSettings({ ...settings, schedule: e.target.value }); setDirty(true); }} />
              <Select className="w-[150px] text-[12.5px]" value="x" onChange={(e) => { if (e.target.value !== "x") { setSettings({ ...settings, schedule: e.target.value }); setDirty(true); } }}>
                <option value="x">Presets…</option>
                {CRON_PRESETS.map(([v, l]) => <option key={l} value={v}>{l}</option>)}
              </Select>
            </div>
          </Field>
          <Field label="Timeout (s)">
            <Input type="number" min={5} max={300} className="w-20" value={settings.timeout_s} onChange={(e) => { setSettings({ ...settings, timeout_s: +e.target.value }); setDirty(true); }} />
          </Field>
          <Field label="Memory (MB)">
            <Input type="number" min={64} max={1024} step={64} className="w-24" value={settings.memory_mb} onChange={(e) => { setSettings({ ...settings, memory_mb: +e.target.value }); setDirty(true); }} />
          </Field>
          {nextRun && settings.schedule && (
            <span className="mb-2 flex items-center gap-1.5 text-[12px] text-muted-foreground">
              <Clock className="h-3.5 w-3.5" /> next run {date(nextRun)}{fn.data?.last_run_at ? ` · last ${ago(fn.data.last_run_at)}` : ""}
            </span>
          )}
        </div>
      </Card>

      <div className="grid gap-5 xl:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
        <Card flush>
          <div className="grid md:grid-cols-[190px_minmax(0,1fr)]">
            <div className="border-b md:border-b-0 md:border-r">
              <div className="flex items-center justify-between px-3 py-2.5">
                <span className="label">Files</span>
                <Button size="sm" variant="ghost" className="h-6 px-1.5" onClick={addFile} aria-label="Add file" title="Add file">
                  <FilePlus2 className="h-3.5 w-3.5" />
                </Button>
              </div>
              <ul className="scroll-thin max-h-[180px] overflow-y-auto px-1.5 pb-2 md:max-h-[56vh]">
                {paths.map((p) => (
                  <li key={p} className="group">
                    <button
                      onClick={() => setActive(p)}
                      className={cn("flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left font-mono text-[12px]", active === p ? "bg-primary/10 font-semibold text-primary" : "text-foreground/80 hover:bg-accent/60")}
                    >
                      <FileCode2 className="h-3.5 w-3.5 shrink-0 opacity-70" />
                      <span className="min-w-0 flex-1 truncate">{p}</span>
                      {p === entry && <Star className="h-3 w-3 shrink-0 fill-warning text-warning" aria-label="entrypoint" />}
                    </button>
                  </li>
                ))}
              </ul>
            </div>
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2">
                <span className="truncate font-mono text-[12px] text-muted-foreground">{active}</span>
                <span className="flex-1" />
                {active !== entry && (
                  <Button size="sm" variant="ghost" icon={<Star className="h-3 w-3" />} onClick={() => { setEntry(active); setDirty(true); }}>
                    Make entrypoint
                  </Button>
                )}
                <Button size="sm" variant="ghost" icon={<Pencil className="h-3 w-3" />} onClick={() => renameFile(active)}>Rename</Button>
                {active !== entry && <Button size="sm" variant="ghost" icon={<Trash2 className="h-3 w-3" />} onClick={() => deleteFile(active)}>Delete</Button>}
                <Kbd>⌘ S</Kbd>
              </div>
              <Editor key={active} lang={langOf(active)} value={files[active] ?? ""} onChange={(v) => change({ [active]: v })} onSave={deploy} className="h-[56vh] min-h-[320px] rounded-none border-0" />
            </div>
          </div>
          <div className="space-y-2 border-t px-4 py-3">
            <CopyField value={url} />
            <Code lang="ts" code={`const { data, error } = await supabase.functions.invoke("${name}", { body: { name: "you" } })`} />
            <p className="text-[12px] text-muted-foreground">
              Env: <code>SUPABASE_URL</code>, <code>SUPABASE_ANON_KEY</code>, <code>SUPABASE_SERVICE_ROLE_KEY</code>, <code>SUPABASE_DB_URL</code> + this project's secrets. The caller's{" "}
              <code>Authorization</code> header is passed through, so <code>createClient(..., {"{ global: { headers: { Authorization } } }"})</code> acts as that user.
            </p>
          </div>
        </Card>

        <Card flush>
          <div className="px-4 pt-2">
            <Tabs
              tabs={[
                { id: "test", label: "Test" },
                { id: "console", label: "Console" },
                { id: "logs", label: "Invocations" },
              ]}
              value={tab}
              onChange={setTab}
            />
          </div>
          <div className="px-4 pb-4">
            {tab === "test" && <Tester slug={slug} name={name} disabled={isNew} />}
            {tab === "console" && <Console slug={slug} name={name} />}
            {tab === "logs" && <Logs slug={slug} name={name} />}
          </div>
        </Card>
      </div>
    </>
  );
}

function Tester({ slug, name, disabled }: { slug: string; name: string; disabled: boolean }) {
  const qc = useQueryClient();
  const [method, setMethod] = useState("POST");
  const [path, setPath] = useState("/");
  const [body, setBody] = useState('{\n  "name": "you"\n}');
  const [res, setRes] = useState<any>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  async function send() {
    setBusy(true);
    setError(null);
    let b: unknown;
    if (method !== "GET" && body.trim()) {
      try {
        b = JSON.parse(body);
      } catch {
        b = body;
      }
    }
    try {
      setRes(await op("functions.invoke", { project: slug, name, method, path, body: b }));
      qc.invalidateQueries({ queryKey: ["op", "functions.logs"] });
      qc.invalidateQueries({ queryKey: ["op", "functions.console"] });
    } catch (e) {
      setError(e);
      setRes(null);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex gap-2">
        <Select className="w-[96px] font-mono text-[12.5px]" value={method} onChange={(e) => setMethod(e.target.value)}>
          {["GET", "POST", "PUT", "PATCH", "DELETE"].map((m) => <option key={m}>{m}</option>)}
        </Select>
        <Input className="font-mono text-[12.5px]" value={path} onChange={(e) => setPath(e.target.value)} />
        <Button variant="primary" icon={<Send className="h-3.5 w-3.5" />} loading={busy} onClick={send} disabled={disabled}>
          Send
        </Button>
      </div>
      {method !== "GET" && <Editor lang="json" value={body} onChange={setBody} className="h-[130px]" />}
      {disabled && <p className="text-[12.5px] text-muted-foreground">Deploy the function first.</p>}
      <ErrorBox error={error} />
      {res && (
        <div className="overflow-hidden rounded-md border">
          <div className="flex items-center gap-2 border-b bg-muted/50 px-3 py-1.5 text-[12px]">
            <Badge tone={res.status < 400 ? "success" : "danger"}>{res.status}</Badge>
            <span className="text-muted-foreground">{res.ms} ms</span>
            <span className="truncate font-mono text-[11px] text-muted-foreground">{res.headers?.["Content-Type"]}</span>
          </div>
          <Code lang={typeof res.body === "string" ? "text" : "json"} code={typeof res.body === "string" ? res.body : pretty(res.body)} className="max-h-[40vh] rounded-none border-0 bg-card" />
        </div>
      )}
    </div>
  );
}

function Console({ slug, name }: { slug: string; name: string }) {
  const lines = useOp<{ id: number; at: string; stream: string; line: string }[]>("functions.console", { project: slug, name, limit: 300 }, { refetchInterval: 3_000 });
  if (lines.isLoading) return <Loading />;
  if (lines.error) return <ErrorBox error={lines.error} />;
  if (!lines.data?.length)
    return (
      <div className="py-10 text-center text-[13px] text-muted-foreground">
        <Terminal className="mx-auto mb-2 h-5 w-5" />
        console.log output appears here.
      </div>
    );
  return (
    <div className="scroll-thin max-h-[60vh] overflow-auto rounded-md border bg-[hsl(30_8%_9%)] p-3 font-mono text-[11.5px] leading-relaxed text-[hsl(40_20%_85%)]">
      {lines.data.slice().reverse().map((l) => (
        <div key={l.id} className={cn("flex gap-3 whitespace-pre-wrap break-all", l.stream === "stderr" && "text-[hsl(0_75%_72%)]")}>
          <span className="shrink-0 text-[hsl(35_8%_50%)]">{new Date(l.at).toLocaleTimeString()}</span>
          <span>{l.line}</span>
        </div>
      ))}
    </div>
  );
}

function Logs({ slug, name }: { slug: string; name: string }) {
  const logs = useOp<any[]>("functions.logs", { project: slug, name, limit: 50 }, { refetchInterval: 10_000 });
  if (logs.isLoading) return <Loading />;
  if (logs.error) return <ErrorBox error={logs.error} />;
  if (!logs.data?.length) return <p className="py-8 text-center text-[13px] text-muted-foreground">No invocations yet.</p>;
  return (
    <ul className="scroll-thin max-h-[60vh] divide-y overflow-auto">
      {logs.data.map((l) => (
        <li key={l.id} className="py-2.5">
          <div className="flex items-center gap-2 text-[12.5px]">
            <Badge tone={l.status < 400 ? "success" : "danger"}>{l.status}</Badge>
            <span className="font-mono">{l.ms} ms</span>
            <span className="ml-auto text-muted-foreground">{ago(l.at)}</span>
          </div>
          {l.error && <pre className="mt-1.5 max-h-40 overflow-auto rounded bg-destructive/[0.07] p-2 text-[11.5px] text-destructive">{l.error}</pre>}
        </li>
      ))}
    </ul>
  );
}
