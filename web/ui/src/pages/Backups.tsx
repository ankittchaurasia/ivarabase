import { useQueryClient } from "@tanstack/react-query";
import { ArchiveRestore, CheckCircle2, CloudUpload, Download, HardDrive, Play, RefreshCw, ShieldCheck, Trash2, XCircle } from "lucide-react";
import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Badge, Button, Callout, Card, Checkbox, DataTable, Dialog, Empty, ErrorBox, Field, Input, Loading, PageHeader, Stat, Switch, toast, useRunner } from "../components/ui";
import { isCancelled, op, useOp, type Backup } from "../lib/api";
import { ago, bytes, date } from "../lib/format";

function VerifyBadge({ b }: { b: Backup }) {
  if (b.verified === true)
    return (
      <Badge tone="success" title={b.verify_detail}>
        <CheckCircle2 className="h-3 w-3" /> verified
      </Badge>
    );
  if (b.verified === false)
    return (
      <Badge tone="danger" title={b.verify_detail}>
        <XCircle className="h-3 w-3" /> failed
      </Badge>
    );
  return <Badge>unverified</Badge>;
}

function BackupTable({ backups, showProject }: { backups: Backup[]; showProject?: boolean }) {
  const qc = useQueryClient();
  const run = useRunner();
  const [restore, setRestore] = useState<Backup | null>(null);
  const [busy, setBusy] = useState("");

  const verify = async (b: Backup) => {
    setBusy(b.id);
    try {
      const v = await op("backups.verify", { id: b.id });
      v.ok ? toast.success("Verified: " + v.detail) : toast.error(new Error("Verification failed: " + v.detail));
      qc.invalidateQueries({ queryKey: ["op", "backups.list"] });
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy("");
    }
  };

  return (
    <>
      <DataTable
        rows={backups}
        rowKey={(b) => b.id}
        empty="No backups yet."
        columns={[
          {
            key: "created",
            label: "Created",
            render: (b) => (
              <div>
                <div className="font-medium">{ago(b.created_at)}</div>
                <div className="whitespace-nowrap text-[11.5px] text-muted-foreground">{date(b.created_at)}</div>
              </div>
            ),
          },
          ...(showProject ? [{ key: "project", label: "Project", render: (b: Backup) => <Link to={`/p/${b.project}/backups`} className="font-mono text-[12.5px] hover:text-primary">{b.project}</Link> }] : []),
          { key: "kind", label: "Kind", render: (b) => <Badge tone={b.kind === "manual" ? "primary" : b.kind === "pre-restore" ? "warning" : "neutral"}>{b.kind}</Badge> },
          { key: "size", label: "Size", render: (b) => <span className="font-mono text-[12px]">{bytes(b.size_bytes)}</span> },
          {
            key: "where",
            label: "Copies",
            render: (b) => (
              <span className="flex gap-1">
                {b.local && <Badge><HardDrive className="h-3 w-3" /> local</Badge>}
                {b.offsite && <Badge tone="info"><CloudUpload className="h-3 w-3" /> off-site</Badge>}
              </span>
            ),
          },
          { key: "check", label: "Restore test", render: (b) => <VerifyBadge b={b} /> },
          {
            key: "actions",
            label: "",
            className: "text-right",
            render: (b) => (
              <div className="flex justify-end gap-1">
                <Button size="sm" variant="ghost" icon={<ShieldCheck className="h-3.5 w-3.5" />} loading={busy === b.id} onClick={() => verify(b)}>
                  Verify
                </Button>
                <Button size="sm" variant="ghost" icon={<ArchiveRestore className="h-3.5 w-3.5" />} onClick={() => setRestore(b)}>
                  Restore
                </Button>
                <a href={`/api/v1/backups/${b.id}/download`}>
                  <Button size="sm" variant="ghost" aria-label="Download" title="Download encrypted bundle">
                    <Download className="h-3.5 w-3.5" />
                  </Button>
                </a>
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label="Delete"
                  title="Delete"
                  className="hover:text-destructive"
                  onClick={async () => {
                    try {
                      await run("backups.delete", { id: b.id }, { title: "Delete this backup?", confirmLabel: "Delete backup" });
                      toast.success("Backup deleted");
                      qc.invalidateQueries({ queryKey: ["op", "backups.list"] });
                    } catch (e) {
                      toast.error(e);
                    }
                  }}
                >
                  <Trash2 className="h-3.5 w-3.5" />
                </Button>
              </div>
            ),
          },
        ]}
      />
      {restore && <RestoreDialog b={restore} onClose={() => setRestore(null)} />}
    </>
  );
}

