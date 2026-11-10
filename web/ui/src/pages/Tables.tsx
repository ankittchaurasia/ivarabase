import { useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, ChevronLeft, ChevronRight, Filter, KeyRound, Link2, Plus, RefreshCw, Search, Table2, Trash2, X } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { createPortal } from "react-dom";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Badge, Button, Card, Checkbox, cn, DataTable, Empty, ErrorBox, Input, Loading, PageHeader, Select, Spinner, Tabs, Textarea, toast } from "../components/ui";
import { op, useOp, type Column } from "../lib/api";
import { bytes, cell } from "../lib/format";

interface TableInfo {
  schema: string;
  name: string;
  kind: string;
  rows_estimate: number;
  size_bytes: number;
  rls_enabled?: boolean;
}
const keyOf = (t: TableInfo) => (t.schema === "public" ? t.name : `${t.schema}.${t.name}`);

export function Tables() {
  const { slug = "", table } = useParams();
  const nav = useNavigate();
  const tables = useOp<TableInfo[]>("db.tables", { project: slug });
  const [q, setQ] = useState("");

  useEffect(() => {
    // open the first table by default on wide screens
    if (!table && tables.data?.length && window.innerWidth >= 1024) nav(`/p/${slug}/tables/${encodeURIComponent(keyOf(tables.data[0]))}`, { replace: true });
  }, [table, tables.data, slug, nav]);

  const filtered = (tables.data ?? []).filter((t) => keyOf(t).toLowerCase().includes(q.toLowerCase()));

  return (
    <>
      <PageHeader
        eyebrow="Database"
        title="Table editor"
        description="Browse, filter, sort and edit rows. Schema changes go through migrations."
        actions={
          <Link to={`/p/${slug}/migrations`}>
            <Button icon={<Plus className="h-4 w-4" />}>New table</Button>
          </Link>
        }
      />
      <div className="grid gap-5 lg:grid-cols-[250px_minmax(0,1fr)]">
        <Card flush className="h-fit lg:sticky lg:top-[76px]">
          <div className="border-b p-2.5">
            <div className="relative">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input className="h-8 pl-8 text-[13px]" placeholder="Search tables" value={q} onChange={(e) => setQ(e.target.value)} />
            </div>
          </div>
          <div className="scroll-thin max-h-[40vh] overflow-y-auto p-1.5 lg:max-h-[calc(100vh-220px)]">
            {tables.isLoading && <Loading />}
            <ErrorBox error={tables.error} />
            {tables.data && !tables.data.length && <p className="px-3 py-6 text-center text-[12.5px] text-muted-foreground">No tables yet.</p>}
            {filtered.map((t) => {
              const k = keyOf(t);
              return (
                <Link
                  key={k}
                  to={`/p/${slug}/tables/${encodeURIComponent(k)}`}
                  className={cn(
                    "flex items-center gap-2 rounded-md px-2.5 py-1.5 text-[13px] transition-colors",
                    k === table ? "bg-primary/10 font-semibold text-primary" : "text-foreground/80 hover:bg-accent/60",
                  )}
                >
                  <Table2 className="h-3.5 w-3.5 shrink-0 opacity-70" />
                  <span className="min-w-0 flex-1 truncate">{k}</span>
                  <span className="font-mono text-[10.5px] text-muted-foreground">{t.kind === "table" ? t.rows_estimate : t.kind.replace("_", " ")}</span>
                </Link>
              );
            })}
          </div>
        </Card>
        <div className="min-w-0">
          {table ? (
            <TableView key={slug + table} slug={slug} table={table} info={tables.data?.find((t) => keyOf(t) === table)} />
          ) : (
            <Card>
              <Empty icon={<Table2 className="h-5 w-5" />} title={tables.data?.length ? "Pick a table" : "No tables yet"}>
                {tables.data?.length ? "Choose a table on the left." : "Create one with a migration or the SQL editor."}
              </Empty>
            </Card>
          )}
        </div>
      </div>
    </>
  );
}

const OPS = ["eq", "neq", "lt", "lte", "gt", "gte", "like", "ilike", "is_null", "not_null"];

