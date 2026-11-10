import { useQueryClient } from "@tanstack/react-query";
import { Download, FileCode2, KeyRound, RefreshCw, ShieldAlert, ShieldCheck } from "lucide-react";
import { useState } from "react";
import { useParams } from "react-router-dom";
import { Badge, Button, Callout, Card, CopyField, Dot, ErrorBox, Loading, PageHeader, Tabs, toast, useRunner } from "../components/ui";
import { Code, type CodeLang } from "../components/Code";
import { op, useOp } from "../lib/api";
import { RealtimeCard } from "./RealtimeCard";

interface ApiInfo {
  url: string;
  rest_url: string;
  anon_key: string;
  service_role_key?: string;
  rest: { available: boolean; running: boolean; version: string; idle_s?: number };
}
interface TableInfo {
  schema: string;
  name: string;
  kind: string;
  rls_enabled: boolean;
}

function Snippet({ code, lang }: { code: string; lang: CodeLang }) {
  return <Code code={code} lang={lang} className="max-h-[60vh] p-4 text-[12.5px]" />;
}

export function ProjectApi() {
  const { slug = "" } = useParams();
  const qc = useQueryClient();
  const run = useRunner();
  const api = useOp<ApiInfo>("projects.api", { project: slug }, { refetchInterval: 15_000 });
  const tables = useOp<TableInfo[]>("db.tables", { project: slug, schema: "public" });
  const [tab, setTab] = useState<"js" | "curl" | "env">("js");
  const [busy, setBusy] = useState("");

  if (api.error) return <ErrorBox error={api.error} />;
  if (!api.data) return <Loading />;
  const d = api.data;
  const t = (tables.data ?? []).find((x) => x.kind === "table")?.name ?? "todos";
  const open = (tables.data ?? []).filter((x) => x.kind === "table" && !x.rls_enabled);

  const snippets = {
    js: `import { createClient } from "@supabase/supabase-js";

const supabase = createClient(
  "${d.url}",
  "${d.anon_key}"
);

const { data, error } = await supabase
  .from("${t}")
  .select("*")
  .limit(10);`,
    curl: `curl "${d.rest_url}/${t}?select=*&limit=10" \\
  -H "apikey: ${d.anon_key}"`,
    env: `SUPABASE_URL=${d.url}
SUPABASE_ANON_KEY=${d.anon_key}${d.service_role_key ? `\n# server-side only - bypasses row level security\nSUPABASE_SERVICE_ROLE_KEY=${d.service_role_key}` : ""}`,
  };

  async function setRls(table: string, enabled: boolean) {
    setBusy(table);
    try {
      await op("db.set_rls", { project: slug, table, enabled });
      toast.success(`Row level security ${enabled ? "enabled" : "disabled"} on ${table}`);
      qc.invalidateQueries({ queryKey: ["op", "db.tables"] });
      qc.invalidateQueries({ queryKey: ["op", "system.health"] });
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy("");
    }
  }

  return (
    <>
      <PageHeader
        eyebrow="Connect"
        title="API"
        description="A Supabase-compatible REST API for this project. Use it from browsers, servers and supabase-js."
        actions={
          <span className="flex items-center gap-2 rounded-md border bg-card px-3 py-1.5 text-[12.5px]">
            <Dot tone={!d.rest.available ? "danger" : d.rest.running ? "success" : "neutral"} />
            {!d.rest.available ? "PostgREST not installed" : d.rest.running ? "REST running" : "REST idle - starts on first request"}
          </span>
        }
      />

      {!d.rest.available && (
        <Callout tone="danger" className="mb-5" title="PostgREST is not installed">
          macOS: <code>brew install postgrest</code>. Linux: put the static binary from github.com/PostgREST/postgrest/releases in /usr/local/bin.
        </Callout>
      )}

      <div className="grid gap-5 lg:grid-cols-2">
        <Card title="Project URL" description="Pass this to createClient(). REST lives at /rest/v1.">
          <CopyField value={d.url} />
          <div className="mt-3">
            <div className="label mb-1.5">REST endpoint</div>
            <CopyField value={d.rest_url} />
          </div>
        </Card>

        <Card
          title="API keys"
          description="Both are JWTs signed with this project's secret."
          actions={
            d.service_role_key && (
              <Button
                size="sm"
                variant="danger-outline"
                icon={<RefreshCw className="h-3.5 w-3.5" />}
                loading={busy === "rotate"}
                onClick={async () => {
                  setBusy("rotate");
                  try {
                    await run("projects.rotate_jwt_secret", { project: slug }, { title: "Rotate the JWT secret?", confirmLabel: "Rotate secret" });
                    toast.success("New keys generated - update your apps");
                    qc.invalidateQueries({ queryKey: ["op", "projects.api"] });
                  } catch (e) {
                    toast.error(e);
                  } finally {
                    setBusy("");
                  }
                }}
              >
                Rotate
              </Button>
            )
          }
        >
          <div className="space-y-4">
            <div>
              <div className="mb-1.5 flex items-center gap-2">
                <span className="font-mono text-[12.5px] font-semibold">anon</span>
                <Badge tone="success">public</Badge>
                <span className="text-[12px] text-muted-foreground">safe in browsers; row level security applies</span>
              </div>
              <CopyField value={d.anon_key} />
            </div>
            {d.service_role_key && (
              <div>
                <div className="mb-1.5 flex items-center gap-2">
                  <span className="font-mono text-[12.5px] font-semibold">service_role</span>
                  <Badge tone="danger">secret</Badge>
                  <span className="text-[12px] text-muted-foreground">bypasses row level security - servers only</span>
                </div>
                <CopyField value={d.service_role_key} secret />
              </div>
            )}
          </div>
        </Card>
      </div>

      <Card title="Quick start" className="mt-5">
        <Tabs
          tabs={[
            { id: "js", label: "supabase-js" },
            { id: "curl", label: "curl" },
            { id: "env", label: ".env" },
          ]}
          value={tab}
          onChange={setTab}
        />
        <Snippet code={snippets[tab]} lang={tab === "js" ? "ts" : "sh"} />
      </Card>

      <TypesCard slug={slug} />

      <RealtimeCard slug={slug} tables={tables.data ?? []} />

      <Card
        title={
          <span className="flex items-center gap-2">
            <KeyRound className="h-4 w-4 text-primary" /> Row level security
          </span>
        }
        description="Tables without RLS can be read and changed by anyone holding the anon key. Enable it, then add policies with a migration."
        className="mt-5"
        flush
      >
        {open.length > 0 && (
          <Callout tone="warning" className="mx-5 mt-4" title={`${open.length} table${open.length === 1 ? " is" : "s are"} open to the anon key`}>
            {open.map((x) => x.name).join(", ")}
          </Callout>
        )}
        {tables.isLoading ? (
          <Loading />
        ) : (
          <ul className="mt-2 divide-y">
            {(tables.data ?? [])
              .filter((x) => x.kind === "table")
              .map((x) => (
                <li key={x.name} className="flex items-center gap-3 px-5 py-3">
                  {x.rls_enabled ? <ShieldCheck className="h-4 w-4 text-success" /> : <ShieldAlert className="h-4 w-4 text-warning" />}
                  <span className="flex-1 font-mono text-[13px]">{x.name}</span>
                  {x.rls_enabled ? <Badge tone="success">RLS on</Badge> : <Badge tone="warning">RLS off</Badge>}
                  <Button size="sm" loading={busy === x.name} onClick={() => setRls(x.name, !x.rls_enabled)}>
                    {x.rls_enabled ? "Disable" : "Enable RLS"}
                  </Button>
                </li>
              ))}
            {!tables.data?.some((x) => x.kind === "table") && <li className="px-5 py-8 text-center text-[13px] text-muted-foreground">No tables in the public schema yet.</li>}
          </ul>
        )}
        <div className="border-t px-5 py-3 text-[12.5px] text-muted-foreground">
          Example policy: <code>create policy "read own" on todos for select to authenticated using (user_id = auth.uid());</code>
        </div>
      </Card>
    </>
  );
}

