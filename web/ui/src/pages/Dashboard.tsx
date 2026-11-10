import { useQueryClient } from "@tanstack/react-query";
import { ArchiveRestore, ArrowUpRight, Database, HardDrive, Plus, ShieldAlert } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Badge, Button, Card, CopyField, Dot, ErrorBox, Loading, PageHeader, Stat, toast } from "../components/ui";
import { op, useOp, type Project } from "../lib/api";
import { ago, bytes } from "../lib/format";

const toneOf = (s: string) => (s === "ok" ? "success" : s === "warn" ? "warning" : s === "error" ? "danger" : "info") as any;

export function Dashboard() {
  const qc = useQueryClient();
  const health = useOp<any>("system.health", {}, { refetchInterval: 30_000 });
  const info = useOp<any>("system.info");
  const projects = useOp<Project[]>("projects.list");
  const [busy, setBusy] = useState(false);

  if (health.error) return <ErrorBox error={health.error} />;
  if (!health.data || !info.data) return <Loading />;
  const h = health.data;
  const i = info.data;
  const list = projects.data ?? [];
  const total = list.reduce((a, p) => a + (p.size_bytes || 0), 0);
  const problems = h.checks.filter((c: any) => c.status === "warn" || c.status === "error");

  return (
    <>
      <PageHeader
        eyebrow="Overview"
        title="Dashboard"
        description="Health, projects and what needs your attention."
        actions={
          <>
            <Button
              icon={<ArchiveRestore className="h-4 w-4" />}
              loading={busy}
              onClick={async () => {
                setBusy(true);
                try {
                  await op("backups.create", {});
                  toast.success("Backed up every project");
                  qc.invalidateQueries();
                } catch (e) {
                  toast.error(e);
                } finally {
                  setBusy(false);
                }
              }}
            >
              Back up all
            </Button>
            <Link to="/projects">
              <Button variant="primary" icon={<Plus className="h-4 w-4" />}>New project</Button>
            </Link>
          </>
        }
      />

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="Status" value={<span className="capitalize">{h.status === "ok" ? "Healthy" : h.status === "warn" ? "Attention" : "Problem"}</span>} hint={`${problems.length} check${problems.length === 1 ? "" : "s"} need attention`} icon={<ShieldAlert className="h-4 w-4" />} />
        <Stat label="Projects" value={i.projects} hint={`${i.active_tokens} active token${i.active_tokens === 1 ? "" : "s"}`} icon={<Database className="h-4 w-4" />} />
        <Stat label="Data" value={bytes(total)} hint={`Postgres ${i.postgres.version}`} icon={<HardDrive className="h-4 w-4" />} />
        <Stat label="Approvals" value={i.pending_approvals} hint="waiting for you" icon={<ArchiveRestore className="h-4 w-4" />} />
      </div>

      <div className="mt-5 grid gap-5 lg:grid-cols-[1.4fr_1fr]">
        <Card title="Health checks" description="Each problem names the operation that fixes it - agents read the same list.">
          <ul className="-mx-1 divide-y">
            {h.checks.map((c: any) => (
              <li key={c.name} className="flex gap-3 px-1 py-3">
                <span className="mt-1.5"><Dot tone={toneOf(c.status)} /></span>
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-medium">{c.name.replace(/_/g, " ")}</span>
                    <Badge tone={toneOf(c.status)}>{c.status}</Badge>
                    {c.fix_op && <code className="text-[11px] text-muted-foreground">{c.fix_op}</code>}
                  </div>
                  <div className="text-[13px] text-foreground/80">{c.message}</div>
                  {c.hint && <div className="mt-0.5 text-[12.5px] text-muted-foreground">{c.hint}</div>}
                </div>
              </li>
            ))}
          </ul>
        </Card>

        <div className="flex flex-col gap-5">
          <Card
            title="Projects"
            actions={<Link to="/projects" className="text-[12.5px] text-primary hover:underline">All projects</Link>}
          >
            {list.length === 0 ? (
              <p className="text-[13px] text-muted-foreground">No projects yet.</p>
            ) : (
              <ul className="-mx-1 divide-y">
                {list.slice(0, 6).map((p) => (
                  <li key={p.slug}>
                    <Link to={`/p/${p.slug}`} className="group flex items-center gap-3 rounded-md px-1 py-2.5 hover:bg-accent/40">
                      <span className="grid h-8 w-8 place-items-center rounded-md bg-muted font-mono text-[11px] font-semibold uppercase text-muted-foreground">{p.name.slice(0, 2)}</span>
                      <span className="min-w-0 flex-1">
                        <span className="block truncate font-medium">{p.name}</span>
                        <span className="block truncate text-[12px] text-muted-foreground">
                          {bytes(p.size_bytes)} · backup {ago(p.last_backup_at)}
                        </span>
                      </span>
                      <ArrowUpRight className="h-4 w-4 text-muted-foreground opacity-0 transition group-hover:opacity-100" />
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card title="Connect an agent" description="Model Context Protocol over HTTP.">
            <CopyField value={i.endpoints.mcp} />
            <p className="mt-2 text-[12.5px] text-muted-foreground">
              Create a token under <Link to="/tokens" className="text-primary hover:underline">Access tokens</Link> to get a ready-made command.
            </p>
          </Card>

          <Card title="System">
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-[13px]">
              <dt className="text-muted-foreground">Version</dt><dd>{i.version}</dd>
              <dt className="text-muted-foreground">Platform</dt><dd>{i.os} / {i.arch}</dd>
              <dt className="text-muted-foreground">Postgres</dt><dd>{i.postgres.version} · port {i.postgres.port}{i.postgres.managed ? " · managed" : ""}</dd>
              <dt className="text-muted-foreground">Deno</dt><dd>{i.deno.available ? i.deno.version : "not installed"}</dd>
              <dt className="text-muted-foreground">HTTPS</dt>
              <dd>{i.tls?.mode === "off" ? (i.tls?.domain ? "off (plain HTTP)" : "off - local only") : `${({ auto: "Let's Encrypt", files: "certificate files", proxy: "your proxy (nginx/Caddy)" } as Record<string, string>)[i.tls?.mode] ?? i.tls?.mode} · ${i.tls?.domain}`}</dd>
              <dt className="text-muted-foreground">Postgres TLS</dt><dd>{i.tls?.postgres_tls ? "on (sslmode=require)" : "off"}</dd>
              <dt className="text-muted-foreground">Uptime</dt><dd>{Math.round(i.uptime_s / 60)} min</dd>
              <dt className="text-muted-foreground">Data</dt><dd className="truncate font-mono text-[12px]" title={i.data_dir}>{i.data_dir}</dd>
            </dl>
          </Card>
        </div>
      </div>
    </>
  );
}