function TableView({ slug, table, info }: { slug: string; table: string; info?: TableInfo }) {
  const [schema, name] = table.includes(".") ? (table.split(".", 2) as [string, string]) : ["public", table];
  const [tab, setTab] = useState<"rows" | "columns">("rows");
  const [st, setSt] = useState({ limit: 50, offset: 0, order_by: "", desc: false, filter_column: "", filter_op: "eq", filter_value: "" });
  const [draft, setDraft] = useState({ column: "", op: "eq", value: "" });
  const [editing, setEditing] = useState<{ row: unknown[] | null } | null>(null);
  const args = useMemo(
    () => ({
      project: slug, table: name, schema, limit: st.limit, offset: st.offset, desc: st.desc,
      order_by: st.order_by || undefined,
      ...(st.filter_column ? { filter_column: st.filter_column, filter_op: st.filter_op, filter_value: st.filter_value } : {}),
    }),
    [slug, name, schema, st],
  );
  const rows = useOp<{ columns: Column[]; rows: unknown[][]; total: number; offset: number }>("db.rows", args, { placeholderData: (p) => p });
  const cols = rows.data?.columns ?? [];

  const sortBy = (c: string) => setSt((s) => ({ ...s, order_by: c, desc: s.order_by === c ? !s.desc : false, offset: 0 }));
  const from = rows.data?.rows.length ? rows.data.offset + 1 : 0;
  const to = (rows.data?.offset ?? 0) + (rows.data?.rows.length ?? 0);
  const total = rows.data?.total ?? -1;

  return (
    <Card flush>
      <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
        <h2 className="mr-auto flex items-center gap-2 font-mono text-[14px] font-semibold">
          {table}
          {info && <Badge>{bytes(info.size_bytes)}</Badge>}
          {info?.kind === "table" && (info.rls_enabled ? <Badge tone="success">RLS on</Badge> : <Link to={`/p/${slug}/api`}><Badge tone="warning">RLS off</Badge></Link>)}
          {rows.isFetching && <Spinner className="h-3.5 w-3.5" />}
        </h2>
        <Button size="sm" variant="ghost" icon={<RefreshCw className="h-3.5 w-3.5" />} onClick={() => rows.refetch()}>
          Refresh
        </Button>
        <Button size="sm" variant="primary" icon={<Plus className="h-3.5 w-3.5" />} onClick={() => setEditing({ row: null })} disabled={!cols.length}>
          Insert row
        </Button>
      </div>
      <div className="px-4 pt-2">
        <Tabs tabs={[{ id: "rows", label: "Data" }, { id: "columns", label: "Definition" }]} value={tab} onChange={setTab} />
      </div>

      {tab === "columns" ? (
        <ColumnsView cols={cols} />
      ) : (
        <>
          <form
            className="flex flex-wrap items-center gap-2 px-4 pb-3"
            onSubmit={(e) => {
              e.preventDefault();
              setSt((s) => ({ ...s, filter_column: draft.column, filter_op: draft.op, filter_value: draft.value, offset: 0 }));
            }}
          >
            <Filter className="h-3.5 w-3.5 text-muted-foreground" />
            <Select className="h-8 w-auto min-w-[140px] text-[12.5px]" value={draft.column} onChange={(e) => setDraft({ ...draft, column: e.target.value })}>
              <option value="">Column…</option>
              {cols.map((c) => (
                <option key={c.name}>{c.name}</option>
              ))}
            </Select>
            <Select className="h-8 w-auto text-[12.5px]" value={draft.op} onChange={(e) => setDraft({ ...draft, op: e.target.value })}>
              {OPS.map((o) => (
                <option key={o}>{o}</option>
              ))}
            </Select>
            {!draft.op.includes("null") && (
              <Input className="h-8 w-auto min-w-[140px] flex-1 text-[12.5px] sm:max-w-[240px]" placeholder="value" value={draft.value} onChange={(e) => setDraft({ ...draft, value: e.target.value })} />
            )}
            <Button size="sm" type="submit" disabled={!draft.column}>
              Apply
            </Button>
            {st.filter_column && (
              <Badge tone="primary" className="normal-case">
                {st.filter_column} {st.filter_op} {st.filter_op.includes("null") ? "" : st.filter_value}
                <button type="button" onClick={() => setSt((s) => ({ ...s, filter_column: "", offset: 0 }))} aria-label="Clear filter">
                  <X className="h-3 w-3" />
                </button>
              </Badge>
            )}
          </form>
          <ErrorBox error={rows.error} className="mx-4 mb-3" />
          {rows.isLoading ? (
            <Loading />
          ) : rows.data && !rows.data.rows.length ? (
            <div className="border-t px-4 py-12 text-center text-[13px] text-muted-foreground">No rows{st.filter_column ? " match this filter" : ""}.</div>
          ) : (
            <div className="scroll-thin max-h-[62vh] overflow-auto border-t">
              <table className="w-full border-collapse font-mono text-[12.5px]">
                <thead>
                  <tr>
                    {cols.map((c) => (
                      <th
                        key={c.name}
                        onClick={() => sortBy(c.name)}
                        title={c.type}
                        className="sticky top-0 z-[1] cursor-pointer select-none whitespace-nowrap border-b border-r bg-muted/80 px-3 py-2 text-left font-sans text-[12px] font-semibold backdrop-blur last:border-r-0 hover:bg-accent"
                      >
                        <span className="flex items-center gap-1.5">
                          {c.primary_key && <KeyRound className="h-3 w-3 text-warning" />}
                          {c.references && <Link2 className="h-3 w-3 text-info" />}
                          {c.name}
                          <span className="font-mono text-[10.5px] font-normal text-muted-foreground">{c.type.split("(")[0]}</span>
                          {st.order_by === c.name && (st.desc ? <ArrowDown className="h-3 w-3" /> : <ArrowUp className="h-3 w-3" />)}
                        </span>
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {rows.data?.rows.map((r, i) => (
                    <tr key={i} onClick={() => setEditing({ row: r })} className="cursor-pointer border-b transition-colors hover:bg-primary/[0.05]">
                      {r.map((v, j) => (
                        <td key={j} title={cell(v)} className={cn("max-w-[280px] truncate border-r px-3 py-[7px] last:border-r-0", v === null && "italic text-muted-foreground/70")}>
                          {cell(v)}
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div className="flex flex-wrap items-center justify-between gap-2 border-t px-4 py-2.5 text-[12.5px] text-muted-foreground">
            <span>
              {from}–{to}
              {total >= 0 && ` of ${total.toLocaleString()}`} rows
            </span>
            <div className="flex items-center gap-2">
              <Select className="h-7 w-auto py-0 text-[12px]" value={st.limit} onChange={(e) => setSt((s) => ({ ...s, limit: +e.target.value, offset: 0 }))}>
                {[25, 50, 100, 250, 500].map((n) => (
                  <option key={n} value={n}>
                    {n} / page
                  </option>
                ))}
              </Select>
              <Button size="sm" variant="secondary" disabled={st.offset === 0} onClick={() => setSt((s) => ({ ...s, offset: Math.max(0, s.offset - s.limit) }))} aria-label="Previous page">
                <ChevronLeft className="h-3.5 w-3.5" />
              </Button>
              <Button
                size="sm"
                variant="secondary"
                disabled={total >= 0 ? to >= total : (rows.data?.rows.length ?? 0) < st.limit}
                onClick={() => setSt((s) => ({ ...s, offset: s.offset + s.limit }))}
                aria-label="Next page"
              >
                <ChevronRight className="h-3.5 w-3.5" />
              </Button>
            </div>
          </div>
        </>
      )}
      {editing && <RowSheet slug={slug} schema={schema} table={name} cols={cols} row={editing.row} onClose={() => setEditing(null)} />}
    </Card>
  );
}

function ColumnsView({ cols }: { cols: Column[] }) {
  return (
    <DataTable
      columns={[
        { key: "name", label: "Column", render: (c) => <span className="font-mono font-semibold">{c.name}</span> },
        { key: "type", label: "Type", render: (c) => <code className="text-info">{c.type}</code> },
        { key: "nullable", label: "Nullable", render: (c) => (c.nullable ? "yes" : <span className="text-muted-foreground">no</span>) },
        { key: "default", label: "Default", render: (c) => <code className="text-[11.5px] text-muted-foreground">{c.default}</code> },
        {
          key: "keys",
          label: "Keys",
          render: (c) => (
            <span className="flex gap-1">
              {c.primary_key && <Badge tone="warning">primary</Badge>}
              {c.references && <Badge tone="info">→ {c.references}</Badge>}
              {c.generated && <Badge>generated</Badge>}
            </span>
          ),
        },
      ]}
      rows={cols}
      rowKey={(c) => c.name}
    />
  );
}

/** Right-hand panel for inserting or editing one row (Supabase style). */
function RowSheet({ slug, schema, table, cols, row, onClose }: { slug: string; schema: string; table: string; cols: Column[]; row: unknown[] | null; onClose: () => void }) {
  const qc = useQueryClient();
  const editing = row !== null;
  const init = () =>
    Object.fromEntries(
      cols.map((c, i) => {
        const v = row ? row[i] : null;
        const text = v === null || v === undefined ? "" : typeof v === "object" ? JSON.stringify(v, null, 2) : String(v);
        return [c.name, { text, isNull: editing && v === null, orig: v }];
      }),
    );
  const [vals, setVals] = useState<Record<string, { text: string; isNull: boolean; orig: unknown }>>(init);
  const [busy, setBusy] = useState(false);
  const hasPk = cols.some((c) => c.primary_key);
  const key = () => Object.fromEntries(cols.flatMap((c, i) => (c.primary_key && row ? [[c.name, row[i]]] : [])));
  const isJson = (c: Column) => /json/.test(c.type);

  useEffect(() => {
    const esc = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    document.addEventListener("keydown", esc);
    return () => document.removeEventListener("keydown", esc);
  }, [onClose]);

  const done = () => {
    qc.invalidateQueries({ queryKey: ["op", "db.rows"] });
    onClose();
  };

  async function save() {
    setBusy(true);
    try {
      const values: Record<string, unknown> = {};
      for (const c of cols) {
        const f = vals[c.name];
        let v: unknown = f.isNull ? null : f.text;
        if (!f.isNull && isJson(c) && f.text.trim()) {
          try {
            v = JSON.parse(f.text);
          } catch {
            throw new Error(`${c.name}: invalid JSON`);
          }
        }
        if (editing) {
          const same = isJson(c) ? JSON.stringify(v) === JSON.stringify(f.orig) : v === null ? f.orig === null : f.orig !== null && String(f.orig) === v;
          if (!same) values[c.name] = v;
        } else if (!(v === "" && !f.isNull)) values[c.name] = v;
      }
      if (!Object.keys(values).length) {
        toast.info("Nothing changed");
        return;
      }
      if (editing) await op("db.row_update", { project: slug, schema, table, key: key(), values });
      else await op("db.row_insert", { project: slug, schema, table, values });
      toast.success(editing ? "Row updated" : "Row inserted");
      done();
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    if (!confirm("Delete this row?")) return;
    setBusy(true);
    try {
      await op("db.row_delete", { project: slug, schema, table, key: key() });
      toast.success("Row deleted");
      done();
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy(false);
    }
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex justify-end bg-black/30 backdrop-blur-[1px]" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="flex h-full w-full max-w-[520px] animate-fade-in flex-col border-l bg-card shadow-pop">
        <div className="flex items-center gap-2 border-b px-5 py-4">
          <div className="min-w-0 flex-1">
            <div className="label">{editing ? "Edit row" : "Insert row"}</div>
            <div className="truncate font-mono text-[14px] font-semibold">{table}</div>
          </div>
          <Button variant="ghost" size="icon" onClick={onClose} aria-label="Close">
            <X className="h-4 w-4" />
          </Button>
        </div>
        <div className="scroll-thin flex-1 space-y-4 overflow-y-auto px-5 py-4">
          {cols.map((c) => {
            const f = vals[c.name];
            const set = (patch: Partial<typeof f>) => setVals((s) => ({ ...s, [c.name]: { ...s[c.name], ...patch } }));
            return (
              <div key={c.name}>
                <div className="mb-1.5 flex items-center gap-2">
                  <span className="font-mono text-[12.5px] font-semibold">{c.name}</span>
                  {c.primary_key && <KeyRound className="h-3 w-3 text-warning" />}
                  <code className="text-[11px] text-muted-foreground">{c.type}</code>
                  {c.nullable && (
                    <span className="ml-auto">
                      <Checkbox label={<span className="text-[12px] text-muted-foreground">NULL</span>} checked={f.isNull} onChange={(e) => set({ isNull: e.target.checked })} />
                    </span>
                  )}
                </div>
                {isJson(c) ? (
                  <Textarea rows={4} disabled={f.isNull} value={f.text} onChange={(e) => set({ text: e.target.value })} />
                ) : (
                  <Input
                    disabled={f.isNull}
                    value={f.text}
                    placeholder={c.default ? `default: ${c.default}` : c.nullable ? "NULL" : ""}
                    onChange={(e) => set({ text: e.target.value })}
                    className="font-mono text-[13px]"
                  />
                )}
              </div>
            );
          })}
          {editing && !hasPk && <ErrorBox error={new Error("This table has no primary key, so rows can only be changed with SQL.")} />}
        </div>
        <div className="flex items-center gap-2 border-t bg-muted/30 px-5 py-3">
          {editing && hasPk && (
            <Button variant="danger-outline" icon={<Trash2 className="h-3.5 w-3.5" />} onClick={remove} disabled={busy}>
              Delete
            </Button>
          )}
          <span className="flex-1" />
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={save} loading={busy} disabled={editing && !hasPk}>
            {editing ? "Save" : "Insert"}
          </Button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