/** Generated TypeScript types for createClient<Database>(). */
function TypesCard({ slug }: { slug: string }) {
  const [ts, setTs] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const generate = async () => {
    setBusy(true);
    try {
      setTs((await op("db.types", { project: slug })).types);
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy(false);
    }
  };
  const download = () => {
    if (!ts) return;
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([ts], { type: "text/plain" }));
    a.download = "database.types.ts";
    a.click();
  };
  return (
    <Card
      title={<span className="flex items-center gap-2"><FileCode2 className="h-4 w-4 text-primary" /> TypeScript types</span>}
      description="Typed queries, inserts, updates and rpc calls: createClient<Database>(url, key). Regenerate after schema changes."
      className="mt-5"
      actions={
        <>
          {ts && <Button size="sm" icon={<Download className="h-3.5 w-3.5" />} onClick={download}>database.types.ts</Button>}
          <Button size="sm" variant="primary" loading={busy} onClick={generate}>{ts ? "Regenerate" : "Generate types"}</Button>
        </>
      }
    >
      {ts ? (
        <Snippet code={ts} lang="ts" />
      ) : (
        <p className="text-[13px] text-muted-foreground">
          Or from your repo: <code>ivarabase db types --project {slug} --out src/database.types.ts</code>
        </p>
      )}
    </Card>
  );
}
