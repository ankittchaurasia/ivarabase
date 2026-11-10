import { Download, History, Lock, Play, Unlock } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { Editor } from "../components/Editor";
import { Badge, Button, Card, cn, ErrorBox, Kbd, PageHeader, Select, Spinner, Switch, useRunner } from "../components/ui";
import { isCancelled, useOp, type QueryResult } from "../lib/api";
import { cell } from "../lib/format";

const SNIPPETS: Record<string, string> = {
  "List tables": "select table_schema, table_name\nfrom information_schema.tables\nwhere table_schema not in ('pg_catalog', 'information_schema')\norder by 1, 2;",
  "Table sizes": "select relname as table, pg_size_pretty(pg_total_relation_size(oid)) as size\nfrom pg_class where relkind = 'r' and relnamespace = 'public'::regnamespace\norder by pg_total_relation_size(oid) desc;",
  "Active queries": "select pid, state, now() - query_start as running_for, left(query, 120) as query\nfrom pg_stat_activity where datname = current_database();",
};

export function SqlEditor() {
  const { slug = "" } = useParams();
  const run = useRunner();
  const histKey = "ivara.sql." + slug;
  const [history, setHistory] = useState<string[]>(() => {
    try {
      return JSON.parse(localStorage.getItem(histKey) || "[]");
    } catch {
      return [];
    }
  });
  const [sql, setSql] = useState(history[0] ?? "select now();");
  const [readOnly, setReadOnly] = useState(true);
  const [limit, setLimit] = useState(500);
  const [result, setResult] = useState<QueryResult | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const schema = useOp<Record<string, { name: string }[]>>("db.schema", { project: slug }, { staleTime: 30_000 });
  const cmSchema = useMemo(() => (schema.data ? Object.fromEntries(Object.entries(schema.data).map(([t, cols]) => [t, cols.map((c) => c.name)])) : undefined), [schema.data]);

  const execute = useCallback(async () => {
    if (!sql.trim() || busy) return;
    setBusy(true);
    setError(null);
    const next = [sql, ...history.filter((h) => h !== sql)].slice(0, 30);
    setHistory(next);
    try {
      localStorage.setItem(histKey, JSON.stringify(next));
    } catch {}
    try {
      setResult(await run<QueryResult>("db.query", { project: slug, sql, read_only: readOnly, limit }, { title: "Run destructive SQL?", confirmLabel: "Run it" }));
    } catch (e) {
      if (!isCancelled(e)) {
        setError(e);
        setResult(null);
      }
    } finally {
      setBusy(false);
    }
  }, [sql, busy, history, histKey, run, slug, readOnly, limit]);

  useEffect(() => setResult(null), [slug]);

  return (
    <>
      <PageHeader eyebrow="Database" title="SQL editor" description="Runs as the project's own role. Read-only until you unlock writes." />
      <Card flush>
        <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2">
          <Button variant="primary" size="sm" icon={busy ? <Spinner className="h-3.5 w-3.5 text-primary-foreground" /> : <Play className="h-3.5 w-3.5" />} onClick={execute} disabled={busy}>
            Run
          </Button>
          <Kbd>⌘ ↵</Kbd>
          <div className="mx-1 h-5 w-px bg-border" />
          <Switch
            checked={!readOnly}
            onChange={(v) => setReadOnly(!v)}
            label={
              <span className={cn("flex items-center gap-1 text-[12.5px]", readOnly ? "text-muted-foreground" : "font-medium text-warning")}>
                {readOnly ? <Lock className="h-3.5 w-3.5" /> : <Unlock className="h-3.5 w-3.5" />}
                {readOnly ? "Read-only" : "Writes enabled"}
              </span>
            }
          />
          <span className="flex-1" />
          <Select className="h-7 w-auto py-0 text-[12px]" value="" onChange={(e) => e.target.value && setSql(SNIPPETS[e.target.value])}>
            <option value="">Snippets</option>
            {Object.keys(SNIPPETS).map((k) => (
              <option key={k}>{k}</option>
            ))}
          </Select>
          <Select className="h-7 w-auto max-w-[220px] py-0 text-[12px]" value="" onChange={(e) => e.target.value !== "" && setSql(history[+e.target.value])} aria-label="History">
            <option value="">History ({history.length})</option>
            {history.map((q, i) => (
              <option key={i} value={i}>
                {q.replace(/\s+/g, " ").slice(0, 60)}
              </option>
            ))}
          </Select>
          <Select className="h-7 w-auto py-0 text-[12px]" value={limit} onChange={(e) => setLimit(+e.target.value)}>
            {[100, 500, 1000, 5000].map((n) => (
              <option key={n} value={n}>
                {n} rows
              </option>
            ))}
          </Select>
        </div>
        <Editor lang="sql" value={sql} onChange={setSql} onRun={execute} schema={cmSchema} className="h-[38vh] min-h-[220px] rounded-none border-0" />
      </Card>

      <div className="mt-4">
        <ErrorBox error={error} />
        {result && <ResultView r={result} />}
        {!result && !error && (
          <div className="flex items-center gap-2 px-1 py-6 text-[13px] text-muted-foreground">
            <History className="h-4 w-4" /> Results appear here. Table and column names autocomplete as you type.
          </div>
        )}
      </div>
    </>
  );
}

function ResultView({ r }: { r: QueryResult }) {
  const csv = () => {
    const esc = (v: unknown) => {
      const s = v === null ? "" : typeof v === "object" ? JSON.stringify(v) : String(v);
      return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
    };
    const text = [r.columns.map((c) => esc(c.name)).join(","), ...r.rows.map((row) => row.map(esc).join(","))].join("\n");
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([text], { type: "text/csv" }));
    a.download = "result.csv";
    a.click();
  };
  if (!r.columns.length)
    return (
      <Card>
        <div className="flex items-center gap-2 text-[13px]">
          <Badge tone="success">ok</Badge> Statement executed in {r.ms} ms.
        </div>
      </Card>
    );
  return (
    <Card flush>
      <div className="flex items-center gap-2 border-b px-4 py-2 text-[12.5px] text-muted-foreground">
        <Badge tone="success">{r.row_count} rows</Badge>
        {r.truncated && <Badge tone="warning">truncated</Badge>}
        <span>{r.ms} ms</span>
        <span className="flex-1" />
        <Button size="sm" variant="ghost" icon={<Download className="h-3.5 w-3.5" />} onClick={csv}>
          CSV
        </Button>
      </div>
      <div className="scroll-thin max-h-[52vh] overflow-auto">
        <table className="w-full border-collapse font-mono text-[12.5px]">
          <thead>
            <tr>
              {r.columns.map((c, i) => (
                <th key={i} title={c.type} className="sticky top-0 whitespace-nowrap border-b border-r bg-muted/80 px-3 py-2 text-left font-sans text-[12px] font-semibold backdrop-blur last:border-r-0">
                  {c.name} <span className="font-mono text-[10.5px] font-normal text-muted-foreground">{c.type}</span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {r.rows.map((row, i) => (
              <tr key={i} className="border-b hover:bg-accent/40">
                {row.map((v, j) => (
                  <td key={j} title={cell(v)} className={cn("max-w-[320px] truncate border-r px-3 py-[6px] last:border-r-0", v === null && "italic text-muted-foreground/70")}>
                    {cell(v)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}
