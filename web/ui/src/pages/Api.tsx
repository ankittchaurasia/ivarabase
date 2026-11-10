import { Bot, ExternalLink, Search, Terminal } from "lucide-react";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { Badge, Button, Card, CopyField, ErrorBox, Input, Loading, PageHeader } from "../components/ui";
import { Code } from "../components/Code";
import { http } from "../lib/api";
import { useQuery } from "@tanstack/react-query";

interface OpDesc {
  name: string;
  summary: string;
  description: string;
  tag: string;
  method: string;
  path: string;
  level: string;
  destructive: boolean;
  may_be_destructive: boolean;
  params: { name: string; type: string; required?: boolean; description?: string }[];
}

const methodTone: Record<string, string> = {
  GET: "text-info bg-info/10",
  POST: "text-success bg-success/10",
  PUT: "text-warning bg-warning/10",
  PATCH: "text-warning bg-warning/10",
  DELETE: "text-destructive bg-destructive/10",
};

export function Api() {
  const ops = useQuery<OpDesc[]>({ queryKey: ["ops-catalog"], queryFn: () => http("GET", "/api/v1/ops") });
  const [q, setQ] = useState("");
  const url = location.origin;
  const groups = useMemo(() => {
    const g: Record<string, OpDesc[]> = {};
    for (const o of ops.data ?? []) if (!q || (o.name + " " + o.summary).toLowerCase().includes(q.toLowerCase())) (g[o.tag] ||= []).push(o);
    return g;
  }, [ops.data, q]);

  return (
    <>
      <PageHeader
        eyebrow="Developers"
        title="API & MCP"
        description={`${ops.data?.length ?? "…"} operations in one registry. REST, POST /api/v1/ops/{name}, MCP tools, the CLI and this UI all run the same code.`}
        actions={
          <>
            <a href="/api/v1/openapi.json" target="_blank" rel="noreferrer">
              <Button icon={<ExternalLink className="h-3.5 w-3.5" />}>OpenAPI</Button>
            </a>
            <Link to="/tokens">
              <Button variant="primary">Create a token</Button>
            </Link>
          </>
        }
      />
      <div className="grid gap-5 lg:grid-cols-2">
        <Card title={<span className="flex items-center gap-2"><Bot className="h-4 w-4 text-primary" /> Connect an AI agent</span>} description="MCP over Streamable HTTP.">
          <div className="space-y-2">
            <CopyField value={`claude mcp add --transport http ivarabase ${url}/mcp --header "Authorization: Bearer <token>"`} />
            <p className="text-[12.5px] text-muted-foreground">
              stdio-only clients: <code>ivarabase mcp --url {url} --token &lt;token&gt;</code>
            </p>
          </div>
        </Card>
        <Card title={<span className="flex items-center gap-2"><Terminal className="h-4 w-4 text-primary" /> REST and CLI</span>}>
          <div className="space-y-2">
            <CopyField value={`curl -H "Authorization: Bearer <token>" ${url}/api/v1/projects`} />
            <CopyField value={`ivarabase login --url ${url} --token <token>`} />
          </div>
        </Card>
      </div>

      <Card className="mt-5" title="Config as code" description="Keep a project's migrations, functions, secrets, auth settings and buckets in your repo.">
        <div className="grid gap-4 lg:grid-cols-2">
          <div className="space-y-2">
            <CopyField value="ivarabase export --project <slug>" />
            <CopyField value="ivarabase plan" />
            <CopyField value="ivarabase apply" />
            <p className="text-[12.5px] text-muted-foreground">
              <code>export</code> writes ivarabase.yaml, migrations/ and functions/. <code>plan</code> shows the difference, <code>apply</code> makes the project match (add <code>--prune</code> to
              delete what is gone - tokens without delete rights get an approval request). Secrets are <code>{"${NAME}"}</code> placeholders read from the environment or a .env file.
            </p>
          </div>
          <Code lang="yaml" code={`project: shop
migrations: migrations        # *.sql, file-name order
functions_dir: functions      # one folder per function
functions:
  stripe-webhook: { verify_jwt: false }
  nightly: { schedule: "0 3 * * *" }
secrets:
  STRIPE_KEY: \${STRIPE_KEY}
auth:
  site_url: https://myapp.com
  confirm_email: true
storage:
  buckets:
    avatars: { public: true, file_size_limit: 1MB }`} />
        </div>
      </Card>

      <div className="relative mt-6">
        <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
        <Input className="h-10 pl-9" placeholder="Search operations…" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>

      {ops.isLoading && <Loading />}
      <ErrorBox error={ops.error} className="mt-4" />
      <div className="mt-5 space-y-5">
        {Object.entries(groups).map(([tag, items]) => (
          <Card key={tag} title={<span className="capitalize">{tag}</span>} flush>
            <ul className="divide-y">
              {items.map((o) => (
                <li key={o.name} className="px-5 py-3.5">
                  <div className="flex flex-wrap items-center gap-2">
                    <code className="text-[13px] font-semibold">{o.name}</code>
                    <span className={`rounded px-1.5 py-0.5 font-mono text-[10.5px] font-bold ${methodTone[o.method]}`}>{o.method}</span>
                    <code className="text-[11.5px] text-muted-foreground">{o.path}</code>
                    <Badge>{o.level}</Badge>
                    {o.destructive ? <Badge tone="danger">destructive</Badge> : o.may_be_destructive ? <Badge tone="warning">may be destructive</Badge> : null}
                  </div>
                  <div className="mt-1 text-[13px]">{o.summary}</div>
                  {o.description && <div className="mt-0.5 text-[12.5px] text-muted-foreground">{o.description}</div>}
                  {(o.params ?? []).length > 0 && (
                    <div className="mt-2 flex flex-wrap gap-1.5">
                      {(o.params ?? []).map((p) => (
                        <span key={p.name} title={p.description} className="rounded border bg-muted/40 px-1.5 py-0.5 font-mono text-[11px]">
                          {p.name}
                          <span className="text-muted-foreground">: {p.type}</span>
                          {p.required && <span className="text-destructive">*</span>}
                        </span>
                      ))}
                    </div>
                  )}
                </li>
              ))}
            </ul>
          </Card>
        ))}
      </div>
    </>
  );
}
