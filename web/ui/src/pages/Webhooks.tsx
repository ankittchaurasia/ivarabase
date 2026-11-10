import { useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, RotateCcw, Send, Trash2, Webhook as WebhookIcon, X } from "lucide-react";
import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { Badge, Button, Callout, Card, Checkbox, CopyField, DataTable, Dialog, Dot, Empty, ErrorBox, Field, Input, Loading, PageHeader, Select, Switch, Tabs, toast, useRunner } from "../components/ui";
import { Code } from "../components/Code";
import { op, useOp } from "../lib/api";
import { ago, date, pretty } from "../lib/format";

interface Hook {
  id: number;
  name: string;
  table: string;
  events: string[];
  target: "url" | "function";
  url?: string;
  method?: string;
  headers: Record<string, string>;
  function?: string;
  timeout_ms: number;
  enabled: boolean;
  secret_hint: string;
  secret?: string;
  state: "active" | "disabled" | "table_missing" | "trigger_missing";
  pending: number;
  delivered_24h: number;
  failed_24h: number;
  last_delivery?: { at: string; status: number; ok: boolean; error: string };
}

interface Delivery {
  id: number;
  event_id: number;
  webhook: string;
  attempt: number;
  at: string;
  status_code: number;
  duration_ms: number;
  ok: boolean;
  error: string;
  response: string;
  type: string;
  event_status: string;
  test: boolean;
  next_attempt_at?: string;
  payload?: unknown;
}

const EVENTS = ["INSERT", "UPDATE", "DELETE"] as const;
const STATE: Record<Hook["state"], { tone: "success" | "neutral" | "warning" | "danger"; label: string }> = {
  active: { tone: "success", label: "active" },
  disabled: { tone: "neutral", label: "disabled" },
  table_missing: { tone: "danger", label: "table missing" },
  trigger_missing: { tone: "warning", label: "trigger missing" },
};

