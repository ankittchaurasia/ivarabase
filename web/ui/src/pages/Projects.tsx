import { useQueryClient } from "@tanstack/react-query";
import { ArrowUpRight, Database, Plus } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Badge, Button, Callout, Card, CopyField, Empty, ErrorBox, Field, Input, Loading, PageHeader } from "../components/ui";
import { op, useOp, type Project } from "../lib/api";
import { ago, bytes } from "../lib/format";

export function Projects() {
  const qc = useQueryClient();
  const projects = useOp<Project[]>("projects.list");
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [created, setCreated] = useState<any>(null);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const r = await op("projects.create", { slug: slug.trim(), name: name.trim() || undefined });
      setCreated(r);
      setSlug("");
      setName("");
      qc.invalidateQueries({ queryKey: ["op", "projects.list"] });
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader eyebrow="Platform" title="Projects" description="Each project is its own Postgres database and login role - created in well under a second." />

      <Card title="New project" className="mb-6">
        <form onSubmit={create} className="grid gap-4 sm:grid-cols-[1fr_1fr_auto] sm:items-end">
          <Field label="Slug" hint="Database and role name: a-z, 0-9, _">
            <Input placeholder="my_app" value={slug} onChange={(e) => setSlug(e.target.value.toLowerCase())} required pattern="[a-z][a-z0-9_]{2,39}" autoCapitalize="off" />
          </Field>
          <Field label="Display name" hint="Optional">
            <Input placeholder="My App" value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Button variant="primary" type="submit" loading={busy} icon={<Plus className="h-4 w-4" />} className="sm:mb-[22px]">
            Create
          </Button>
        </form>
        <ErrorBox error={error} className="mt-4" />
        {created && (
          <Callout tone="success" className="mt-4" title={`Created ${created.slug} in ${created.created_ms} ms`}>
            <div className="mb-2 text-foreground/80">Connection string (also available later on the project page):</div>
            <CopyField value={created.connection.url} secret />
            <Link to={`/p/${created.slug}`} className="mt-3 inline-flex items-center gap-1 text-[13px] font-medium text-primary hover:underline">
              Open project <ArrowUpRight className="h-3.5 w-3.5" />
            </Link>
          </Callout>
        )}
      </Card>

      {projects.isLoading ? (
        <Loading />
      ) : projects.error ? (
        <ErrorBox error={projects.error} />
      ) : !projects.data?.length ? (
        <Card>
          <Empty icon={<Database className="h-5 w-5" />} title="No projects yet">
            Create your first project above. You get a database, a role and a connection string immediately.
          </Empty>
        </Card>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {projects.data.map((p) => (
            <Link key={p.slug} to={`/p/${p.slug}`} className="group rounded-lg border bg-card p-4 shadow-soft transition hover:-translate-y-px hover:border-primary/40">
              <div className="flex items-start gap-3">
                <span className="grid h-9 w-9 shrink-0 place-items-center rounded-md bg-primary/10 font-mono text-[12px] font-semibold uppercase text-primary">{p.name.slice(0, 2)}</span>
                <div className="min-w-0 flex-1">
                  <div className="truncate font-semibold">{p.name}</div>
                  <div className="truncate font-mono text-[11.5px] text-muted-foreground">{p.slug}</div>
                </div>
                <ArrowUpRight className="h-4 w-4 text-muted-foreground opacity-0 transition group-hover:opacity-100" />
              </div>
              <div className="mt-4 grid grid-cols-3 gap-2 border-t pt-3 text-[12px]">
                <div>
                  <div className="label">Size</div>
                  <div className="mt-0.5">{bytes(p.size_bytes)}</div>
                </div>
                <div>
                  <div className="label">Conns</div>
                  <div className="mt-0.5">{p.connections}</div>
                </div>
                <div>
                  <div className="label">Backup</div>
                  <div className="mt-0.5">{p.last_backup_at ? ago(p.last_backup_at) : <Badge tone="warning">none</Badge>}</div>
                </div>
              </div>
            </Link>
          ))}
        </div>
      )}
    </>
  );
}