function RestoreDialog({ b, onClose }: { b: Backup; onClose: () => void }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const run = useRunner();
  const [mode, setMode] = useState<"new" | "replace">("new");
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function go() {
    setBusy(true);
    setError(null);
    try {
      const r = await run("backups.restore", { id: b.id, mode, target_project: target.trim() || undefined }, { title: `Replace ${target || b.project}?`, confirmLabel: "Replace project" });
      toast.success(`Restored into ${r.project}`);
      qc.invalidateQueries();
      onClose();
      nav(`/p/${r.project}`);
    } catch (e) {
      if (!isCancelled(e)) setError(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      open
      onClose={onClose}
      title="Restore backup"
      description={`${b.project} · ${date(b.created_at)}`}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant={mode === "replace" ? "danger" : "primary"} loading={busy} onClick={go}>
            {mode === "replace" ? "Replace project" : "Restore as new project"}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="grid gap-2 sm:grid-cols-2">
          {(["new", "replace"] as const).map((m) => (
            <button
              key={m}
              onClick={() => setMode(m)}
              className={`rounded-md border p-3 text-left transition ${mode === m ? "border-primary bg-primary/[0.06] ring-2 ring-primary/15" : "hover:bg-accent/40"}`}
            >
              <div className="font-medium">{m === "new" ? "As a new project" : "Replace existing"}</div>
              <div className="mt-0.5 text-[12px] text-muted-foreground">{m === "new" ? "Safe: nothing existing is touched." : "Overwrites the project with this backup."}</div>
            </button>
          ))}
        </div>
        <Field label="Target project" hint={mode === "new" ? `Default: ${b.project}_restored` : `Default: ${b.project}`}>
          <Input value={target} onChange={(e) => setTarget(e.target.value.toLowerCase())} placeholder={mode === "new" ? `${b.project}_restored` : b.project} />
        </Field>
        {mode === "replace" && (
          <Callout tone="warning" title="How replace stays safe">
            A safety backup of the current state is taken first. The backup is restored into a side database and checked table by table; the live project is only swapped
            when everything matches.
          </Callout>
        )}
        <ErrorBox error={error} />
      </div>
    </Dialog>
  );
}

export function ProjectBackups() {
  const { slug = "" } = useParams();
  const qc = useQueryClient();
  const backups = useOp<Backup[]>("backups.list", { project: slug });
  const [busy, setBusy] = useState(false);
  return (
    <>
      <PageHeader
        eyebrow="Recovery"
        title="Backups"
        description="Encrypted bundles: the database dump plus functions, secrets and migration history."
        actions={
          <>
            <Link to="/backups">
              <Button>Schedule &amp; off-site</Button>
            </Link>
            <Button
              variant="primary"
              icon={<ArchiveRestore className="h-4 w-4" />}
              loading={busy}
              onClick={async () => {
                setBusy(true);
                try {
                  const b = await op("backups.create", { project: slug, verify: true });
                  b.verify?.ok ? toast.success("Backup created and verified") : toast.success("Backup created");
                  qc.invalidateQueries();
                } catch (e) {
                  toast.error(e);
                } finally {
                  setBusy(false);
                }
              }}
            >
              Back up + verify now
            </Button>
          </>
        }
      />
      <Card flush>{backups.isLoading ? <Loading /> : backups.error ? <ErrorBox error={backups.error} className="m-4" /> : <BackupTable backups={backups.data ?? []} />}</Card>
    </>
  );
}

export function Backups() {
  const qc = useQueryClient();
  const cfg = useOp<any>("backups.config_get");
  const backups = useOp<Backup[]>("backups.list");
  const [form, setForm] = useState<any>(null);
  const [saving, setSaving] = useState(false);
  const [running, setRunning] = useState(false);
  const [test, setTest] = useState<any>(null);
  const [testing, setTesting] = useState(false);

  useEffect(() => {
    if (cfg.data) {
      const c = cfg.data.config;
      setForm({
        enabled: c.enabled, schedule_hours: c.schedule_hours, keep_last: c.keep_last, verify_after: c.verify_after,
        s3_endpoint: c.s3.endpoint, s3_region: c.s3.region, s3_bucket: c.s3.bucket, s3_prefix: c.s3.prefix, s3_access_key: c.s3.access_key, s3_secret_key: c.s3.secret_key,
      });
    }
  }, [cfg.data]);

  if (cfg.error) return <ErrorBox error={cfg.error} />;
  if (!cfg.data || !form) return <Loading />;
  const last = cfg.data.last_run;
  const list = backups.data ?? [];
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.type === "number" ? +e.target.value : e.target.value });

  async function save() {
    setSaving(true);
    try {
      await op("backups.config_set", form);
      toast.success("Backup settings saved");
      qc.invalidateQueries({ queryKey: ["op", "backups.config_get"] });
      qc.invalidateQueries({ queryKey: ["op", "system.health"] });
    } catch (e) {
      toast.error(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
      <PageHeader
        eyebrow="Recovery"
        title="Backups"
        description="Schedule, retention, restore tests and off-site copies for every project."
        actions={
          <>
            <Button
              icon={<RefreshCw className="h-4 w-4" />}
              onClick={async () => {
                try {
                  const r = await op("backups.sync");
                  toast.success(`Found ${r.bundles_found} bundles`);
                  qc.invalidateQueries({ queryKey: ["op", "backups.list"] });
                } catch (e) {
                  toast.error(e);
                }
              }}
            >
              Rescan
            </Button>
            <Button
              variant="primary"
              icon={<Play className="h-4 w-4" />}
              loading={running}
              onClick={async () => {
                setRunning(true);
                try {
                  const r = await op("backups.run_schedule");
                  r.ok ? toast.success(`Backed up ${r.created} project(s)`) : toast.error(new Error(r.errors.join("; ")));
                  qc.invalidateQueries();
                } catch (e) {
                  toast.error(e);
                } finally {
                  setRunning(false);
                }
              }}
            >
              Run job now
            </Button>
          </>
        }
      />

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="Backups" value={list.length} hint={bytes(list.reduce((a, b) => a + b.size_bytes, 0)) + " total"} />
        <Stat label="Last run" value={last ? ago(last.at) : "never"} hint={last ? (last.ok ? `${last.created} created, all ok` : "had errors") : "not run yet"} />
        <Stat label="Next run" value={cfg.data.next_run_at && form.enabled ? ago(cfg.data.next_run_at) : "off"} hint={`every ${form.schedule_hours}h`} />
        <Stat label="Off-site" value={cfg.data.config.s3.bucket ? "On" : "Off"} hint={cfg.data.config.s3.bucket || "this machine only"} />
      </div>

      <Callout tone="info" className="mt-5" title="Keep your recovery key safe">
        Bundles are encrypted with the recovery key in <code>{cfg.data.recovery_key_file}</code>. It is the only way to restore on a new machine - keep a copy in your password
        manager.
      </Callout>
      {last && !last.ok && (
        <Callout tone="danger" className="mt-3" title="The last scheduled run had problems">
          {last.errors.join("; ")}
        </Callout>
      )}

      <div className="mt-5 grid gap-5 lg:grid-cols-2">
        <Card title="Schedule" description="Runs in the background while the server is up.">
          <div className="space-y-4">
            <Switch checked={form.enabled} onChange={(v) => setForm({ ...form, enabled: v })} label="Scheduled backups" />
            <div className="grid grid-cols-2 gap-4">
              <Field label="Every (hours)">
                <Input type="number" min={1} max={168} value={form.schedule_hours} onChange={set("schedule_hours")} />
              </Field>
              <Field label="Keep per project">
                <Input type="number" min={1} max={365} value={form.keep_last} onChange={set("keep_last")} />
              </Field>
            </div>
            <Checkbox
              checked={form.verify_after}
              onChange={(e) => setForm({ ...form, verify_after: e.target.checked })}
              label="Restore-test every scheduled backup"
              hint="Restores into a scratch database and compares every table's row count."
            />
            <Button variant="primary" loading={saving} onClick={save}>
              Save schedule
            </Button>
          </div>
        </Card>

        <Card title="Off-site copies" description="Any S3-compatible bucket: AWS, Cloudflare R2, Backblaze B2, MinIO, Hetzner, Wasabi.">
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Endpoint" className="sm:col-span-2">
              <Input placeholder="https://<account>.r2.cloudflarestorage.com" value={form.s3_endpoint} onChange={set("s3_endpoint")} />
            </Field>
            <Field label="Bucket">
              <Input value={form.s3_bucket} onChange={set("s3_bucket")} />
            </Field>
            <Field label="Region">
              <Input placeholder="auto / us-east-1" value={form.s3_region} onChange={set("s3_region")} />
            </Field>
            <Field label="Prefix">
              <Input placeholder="ivarabase/" value={form.s3_prefix} onChange={set("s3_prefix")} />
            </Field>
            <Field label="Access key">
              <Input autoComplete="off" value={form.s3_access_key} onChange={set("s3_access_key")} />
            </Field>
            <Field label="Secret key" className="sm:col-span-2">
              <Input type="password" autoComplete="off" value={form.s3_secret_key} onChange={set("s3_secret_key")} />
            </Field>
          </div>
          <div className="mt-4 flex flex-wrap items-center gap-2">
            <Button variant="primary" loading={saving} onClick={save}>
              Save target
            </Button>
            <Button
              loading={testing}
              onClick={async () => {
                setTesting(true);
                setTest(null);
                try {
                  setTest(await op("backups.test_target"));
                } catch (e) {
                  setTest({ error: e });
                } finally {
                  setTesting(false);
                }
              }}
            >
              Test connection
            </Button>
          </div>
          {test?.error && <ErrorBox error={test.error} className="mt-3" />}
          {test && !test.error && (
            <Callout tone={test.ok ? "success" : "danger"} className="mt-3">
              {test.ok ? "Bucket works: write, list, read and delete all succeeded." : test.steps.filter((s: any) => !s.ok).map((s: any) => `${s.step}: ${s.error}`).join("; ")}
            </Callout>
          )}
        </Card>
      </div>

      <Card title="All backups" className="mt-5" flush>
        {backups.isLoading ? <Loading /> : list.length ? <BackupTable backups={list} showProject /> : <Empty icon={<ArchiveRestore className="h-5 w-5" />} title="No backups yet" />}
      </Card>
    </>
  );
}