export function Webhooks() {
  const { slug = "" } = useParams();
  const qc = useQueryClient();
  const run = useRunner();
  const hooks = useOp<Hook[]>("webhooks.list", { project: slug }, { refetchInterval: 10_000 });
  const [editing, setEditing] = useState<Hook | "new" | null>(null);
  const [secret, setSecret] = useState<{ name: string; secret: string } | null>(null);
  const [filter, setFilter] = useState("");
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["op", "webhooks.list"] });
    qc.invalidateQueries({ queryKey: ["op", "webhooks.deliveries"] });
  };

  async function toggle(h: Hook, enabled: boolean) {
    try {
      await op("webhooks.update", { project: slug, name: h.name, enabled });
      refresh();
    } catch (e) {
      toast.error(e);
    }
  }
  async function test(h: Hook) {
    try {
      const r = await op("webhooks.test", { project: slug, name: h.name });
      r.ok ? toast.success(`${h.name}: HTTP ${r.status_code} in ${r.duration_ms} ms`) : toast.error(new Error(`${h.name}: ${r.error || "HTTP " + r.status_code}`));
      refresh();
    } catch (e) {
      toast.error(e);
    }
  }
  async function remove(h: Hook) {
    try {
      await run("webhooks.delete", { project: slug, name: h.name }, { title: `Delete webhook ${h.name}?`, confirmLabel: "Delete webhook" });
      toast.success(`Deleted ${h.name}`);
      refresh();
    } catch (e) {
      toast.error(e);
    }
  }

  return (
    <>
      <PageHeader
        eyebrow="Compute"
        title="Webhooks"
        description="Call a URL or one of this project's functions when rows are inserted, updated or deleted. Queued in the database, delivered in the background with retries - your writes never wait."
        actions={
          <Button variant="primary" icon={<Plus className="h-4 w-4" />} onClick={() => setEditing("new")}>
            New webhook
          </Button>
        }
      />
      <Card flush className="mb-5">
        {hooks.isLoading ? (
          <Loading />
        ) : hooks.error ? (
          <ErrorBox error={hooks.error} className="m-4" />
        ) : !hooks.data?.length ? (
          <Empty
            icon={<WebhookIcon className="h-5 w-5" />}
            title="No webhooks yet"
            action={<Button icon={<Plus className="h-4 w-4" />} onClick={() => setEditing("new")}>New webhook</Button>}
          >
            Payloads look like Supabase's: <code>{`{"type":"INSERT","table":"orders","schema":"public","record":{…},"old_record":null}`}</code>
          </Empty>
        ) : (
          <DataTable
            rows={hooks.data}
            rowKey={(h) => h.name}
            onRowClick={(h) => setFilter(filter === h.name ? "" : h.name)}
            columns={[
              {
                key: "on",
                label: "",
                render: (h) => <Switch checked={h.enabled} onChange={(v) => toggle(h, v)} />,
              },
              {
                key: "name",
                label: "Webhook",
                render: (h) => (
                  <div className="min-w-0">
                    <div className="font-medium">{h.name}</div>
                    <div className="mt-0.5 flex flex-wrap gap-1">
                      <Badge tone={STATE[h.state].tone}>{STATE[h.state].label}</Badge>
                      {h.events.map((e) => (
                        <Badge key={e}>{e}</Badge>
                      ))}
                    </div>
                  </div>
                ),
              },
              { key: "table", label: "Table", render: (h) => <code className="text-[12px]">{h.table}</code> },
              {
                key: "target",
                label: "Target",
                render: (h) =>
                  h.target === "function" ? (
                    <span className="text-[12.5px]">
                      function <code>{h.function}</code>
                    </span>
                  ) : (
                    <span className="block max-w-[280px] truncate font-mono text-[12px]" title={h.url}>
                      {h.method} {h.url}
                    </span>
                  ),
              },
              {
                key: "last",
                label: "Last delivery",
                render: (h) =>
                  h.last_delivery ? (
                    <span className="flex items-center gap-1.5 text-[12.5px]" title={h.last_delivery.error}>
                      <Dot tone={h.last_delivery.ok ? "success" : "danger"} />
                      {h.last_delivery.status || "–"} · {ago(h.last_delivery.at)}
                    </span>
                  ) : (
                    <span className="text-muted-foreground">never</span>
                  ),
              },
              {
                key: "stats",
                label: "24h",
                render: (h) => (
                  <span className="flex items-center gap-1.5 text-[12.5px]">
                    {h.delivered_24h} ok
                    {h.failed_24h > 0 && <Badge tone="danger">{h.failed_24h} failed</Badge>}
                    {h.pending > 0 && <Badge tone="warning">{h.pending} queued</Badge>}
                  </span>
                ),
              },
              {
                key: "x",
                label: "",
                className: "text-right whitespace-nowrap",
                render: (h) => (
                  <span className="inline-flex gap-1">
                    <Button size="sm" variant="ghost" icon={<Send className="h-3.5 w-3.5" />} onClick={() => test(h)}>
                      Test
                    </Button>
                    <Button size="sm" variant="ghost" icon={<Pencil className="h-3.5 w-3.5" />} onClick={() => setEditing(h)}>
                      Edit
                    </Button>
                    <Button size="sm" variant="ghost" icon={<Trash2 className="h-3.5 w-3.5" />} onClick={() => remove(h)} aria-label="Delete" />
                  </span>
                ),
              },
            ]}
          />
        )}
      </Card>
      <Deliveries slug={slug} filter={filter} onClear={() => setFilter("")} />
      <Callout tone="neutral" className="mt-5" title="Verifying requests">
        Each request has <code>X-Ivarabase-Timestamp</code> and <code>X-Ivarabase-Signature: sha256=…</code>, the hex HMAC-SHA256 of{" "}
        <code>{"<timestamp>.<raw body>"}</code> with the webhook's secret. Delivery is at least once - deduplicate on <code>X-Ivarabase-Event-Id</code>. Failed deliveries are
        retried 5 times over about an hour.
      </Callout>
      {editing && (
        <HookDialog
          slug={slug}
          hook={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={(r, isNew) => {
            setEditing(null);
            refresh();
            if (r.secret) setSecret({ name: r.name, secret: r.secret });
            else toast.success(isNew ? `Created ${r.name}` : `Saved ${r.name}`);
          }}
        />
      )}
      {secret && (
        <Dialog
          open
          onClose={() => setSecret(null)}
          title={`Signing secret for ${secret.name}`}
          description="Copy it now and keep it with the receiver - it is only shown again when you rotate it or ask for it with webhooks.list reveal_secrets."
          footer={<Button variant="primary" onClick={() => setSecret(null)}>Done</Button>}
        >
          <CopyField value={secret.secret} secret />
        </Dialog>
      )}
    </>
  );
}

