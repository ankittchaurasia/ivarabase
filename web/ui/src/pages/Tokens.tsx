import { useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus } from "lucide-react";
import { useState } from "react";
import { Badge, Button, Callout, Card, Checkbox, CopyField, DataTable, Dialog, Empty, ErrorBox, Field, Input, Loading, PageHeader, Select, toast } from "../components/ui";
import { Code } from "../components/Code";
import { op, useOp, type Project } from "../lib/api";
import { ago, pretty } from "../lib/format";

const LEVELS = {
  read: "Inspect only: health, schema, rows, logs.",
  write: "Change data, run migrations, deploy functions, take backups.",
  admin: "Everything, including tokens and backup settings.",
};

export function Tokens() {
  const qc = useQueryClient();
  const tokens = useOp<any[]>("tokens.list");
  const projects = useOp<Project[]>("projects.list");
  const [form, setForm] = useState({ name: "", level: "write", projects: [] as string[], allow_destructive: false, expires_days: 0 });
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<any>(null);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      setCreated(await op("tokens.create", form));
      setForm({ ...form, name: "" });
      qc.invalidateQueries({ queryKey: ["op", "tokens.list"] });
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  }

  const toggleProject = (s: string) => setForm((f) => ({ ...f, projects: f.projects.includes(s) ? f.projects.filter((x) => x !== s) : [...f.projects, s] }));

  return (
    <>
      <PageHeader eyebrow="Access" title="Access tokens" description="For AI agents, scripts and CI. Each token has a level, an optional project scope and an optional expiry." />
      <Card title="New token" className="mb-5">
        <form onSubmit={create} className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-[1.3fr_1fr_0.7fr]">
            <Field label="Name">
              <Input placeholder="claude-code" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} required />
            </Field>
            <Field label="Level" hint={LEVELS[form.level as keyof typeof LEVELS]}>
              <Select value={form.level} onChange={(e) => setForm({ ...form, level: e.target.value })}>
                <option value="read">read</option>
                <option value="write">write</option>
                <option value="admin">admin</option>
              </Select>
            </Field>
            <Field label="Expires in days" hint="0 = never">
              <Input type="number" min={0} value={form.expires_days} onChange={(e) => setForm({ ...form, expires_days: +e.target.value })} />
            </Field>
          </div>
          <div>
            <div className="mb-1.5 text-[12.5px] font-medium text-foreground/80">Projects</div>
            <div className="flex flex-wrap gap-2">
              {(projects.data ?? []).map((p) => (
                <button
                  type="button"
                  key={p.slug}
                  onClick={() => toggleProject(p.slug)}
                  className={`rounded-full border px-3 py-1 font-mono text-[12px] transition ${form.projects.includes(p.slug) ? "border-primary bg-primary/10 text-primary" : "text-muted-foreground hover:bg-accent/50"}`}
                >
                  {p.slug}
                </button>
              ))}
            </div>
            <div className="mt-1.5 text-[11.5px] text-muted-foreground">{form.projects.length ? `Limited to ${form.projects.length} project(s)` : "None selected = all projects"}</div>
          </div>
          <Checkbox
            checked={form.allow_destructive}
            onChange={(e) => setForm({ ...form, allow_destructive: e.target.checked })}
            label="Allow destructive actions without my approval"
            hint="Leave off for agents: deletes and replace-restores will wait on the Approvals page for you."
          />
          <Button variant="primary" type="submit" loading={busy} icon={<Plus className="h-4 w-4" />}>
            Create token
          </Button>
        </form>
      </Card>

      <Card flush>
        {tokens.isLoading ? (
          <Loading />
        ) : tokens.error ? (
          <ErrorBox error={tokens.error} className="m-4" />
        ) : !tokens.data?.length ? (
          <Empty icon={<KeyRound className="h-5 w-5" />} title="No tokens yet">Create one to connect an agent over MCP or use the CLI.</Empty>
        ) : (
          <DataTable
            rows={tokens.data}
            rowKey={(t) => t.id}
            columns={[
              { key: "name", label: "Name", render: (t) => <div><div className="font-medium">{t.name}</div><div className="font-mono text-[11px] text-muted-foreground">{t.prefix}</div></div> },
              { key: "level", label: "Level", render: (t) => <Badge tone={t.level === "admin" ? "warning" : t.level === "write" ? "primary" : "neutral"}>{t.level}</Badge> },
              { key: "projects", label: "Projects", render: (t) => <span className="font-mono text-[12px]">{t.projects?.length ? t.projects.join(", ") : "all"}</span> },
              { key: "d", label: "Destructive", render: (t) => (t.allow_destructive ? <Badge tone="warning">allowed</Badge> : <Badge tone="success">needs approval</Badge>) },
              { key: "used", label: "Last used", render: (t) => <span className="text-muted-foreground">{ago(t.last_used_at)}</span> },
              {
                key: "x",
                label: "",
                className: "text-right",
                render: (t) =>
                  t.revoked ? (
                    <Badge>revoked</Badge>
                  ) : (
                    <Button
                      size="sm"
                      variant="danger-outline"
                      onClick={async () => {
                        if (!confirm(`Revoke ${t.name}? Anything using it stops working immediately.`)) return;
                        try {
                          await op("tokens.revoke", { id: t.id });
                          qc.invalidateQueries({ queryKey: ["op", "tokens.list"] });
                        } catch (e) {
                          toast.error(e);
                        }
                      }}
                    >
                      Revoke
                    </Button>
                  ),
              },
            ]}
          />
        )}
      </Card>

      {created && <TokenCreated t={created} onClose={() => setCreated(null)} />}
    </>
  );
}

function TokenCreated({ t, onClose }: { t: any; onClose: () => void }) {
  const url = location.origin;
  const claude = `claude mcp add --transport http ivarabase ${url}/mcp --header "Authorization: Bearer ${t.token}"`;
  const http = pretty({ mcpServers: { ivarabase: { type: "http", url: `${url}/mcp`, headers: { Authorization: `Bearer ${t.token}` } } } });
  const stdio = pretty({ mcpServers: { ivarabase: { command: "ivarabase", args: ["mcp", "--url", url, "--token", t.token] } } });
  return (
    <Dialog open onClose={onClose} title={`Token "${t.name}" created`} description="Copy it now - it is never shown again." size="lg" footer={<Button variant="primary" onClick={onClose}>Done</Button>}>
      <div className="space-y-4">
        <CopyField value={t.token} />
        <Callout tone="neutral">
          <div className="label mb-1.5">Claude Code</div>
          <CopyField value={claude} />
        </Callout>
        <div className="grid gap-3 sm:grid-cols-2">
          <div>
            <div className="label mb-1.5">MCP config (HTTP)</div>
            <Code lang="json" code={http} className="max-h-48 text-[11.5px]" />
          </div>
          <div>
            <div className="label mb-1.5">MCP config (stdio)</div>
            <Code lang="json" code={stdio} className="max-h-48 text-[11.5px]" />
          </div>
        </div>
        <div>
          <div className="label mb-1.5">CLI</div>
          <CopyField value={`ivarabase login --url ${url} --token ${t.token}`} />
        </div>
      </div>
    </Dialog>
  );
}
