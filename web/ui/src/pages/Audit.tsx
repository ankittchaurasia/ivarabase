import { useState } from "react";
import { Badge, Card, DataTable, Dialog, ErrorBox, Loading, PageHeader, Select } from "../components/ui";
import { Code } from "../components/Code";
import { useOp, type Project } from "../lib/api";
import { ago, date, pretty } from "../lib/format";

export function Audit() {
  const [project, setProject] = useState("");
  const projects = useOp<Project[]>("projects.list");
  const rows = useOp<any[]>("audit.list", { project: project || undefined, limit: 300 }, { refetchInterval: 20_000 });
  const [view, setView] = useState<any>(null);
  return (
    <>
      <PageHeader
        eyebrow="Access"
        title="Audit log"
        description="Every change - from the UI, API, CLI or an agent - with who did it and how it went."
        actions={
          <Select className="w-[200px]" value={project} onChange={(e) => setProject(e.target.value)}>
            <option value="">All projects</option>
            {(projects.data ?? []).map((p) => (
              <option key={p.slug}>{p.slug}</option>
            ))}
          </Select>
        }
      />
      <Card flush>
        {rows.isLoading ? (
          <Loading />
        ) : rows.error ? (
          <ErrorBox error={rows.error} className="m-4" />
        ) : (
          <DataTable
            rows={rows.data ?? []}
            rowKey={(r) => r.id}
            onRowClick={setView}
            empty="No changes recorded yet."
            columns={[
              { key: "at", label: "When", render: (r) => <span className="whitespace-nowrap text-muted-foreground" title={date(r.at)}>{ago(r.at)}</span> },
              { key: "actor", label: "Actor", render: (r) => <span className="whitespace-nowrap text-[12.5px]">{r.actor}</span> },
              { key: "op", label: "Operation", render: (r) => <code className="text-[12.5px] font-semibold">{r.op}</code> },
              { key: "project", label: "Project", render: (r) => <span className="font-mono text-[12px]">{r.project}</span> },
              { key: "ok", label: "Result", render: (r) => (r.ok ? <Badge tone="success">ok</Badge> : <Badge tone="danger" title={r.error}>error</Badge>) },
              { key: "detail", label: "Details", render: (r) => <span className="block max-w-[340px] truncate font-mono text-[11.5px] text-muted-foreground">{JSON.stringify(r.detail)}</span> },
            ]}
          />
        )}
      </Card>
      <Dialog open={!!view} onClose={() => setView(null)} title={view?.op ?? ""} description={view ? `${view.actor} · ${date(view.at)}` : ""} size="lg">
        {view?.error && <ErrorBox error={new Error(view.error)} className="mb-3" />}
        <Code lang="json" code={pretty(view?.detail)} className="max-h-[60vh]" />
      </Dialog>
    </>
  );
}