function Deliveries({ slug, filter, onClear }: { slug: string; filter: string; onClear: () => void }) {
  const qc = useQueryClient();
  const [failedOnly, setFailedOnly] = useState(false);
  const [open, setOpen] = useState<Delivery | null>(null);
  const log = useOp<Delivery[]>(
    "webhooks.deliveries",
    { project: slug, name: filter || undefined, failed_only: failedOnly, include_payload: true, limit: 100 },
    { refetchInterval: 5_000 },
  );
  async function redeliver(d: Delivery) {
    try {
      await op("webhooks.redeliver", { project: slug, event_id: d.event_id });
      toast.success(`Event ${d.event_id} queued again`);
      setOpen(null);
      qc.invalidateQueries({ queryKey: ["op", "webhooks.deliveries"] });
    } catch (e) {
      toast.error(e);
    }
  }
  return (
    <Card
      flush
      title="Recent deliveries"
      description="Every attempt, newest first. Click one for the payload and response."
      actions={
        <>
          {filter && (
            <Button size="sm" variant="ghost" icon={<X className="h-3.5 w-3.5" />} onClick={onClear}>
              {filter}
            </Button>
          )}
          <Checkbox label="Failures only" checked={failedOnly} onChange={(e) => setFailedOnly(e.target.checked)} />
        </>
      }
    >
      {log.isLoading ? (
        <Loading />
      ) : log.error ? (
        <ErrorBox error={log.error} className="m-4" />
      ) : (
        <DataTable
          rows={log.data ?? []}
          rowKey={(d) => d.id}
          maxHeight="480px"
          empty="No deliveries yet. Change a row in a watched table, or press Test."
          onRowClick={setOpen}
          columns={[
            {
              key: "status",
              label: "Status",
              render: (d) => (
                <span className="flex items-center gap-1.5">
                  <Dot tone={d.ok ? "success" : "danger"} />
                  <span className="font-mono text-[12px]">{d.status_code || "ERR"}</span>
                </span>
              ),
            },
            { key: "webhook", label: "Webhook", render: (d) => <span className="font-medium">{d.webhook}</span> },
            {
              key: "event",
              label: "Event",
              render: (d) => (
                <span className="flex items-center gap-1.5">
                  <Badge>{d.type}</Badge>
                  {d.test && <Badge tone="info">test</Badge>}
                  <span className="font-mono text-[11.5px] text-muted-foreground">#{d.event_id}</span>
                </span>
              ),
            },
            { key: "attempt", label: "Attempt", render: (d) => <span className="font-mono text-[12px]">{d.attempt}</span> },
            {
              key: "outcome",
              label: "Outcome",
              render: (d) =>
                d.event_status === "pending" && !d.ok ? (
                  <span className="text-[12px] text-warning">retry {d.next_attempt_at ? ago(d.next_attempt_at) : "soon"}</span>
                ) : (
                  <span className={d.ok ? "text-[12px] text-muted-foreground" : "block max-w-[260px] truncate text-[12px] text-destructive"} title={d.error}>
                    {d.ok ? "delivered" : d.error || "failed"}
                  </span>
                ),
            },
            { key: "ms", label: "Time", render: (d) => <span className="font-mono text-[12px]">{d.duration_ms} ms</span> },
            { key: "at", label: "When", render: (d) => <span className="text-muted-foreground" title={date(d.at)}>{ago(d.at)}</span> },
          ]}
        />
      )}
      {open && (
        <Dialog
          open
          size="lg"
          onClose={() => setOpen(null)}
          title={`${open.webhook} · event #${open.event_id} · attempt ${open.attempt}`}
          description={`${date(open.at)} · ${open.status_code || "no response"} · ${open.duration_ms} ms · event ${open.event_status}`}
          footer={
            <>
              <Button onClick={() => setOpen(null)}>Close</Button>
              <Button variant="primary" icon={<RotateCcw className="h-4 w-4" />} disabled={open.event_status === "pending"} onClick={() => redeliver(open)}>
                Redeliver
              </Button>
            </>
          }
        >
          {open.error && <Callout tone="danger" className="mb-3">{open.error}</Callout>}
          <div className="label mb-1.5">Payload</div>
          <Code lang="json" code={pretty(open.payload)} className="mb-3 max-h-72" />
          <div className="label mb-1.5">Response (first 1000 characters)</div>
          <Code lang={/^\s*[[{]/.test(open.response || "") ? "json" : "text"} code={open.response || "(empty)"} wrap className="max-h-48" />
        </Dialog>
      )}
    </Card>
  );
}

function HookDialog({ slug, hook, onClose, onSaved }: { slug: string; hook: Hook | null; onClose: () => void; onSaved: (r: any, isNew: boolean) => void }) {
  const tables = useOp<{ schema: string; name: string; kind: string }[]>("db.tables", { project: slug });
  const fns = useOp<{ name: string }[]>("functions.list", { project: slug });
  const [form, setForm] = useState({
    name: hook?.name ?? "",
    table: hook?.table ?? "",
    events: hook?.events ?? ["INSERT", "UPDATE", "DELETE"],
    target: hook?.target ?? ("url" as "url" | "function"),
    url: hook?.url ?? "",
    method: hook?.method ?? "POST",
    function: hook?.function ?? "",
    timeout_ms: hook?.timeout_ms ?? 10000,
    enabled: hook?.enabled ?? true,
  });
  const [headers, setHeaders] = useState<[string, string][]>(Object.entries(hook?.headers ?? {}));
  const [rotate, setRotate] = useState(false);
  const [tab, setTab] = useState<"basics" | "advanced">("basics");
  const [busy, setBusy] = useState(false);
  const set = (p: Partial<typeof form>) => setForm((f) => ({ ...f, ...p }));
  const userTables = (tables.data ?? []).filter((t) => t.kind === "table" && !t.schema.startsWith("ivarabase_"));

  useEffect(() => {
    if (!form.table && userTables.length) set({ table: `${userTables[0].schema}.${userTables[0].name}` });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tables.data]);

  async function save(e: React.FormEvent) {
    e.preventDefault();
    if (!form.events.length) return toast.info("Pick at least one event");
    setBusy(true);
    const args: Record<string, unknown> = {
      project: slug,
      name: hook ? hook.name : form.name.trim(),
      table: form.table,
      events: form.events,
      timeout_ms: Number(form.timeout_ms),
      enabled: form.enabled,
      headers: Object.fromEntries(headers.filter(([k]) => k.trim()).map(([k, v]) => [k.trim(), v])),
    };
    if (form.target === "url") Object.assign(args, { url: form.url.trim(), method: form.method });
    else args.function = form.function;
    if (hook) {
      if (form.name.trim() !== hook.name) args.new_name = form.name.trim();
      if (rotate) args.rotate_secret = true;
    }
    try {
      const r = await op(hook ? "webhooks.update" : "webhooks.create", args);
      onSaved(r, !hook);
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open
      size="lg"
      onClose={onClose}
      title={hook ? `Edit ${hook.name}` : "New webhook"}
      description="An AFTER ROW trigger queues each change; IvaraBase delivers it in the background."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="hook-form" loading={busy}>
            {hook ? "Save" : "Create webhook"}
          </Button>
        </>
      }
    >
      <Tabs
        tabs={[
          { id: "basics", label: "Basics" },
          { id: "advanced", label: `Headers & options${headers.length ? ` (${headers.length})` : ""}` },
        ]}
        value={tab}
        onChange={setTab}
      />
      <form id="hook-form" onSubmit={save} className="space-y-4">
        <div className={tab === "basics" ? "space-y-4" : "hidden"}>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Name" hint="letters, digits, - and _">
              <Input value={form.name} onChange={(e) => set({ name: e.target.value })} pattern="[A-Za-z0-9][A-Za-z0-9_\-]{0,62}" placeholder="order-created" required autoCapitalize="off" />
            </Field>
            <Field label="Table" hint={tables.isLoading ? "loading tables…" : userTables.length ? undefined : "create a table first"}>
              <Select value={form.table} onChange={(e) => set({ table: e.target.value })} required>
                {form.table && !userTables.some((t) => `${t.schema}.${t.name}` === form.table) && <option value={form.table}>{form.table} (missing)</option>}
                {userTables.map((t) => (
                  <option key={`${t.schema}.${t.name}`} value={`${t.schema}.${t.name}`}>
                    {t.schema}.{t.name}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
          <Field label="Events">
            <div className="flex flex-wrap gap-4 pt-1">
              {EVENTS.map((ev) => (
                <Checkbox
                  key={ev}
                  label={ev}
                  checked={form.events.includes(ev)}
                  onChange={(e) => set({ events: e.target.checked ? [...form.events, ev] : form.events.filter((x) => x !== ev) })}
                />
              ))}
            </div>
          </Field>
          <Field label="Call">
            <div className="flex gap-4 pt-1">
              <Checkbox label="An HTTP endpoint" checked={form.target === "url"} onChange={() => set({ target: "url" })} />
              <Checkbox label="A function in this project" checked={form.target === "function"} onChange={() => set({ target: "function" })} />
            </div>
          </Field>
          {form.target === "url" ? (
            <div className="grid gap-4 sm:grid-cols-[110px_1fr]">
              <Field label="Method">
                <Select value={form.method} onChange={(e) => set({ method: e.target.value })}>
                  {["POST", "PUT", "PATCH"].map((m) => (
                    <option key={m}>{m}</option>
                  ))}
                </Select>
              </Field>
              <Field label="URL" hint="Private and loopback addresses are refused unless the server allows them.">
                <Input type="url" className="font-mono" value={form.url} onChange={(e) => set({ url: e.target.value })} placeholder="https://example.com/hooks/orders" required />
              </Field>
            </div>
          ) : (
            <Field label="Function" hint="Called with the service_role key, so it can read and write any table.">
              <Select value={form.function} onChange={(e) => set({ function: e.target.value })} required>
                <option value="">{fns.data?.length ? "Pick a function" : "No functions deployed"}</option>
                {fns.data?.map((f) => (
                  <option key={f.name}>{f.name}</option>
                ))}
              </Select>
            </Field>
          )}
          <Switch checked={form.enabled} onChange={(v) => set({ enabled: v })} label="Enabled" />
        </div>
        <div className={tab === "advanced" ? "space-y-4" : "hidden"}>
          <Field label="Headers" hint="Sent with every request, e.g. Authorization for your receiver.">
            <div className="space-y-2">
              {headers.map(([k, v], i) => (
                <div key={i} className="flex gap-2">
                  <Input className="font-mono" placeholder="Authorization" value={k} onChange={(e) => setHeaders(headers.map((h, j) => (j === i ? [e.target.value, h[1]] : h)))} />
                  <Input className="font-mono" placeholder="Bearer …" value={v} onChange={(e) => setHeaders(headers.map((h, j) => (j === i ? [h[0], e.target.value] : h)))} />
                  <Button type="button" variant="ghost" size="icon" aria-label="Remove header" onClick={() => setHeaders(headers.filter((_, j) => j !== i))}>
                    <X className="h-4 w-4" />
                  </Button>
                </div>
              ))}
              <Button type="button" size="sm" icon={<Plus className="h-3.5 w-3.5" />} onClick={() => setHeaders([...headers, ["", ""]])}>
                Add header
              </Button>
            </div>
          </Field>
          <Field label="Timeout (ms)" hint="500 - 60000" className="max-w-[200px]">
            <Input type="number" min={500} max={60000} step={500} value={form.timeout_ms} onChange={(e) => set({ timeout_ms: Number(e.target.value) })} />
          </Field>
          {hook && (
            <Field label="Signing secret" hint={`current: ${hook.secret_hint}`}>
              <Checkbox label="Rotate the secret on save (receivers must be updated)" checked={rotate} onChange={(e) => setRotate(e.target.checked)} />
            </Field>
          )}
        </div>
      </form>
    </Dialog>
  );
}
