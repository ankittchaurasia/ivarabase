import { useQueryClient } from "@tanstack/react-query";
import { ArchiveRestore, ArrowUpRight, Database, Table2, TerminalSquare, Zap } from "lucide-react";
import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Button, Callout, Card, CopyField, Dialog, ErrorBox, Field, Input, Loading, PageHeader, Stat, Textarea, toast, useRunner } from "../components/ui";
import { op, useOp } from "../lib/api";
import { MetricChart, RangePicker, type Range } from "./Monitoring";
import { ago, bytes } from "../lib/format";

export function ProjectOverview() {
  const { slug = "" } = useParams();
  const qc = useQueryClient();
  const nav = useNavigate();
  const run = useRunner();
  const p = useOp<any>("projects.get", { project: slug });
  const [creds, setCreds] = useState<any>(null);
  const [name, setName] = useState("");
  const [notes, setNotes] = useState("");
  const [rotated, setRotated] = useState<any>(null);
  const [busy, setBusy] = useState("");
  const [range, setRange] = useState<Range>("24h");

  useEffect(() => {
    if (p.data) {
      setName(p.data.name);
      setNotes(p.data.notes ?? "");
    }
  }, [p.data]);
  useEffect(() => setCreds(null), [slug]);

  if (p.error) return <ErrorBox error={p.error} />;
  if (!p.data) return <Loading />;
  const d = p.data;

  const act = async (key: string, fn: () => Promise<void>) => {
    setBusy(key);
    try {
      await fn();
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy("");
    }
  };

  return (
    <>
      <PageHeader
        eyebrow={<span className="font-mono normal-case tracking-normal">{slug}</span>}
        title={d.name}
        description={d.notes || "Postgres database, role, functions and backups."}
        actions={
          <>
            <Button
              icon={<ArchiveRestore className="h-4 w-4" />}
              loading={busy === "backup"}
              onClick={() =>
                act("backup", async () => {
                  const b = await op("backups.create", { project: slug, verify: true });
                  toast.success(b.verify?.ok ? "Backup created and verified" : "Backup created");
                  qc.invalidateQueries();
                })
              }
            >
              Back up now
            </Button>
            <Link to={`/p/${slug}/sql`}>
              <Button variant="primary" icon={<TerminalSquare className="h-4 w-4" />}>SQL editor</Button>
            </Link>
          </>
        }
      />

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="Database" value={bytes(d.size_bytes)} hint={`${d.connections} open connection${d.connections === 1 ? "" : "s"}`} icon={<Database className="h-4 w-4" />} />
        <Stat label="Tables" value={d.tables} hint={`${d.migrations} migration${d.migrations === 1 ? "" : "s"}`} icon={<Table2 className="h-4 w-4" />} />
        <Stat label="Functions" value={d.functions} hint={`${d.secrets} secret${d.secrets === 1 ? "" : "s"}`} icon={<Zap className="h-4 w-4" />} />
        <Stat label="Last backup" value={d.last_backup_at ? ago(d.last_backup_at) : "never"} hint={`${d.backups} kept`} icon={<ArchiveRestore className="h-4 w-4" />} />
      </div>

      <Card title="Traffic" description="API requests per minute by service" className="mt-5" actions={<RangePicker value={range} onChange={setRange} />}>
        <MetricChart
          scope={slug}
          range={range}
          metrics={[
            { name: "req_rest", label: "REST" },
            { name: "req_auth", label: "Auth" },
            { name: "req_storage", label: "Storage" },
            { name: "req_functions", label: "Functions" },
          ]}
          unit="/min"
        />
        <div className="mt-4 grid gap-5 md:grid-cols-2">
          <div>
            <div className="label mb-2">Errors per minute (5xx)</div>
            <MetricChart scope={slug} range={range} height={120} metrics={[{ name: "err_total", label: "Errors" }]} unit="/min" />
          </div>
          <div>
            <div className="label mb-2">Average latency (ms)</div>
            <MetricChart scope={slug} range={range} height={120} metrics={[{ name: "lat_avg", label: "Latency" }]} unit=" ms" />
          </div>
        </div>
      </Card>

      <div className="mt-5 grid gap-5 lg:grid-cols-[1.5fr_1fr]">
        <Card title="Connect" description="Use this as DATABASE_URL. The project role owns the database.">
          {creds ? (
            <CopyField value={creds.url} secret />
          ) : (
            <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
              <CopyField value={d.connection.url} className="flex-1" />
              <Button
                loading={busy === "creds"}
                onClick={() => act("creds", async () => setCreds(await op("projects.credentials", { project: slug })))}
              >
                Reveal password
              </Button>
            </div>
          )}
          <dl className="mt-4 grid grid-cols-2 gap-3 text-[13px] sm:grid-cols-4">
            {[
              ["Host", d.connection.host],
              ["Port", d.connection.port],
              ["Database", d.connection.database],
              ["User", d.connection.user],
            ].map(([k, v]) => (
              <div key={k as string} className="rounded-md border bg-muted/40 px-3 py-2">
                <dt className="label">{k}</dt>
                <dd className="mt-0.5 truncate font-mono text-[12.5px]">{v}</dd>
              </div>
            ))}
          </dl>
          <div className="mt-4">
            <div className="label mb-1.5">API URL (supabase-js)</div>
            <CopyField value={d.api_url} />
          </div>
          {d.tables_without_rls?.length > 0 && (
            <Callout tone="warning" className="mt-4" title="Tables open to the anon key">
              {d.tables_without_rls.join(", ")} - <Link to={`/p/${slug}/api`} className="font-medium underline">enable row level security</Link>
            </Callout>
          )}
        </Card>

        <Card title="Quick links">
          <ul className="-mx-1 divide-y text-[13.5px]">
            {[
              ["API", `/p/${slug}/api`, "URL, keys and row level security"],
              ["Table editor", `/p/${slug}/tables`, "Browse and edit rows"],
              ["Migrations", `/p/${slug}/migrations`, "Versioned schema changes"],
              ["Functions", `/p/${slug}/functions`, "Deploy TypeScript on Deno"],
              ["Backups", `/p/${slug}/backups`, "Verify and restore"],
            ].map(([label, to, sub]) => (
              <li key={to}>
                <Link to={to} className="group flex items-center gap-2 rounded-md px-1 py-2.5 hover:bg-accent/40">
                  <span className="flex-1">
                    <span className="block font-medium">{label}</span>
                    <span className="block text-[12px] text-muted-foreground">{sub}</span>
                  </span>
                  <ArrowUpRight className="h-4 w-4 text-muted-foreground group-hover:text-foreground" />
                </Link>
              </li>
            ))}
          </ul>
        </Card>
      </div>

      <Card title="Settings" className="mt-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Display name">
            <Input value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Notes" hint="Visible to agents in projects.get">
            <Textarea className="min-h-[38px] font-sans text-[13.5px]" rows={1} value={notes} onChange={(e) => setNotes(e.target.value)} />
          </Field>
        </div>
        <div className="mt-4">
          <Button
            variant="primary"
            loading={busy === "save"}
            onClick={() =>
              act("save", async () => {
                await op("projects.update", { project: slug, name, notes });
                toast.success("Saved");
                qc.invalidateQueries();
              })
            }
          >
            Save changes
          </Button>
        </div>
      </Card>

      <Card title="Danger zone" className="mt-5 border-destructive/30">
        <div className="divide-y">
          <div className="flex flex-wrap items-center justify-between gap-3 pb-4">
            <div>
              <div className="font-medium">Rotate database password</div>
              <div className="text-[12.5px] text-muted-foreground">Apps using the current password stop connecting immediately.</div>
            </div>
            <Button
              variant="danger-outline"
              loading={busy === "rotate"}
              onClick={() =>
                act("rotate", async () => {
                  setRotated(await run("projects.rotate_password", { project: slug }, { confirmLabel: "Rotate password" }));
                })
              }
            >
              Rotate password
            </Button>
          </div>
          <div className="flex flex-wrap items-center justify-between gap-3 pt-4">
            <div>
              <div className="font-medium">Delete project</div>
              <div className="text-[12.5px] text-muted-foreground">Drops the database, role, functions and secrets. Backups are kept.</div>
            </div>
            <Button
              variant="danger"
              loading={busy === "delete"}
              onClick={() =>
                act("delete", async () => {
                  await run("projects.delete", { project: slug }, { title: `Delete ${slug}?`, confirmLabel: "Delete project" });
                  toast.success("Deleted " + slug);
                  localStorage.removeItem("ivara.lastProject");
                  qc.invalidateQueries();
                  nav("/projects");
                })
              }
            >
              Delete project
            </Button>
          </div>
        </div>
      </Card>

      <Dialog open={!!rotated} onClose={() => setRotated(null)} title="New password" description="Update every app that connects to this database.">
        {rotated && <CopyField value={rotated.url} secret />}
      </Dialog>
    </>
  );
}
