import { useQueryClient } from "@tanstack/react-query";
import {
  ChevronRight,
  Download,
  File as FileGlyph,
  FileImage,
  FileText,
  Folder,
  FolderPlus,
  Globe,
  HardDrive,
  Link2,
  Lock,
  Plus,
  RefreshCw,
  Search,
  Settings2,
  Trash2,
  Upload,
  X,
} from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { useParams, useSearchParams } from "react-router-dom";
import { Badge, Button, Callout, Card, Checkbox, cn, CopyField, Dialog, Empty, ErrorBox, Field, Input, Loading, PageHeader, Select, Spinner, toast, useRunner } from "../components/ui";
import { op, useOp } from "../lib/api";
import { ago, bytes, date } from "../lib/format";

interface BucketRow {
  id: string;
  public: boolean;
  objects: number;
  size_bytes: number;
  file_size_limit: number | null;
  allowed_mime_types: string[] | null;
}
interface Entry {
  name: string;
  id: string | null;
  updated_at: string | null;
  metadata: { size?: number; mimetype?: string } | null;
}

const PLACEHOLDER = ".emptyFolderPlaceholder";
const fileURL = (slug: string, bucket: string, path: string, dl = false) =>
  `/api/v1/projects/${slug}/storage/download?bucket=${encodeURIComponent(bucket)}&path=${encodeURIComponent(path)}${dl ? "&download" : ""}`;

function fileIcon(e: Entry) {
  const m = e.metadata?.mimetype ?? "";
  if (m.startsWith("image/")) return <FileImage className="h-4 w-4 text-info" />;
  if (m.startsWith("text/") || m.includes("json") || m.includes("pdf")) return <FileText className="h-4 w-4 text-warning" />;
  return <FileGlyph className="h-4 w-4 text-muted-foreground" />;
}

/** Upload with progress (XHR), through the dashboard's storage endpoint. */
function uploadFile(slug: string, bucket: string, path: string, file: File, onProgress: (p: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `/api/v1/projects/${slug}/storage/upload?bucket=${encodeURIComponent(bucket)}&path=${encodeURIComponent(path)}&upsert=true`);
    xhr.setRequestHeader("X-Ivara", "1");
    xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      if (xhr.status < 300) return resolve();
      let msg = xhr.statusText;
      try {
        msg = JSON.parse(xhr.responseText).message ?? JSON.parse(xhr.responseText).error?.message ?? msg;
      } catch {}
      reject(new Error(msg));
    };
    xhr.onerror = () => reject(new Error("network error"));
    xhr.send(file);
  });
}

export function Storage() {
  const { slug = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const buckets = useOp<BucketRow[]>("storage.buckets_list", { project: slug });
  const bucket = params.get("bucket") ?? buckets.data?.[0]?.id ?? "";
  const prefix = params.get("prefix") ?? "";
  const [creating, setCreating] = useState(false);
  const [settings, setSettings] = useState(false);
  const go = (b: string, p = "") => setParams(p ? { bucket: b, prefix: p } : { bucket: b });
  const current = buckets.data?.find((b) => b.id === bucket);

  return (
    <>
      <PageHeader
        eyebrow="Storage"
        title="Storage"
        description="Files for your app, served by the Supabase-compatible storage API. Access is decided by row level security on storage.objects."
        actions={
          <Button icon={<Settings2 className="h-4 w-4" />} onClick={() => setSettings(true)}>
            Storage settings
          </Button>
        }
      />
      <div className="grid gap-5 lg:grid-cols-[240px_minmax(0,1fr)]">
        <Card flush className="h-fit lg:sticky lg:top-[76px]">
          <div className="flex items-center justify-between border-b px-4 py-3">
            <span className="label">Buckets</span>
            <Button size="sm" variant="ghost" icon={<Plus className="h-3.5 w-3.5" />} onClick={() => setCreating(true)}>
              New
            </Button>
          </div>
          <div className="p-1.5">
            {buckets.isLoading && <Loading />}
            <ErrorBox error={buckets.error} />
            {buckets.data?.length === 0 && <p className="px-3 py-6 text-center text-[12.5px] text-muted-foreground">No buckets yet.</p>}
            {buckets.data?.map((b) => (
              <button
                key={b.id}
                onClick={() => go(b.id)}
                className={cn(
                  "flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left text-[13px] transition-colors",
                  b.id === bucket ? "bg-primary/10 font-semibold text-primary" : "text-foreground/80 hover:bg-accent/60",
                )}
              >
                {b.public ? <Globe className="h-3.5 w-3.5 shrink-0 opacity-70" /> : <Lock className="h-3.5 w-3.5 shrink-0 opacity-70" />}
                <span className="min-w-0 flex-1 truncate">{b.id}</span>
                <span className="font-mono text-[10.5px] text-muted-foreground">{b.objects}</span>
              </button>
            ))}
          </div>
        </Card>
        <div className="min-w-0">
          {current ? (
            <Browser key={slug + bucket} slug={slug} bucket={current} prefix={prefix} go={go} />
          ) : (
            !buckets.isLoading && (
              <Card>
                <Empty
                  icon={<HardDrive className="h-5 w-5" />}
                  title="Create your first bucket"
                  action={<Button variant="primary" icon={<Plus className="h-4 w-4" />} onClick={() => setCreating(true)}>New bucket</Button>}
                >
                  A bucket groups files, e.g. <code>avatars</code> (public) or <code>documents</code> (private).
                </Empty>
              </Card>
            )
          )}
        </div>
      </div>
      {creating && <BucketDialog slug={slug} onClose={() => setCreating(false)} onDone={(id) => go(id)} />}
      {settings && <StorageSettings onClose={() => setSettings(false)} />}
    </>
  );
}

function BucketDialog({ slug, bucket, onClose, onDone }: { slug: string; bucket?: BucketRow; onClose: () => void; onDone?: (id: string) => void }) {
  const qc = useQueryClient();
  const [id, setId] = useState(bucket?.id ?? "");
  const [pub, setPub] = useState(bucket?.public ?? false);
  const [limit, setLimit] = useState(bucket?.file_size_limit ? bytes(bucket.file_size_limit).replace(" ", "") : "");
  const [mimes, setMimes] = useState((bucket?.allowed_mime_types ?? []).join(", "));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  async function save() {
    setBusy(true);
    setError(null);
    try {
      await op(bucket ? "storage.bucket_update" : "storage.bucket_create", {
        project: slug, bucket: id.trim(), public: pub, file_size_limit: limit.trim(),
        allowed_mime_types: mimes.split(",").map((s) => s.trim()).filter(Boolean),
      });
      toast.success(bucket ? "Bucket updated" : `Bucket ${id} created`);
      qc.invalidateQueries({ queryKey: ["op", "storage.buckets_list"] });
      onDone?.(id.trim());
      onClose();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Dialog
      open
      onClose={onClose}
      title={bucket ? `Bucket ${bucket.id}` : "New bucket"}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} onClick={save} disabled={!id.trim()}>
            {bucket ? "Save" : "Create bucket"}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        {!bucket && (
          <Field label="Name" hint="Letters, numbers, dots, hyphens, underscores">
            <Input autoFocus value={id} onChange={(e) => setId(e.target.value)} placeholder="avatars" />
          </Field>
        )}
        <Checkbox checked={pub} onChange={(e) => setPub(e.target.checked)} label="Public bucket" hint="Anyone with the URL can download files. Uploads and deletes still follow your policies." />
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Max file size" hint="e.g. 5MB - empty for the server limit">
            <Input value={limit} onChange={(e) => setLimit(e.target.value)} placeholder="5MB" />
          </Field>
          <Field label="Allowed types" hint="Comma separated, e.g. image/*, application/pdf">
            <Input value={mimes} onChange={(e) => setMimes(e.target.value)} placeholder="any" />
          </Field>
        </div>
        {!bucket && (
          <Callout tone="neutral">
            Clients need policies on <code>storage.objects</code> to read or upload, e.g.{" "}
            <code>create policy "read" on storage.objects for select to anon using (bucket_id = '{id || "avatars"}');</code>
          </Callout>
        )}
        <ErrorBox error={error} />
      </div>
    </Dialog>
  );
}

function Browser({ slug, bucket, prefix, go }: { slug: string; bucket: BucketRow; prefix: string; go: (b: string, p?: string) => void }) {
  const qc = useQueryClient();
  const run = useRunner();
  const [search, setSearch] = useState("");
  const list = useOp<Entry[]>("storage.objects_list", { project: slug, bucket: bucket.id, prefix, search: search || undefined, limit: 1000 }, { placeholderData: (p) => p });
  const [uploads, setUploads] = useState<{ name: string; progress: number; error?: string }[]>([]);
  const [drag, setDrag] = useState(false);
  const [open, setOpen] = useState<Entry | null>(null);
  const [editing, setEditing] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const crumbs = prefix ? prefix.replace(/\/$/, "").split("/") : [];
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["op", "storage.objects_list"] });
    qc.invalidateQueries({ queryKey: ["op", "storage.buckets_list"] });
  };

  async function uploadAll(files: FileList | File[]) {
    const arr = Array.from(files);
    setUploads(arr.map((f) => ({ name: f.name, progress: 0 })));
    let failed = 0;
    for (let i = 0; i < arr.length; i++) {
      const f = arr[i];
      try {
        await uploadFile(slug, bucket.id, prefix + f.name, f, (p) => setUploads((u) => u.map((x, j) => (j === i ? { ...x, progress: p } : x))));
        setUploads((u) => u.map((x, j) => (j === i ? { ...x, progress: 1 } : x)));
      } catch (e) {
        failed++;
        setUploads((u) => u.map((x, j) => (j === i ? { ...x, error: (e as Error).message } : x)));
      }
    }
    refresh();
    if (!failed) {
      toast.success(`Uploaded ${arr.length} file${arr.length === 1 ? "" : "s"}`);
      setTimeout(() => setUploads([]), 1500);
    }
  }

  async function newFolder() {
    const name = prompt("Folder name")?.trim().replace(/\//g, "");
    if (!name) return;
    await uploadFile(slug, bucket.id, `${prefix}${name}/${PLACEHOLDER}`, new File([""], PLACEHOLDER, { type: "text/plain" }), () => {}).catch((e) => toast.error(e));
    go(bucket.id, `${prefix}${name}/`);
    refresh();
  }

  const entries = (list.data ?? []).filter((e) => e.name !== PLACEHOLDER);

  return (
    <Card flush>
      <div
        className="relative"
        onDragOver={(e) => {
          e.preventDefault();
          setDrag(true);
        }}
        onDragLeave={(e) => e.currentTarget === e.target && setDrag(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDrag(false);
          if (e.dataTransfer.files.length) uploadAll(e.dataTransfer.files);
        }}
      >
        <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
          <div className="flex min-w-0 flex-1 items-center gap-1 text-[13.5px]">
            <button onClick={() => go(bucket.id)} className="flex items-center gap-1.5 font-semibold hover:text-primary">
              {bucket.public ? <Globe className="h-3.5 w-3.5" /> : <Lock className="h-3.5 w-3.5" />}
              {bucket.id}
            </button>
            {crumbs.map((c, i) => (
              <span key={i} className="flex min-w-0 items-center gap-1">
                <ChevronRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                <button className="truncate hover:text-primary" onClick={() => go(bucket.id, crumbs.slice(0, i + 1).join("/") + "/")}>
                  {c}
                </button>
              </span>
            ))}
            {list.isFetching && <Spinner className="ml-2 h-3.5 w-3.5" />}
          </div>
          <Badge tone={bucket.public ? "warning" : "neutral"}>{bucket.public ? "public" : "private"}</Badge>
          <span className="text-[12px] text-muted-foreground">
            {bucket.objects} files · {bytes(bucket.size_bytes)}
          </span>
        </div>
        <div className="flex flex-wrap items-center gap-2 px-4 py-2.5">
          <div className="relative min-w-[160px] flex-1 sm:max-w-[260px]">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input className="h-8 pl-8 text-[13px]" placeholder="Search in folder" value={search} onChange={(e) => setSearch(e.target.value)} />
          </div>
          <span className="flex-1" />
          <Button size="sm" variant="ghost" icon={<RefreshCw className="h-3.5 w-3.5" />} onClick={refresh}>Refresh</Button>
          <Button size="sm" variant="ghost" icon={<Settings2 className="h-3.5 w-3.5" />} onClick={() => setEditing(true)}>Bucket</Button>
          <Button size="sm" icon={<FolderPlus className="h-3.5 w-3.5" />} onClick={newFolder}>Folder</Button>
          <Button size="sm" variant="primary" icon={<Upload className="h-3.5 w-3.5" />} onClick={() => fileInput.current?.click()}>Upload</Button>
          <input ref={fileInput} type="file" multiple hidden onChange={(e) => e.target.files?.length && uploadAll(e.target.files)} />
        </div>
        {uploads.length > 0 && (
          <div className="space-y-1.5 border-t bg-muted/30 px-4 py-3">
            {uploads.map((u, i) => (
              <div key={i} className="flex items-center gap-3 text-[12.5px]">
                <span className="w-48 truncate">{u.name}</span>
                <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
                  <div className={cn("h-full transition-all", u.error ? "bg-destructive" : "bg-primary")} style={{ width: `${Math.round(u.progress * 100)}%` }} />
                </div>
                <span className={cn("w-40 truncate text-right", u.error ? "text-destructive" : "text-muted-foreground")}>{u.error ?? `${Math.round(u.progress * 100)}%`}</span>
              </div>
            ))}
          </div>
        )}
        <ErrorBox error={list.error} className="mx-4 mb-3" />
        {list.isLoading ? (
          <Loading />
        ) : !entries.length ? (
          <div className="border-t px-6 py-14 text-center">
            <Upload className="mx-auto h-6 w-6 text-muted-foreground" />
            <div className="mt-2 font-medium">{search ? "No matches" : "Drop files here"}</div>
            <div className="text-[12.5px] text-muted-foreground">or use the Upload button</div>
          </div>
        ) : (
          <div className="scroll-thin max-h-[62vh] overflow-auto border-t">
            <table className="w-full border-collapse text-[13px]">
              <thead>
                <tr>
                  {["Name", "Size", "Type", "Updated"].map((h) => (
                    <th key={h} className="sticky top-0 border-b bg-muted/80 px-4 py-2 text-left font-mono text-[10.5px] font-medium uppercase tracking-[0.1em] text-muted-foreground backdrop-blur">
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {entries.map((e) => (
                  <tr
                    key={e.name + (e.id ?? "/")}
                    className="cursor-pointer border-b transition-colors hover:bg-primary/[0.05]"
                    onClick={() => (e.id ? setOpen(e) : go(bucket.id, prefix + e.name + "/"))}
                  >
                    <td className="px-4 py-2.5">
                      <span className="flex items-center gap-2.5">
                        {e.id ? fileIcon(e) : <Folder className="h-4 w-4 fill-warning/20 text-warning" />}
                        <span className="truncate">{e.name}</span>
                      </span>
                    </td>
                    <td className="whitespace-nowrap px-4 py-2.5 font-mono text-[12px] text-muted-foreground">{e.id ? bytes(e.metadata?.size) : ""}</td>
                    <td className="px-4 py-2.5 text-[12px] text-muted-foreground">{e.metadata?.mimetype ?? (e.id ? "" : "folder")}</td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-[12px] text-muted-foreground">{e.updated_at ? ago(e.updated_at) : ""}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {drag && (
          <div className="pointer-events-none absolute inset-0 grid place-items-center rounded-lg border-2 border-dashed border-primary bg-primary/[0.06]">
            <div className="flex items-center gap-2 font-medium text-primary">
              <Upload className="h-5 w-5" /> Drop to upload into {bucket.id}/{prefix}
            </div>
          </div>
        )}
      </div>
      <div className="flex items-center justify-between border-t px-4 py-2.5">
        <span className="text-[12px] text-muted-foreground">{entries.length} items</span>
        <div className="flex gap-2">
          <Button
            size="sm"
            variant="ghost"
            onClick={async () => {
              try {
                await run("storage.bucket_empty", { project: slug, bucket: bucket.id }, { title: `Empty ${bucket.id}?`, confirmLabel: "Delete all files" });
                toast.success("Bucket emptied");
                refresh();
              } catch (e) {
                toast.error(e);
              }
            }}
          >
            Empty bucket
          </Button>
          <Button
            size="sm"
            variant="danger-outline"
            icon={<Trash2 className="h-3.5 w-3.5" />}
            onClick={async () => {
              try {
                await run("storage.bucket_delete", { project: slug, bucket: bucket.id }, { title: `Delete ${bucket.id}?`, confirmLabel: "Delete bucket" });
                toast.success("Bucket deleted");
                qc.invalidateQueries({ queryKey: ["op", "storage.buckets_list"] });
                go("");
              } catch (e) {
                toast.error(e);
              }
            }}
          >
            Delete bucket
          </Button>
        </div>
      </div>
      {open && <FileSheet slug={slug} bucket={bucket} prefix={prefix} entry={open} onClose={() => setOpen(null)} onChanged={refresh} />}
      {editing && <BucketDialog slug={slug} bucket={bucket} onClose={() => setEditing(false)} />}
    </Card>
  );
}

function FileSheet({ slug, bucket, prefix, entry, onClose, onChanged }: { slug: string; bucket: BucketRow; prefix: string; entry: Entry; onClose: () => void; onChanged: () => void }) {
  const run = useRunner();
  const path = prefix + entry.name;
  const [signed, setSigned] = useState<string | null>(null);
  const [expiry, setExpiry] = useState(3600);
  const [publicUrl, setPublicUrl] = useState<string | null>(null);
  const [newPath, setNewPath] = useState(path);
  const [busy, setBusy] = useState("");
  const mime = entry.metadata?.mimetype ?? "";

  useEffect(() => {
    const esc = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    document.addEventListener("keydown", esc);
    if (bucket.public) op("storage.signed_url", { project: slug, bucket: bucket.id, path, expires_in: 60 }).then((r) => setPublicUrl(r.public_url)).catch(() => {});
    return () => document.removeEventListener("keydown", esc);
  }, [onClose, bucket, slug, path]);

  const row = (k: string, v: ReactNode) => (
    <div className="rounded-md border bg-muted/40 px-3 py-2">
      <div className="label">{k}</div>
      <div className="mt-0.5 truncate text-[13px]">{v}</div>
    </div>
  );

  return createPortal(
    <div className="fixed inset-0 z-50 flex justify-end bg-black/30 backdrop-blur-[1px]" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="flex h-full w-full max-w-[520px] animate-fade-in flex-col border-l bg-card shadow-pop">
        <div className="flex items-center gap-2 border-b px-5 py-4">
          <div className="min-w-0 flex-1">
            <div className="label">{bucket.id}</div>
            <div className="truncate text-[15px] font-semibold">{entry.name}</div>
          </div>
          <Button variant="ghost" size="icon" onClick={onClose} aria-label="Close">
            <X className="h-4 w-4" />
          </Button>
        </div>
        <div className="scroll-thin flex-1 space-y-5 overflow-y-auto px-5 py-4">
          <div className="grid h-56 place-items-center overflow-hidden rounded-md border bg-muted/30">
            {mime.startsWith("image/") ? (
              <img src={fileURL(slug, bucket.id, path)} alt={entry.name} className="max-h-full max-w-full object-contain" />
            ) : (
              <div className="flex flex-col items-center gap-2 text-muted-foreground">
                {fileIcon(entry)}
                <span className="text-[12px]">No preview</span>
              </div>
            )}
          </div>
          <div className="grid grid-cols-2 gap-3">
            {row("Size", bytes(entry.metadata?.size))}
            {row("Type", mime || "–")}
            {row("Updated", entry.updated_at ? date(entry.updated_at) : "–")}
            {row("Path", <code className="text-[12px]">{path}</code>)}
          </div>
          {publicUrl && (
            <div>
              <div className="label mb-1.5">Public URL</div>
              <CopyField value={publicUrl} />
            </div>
          )}
          <div>
            <div className="label mb-1.5">Signed URL (works for private buckets)</div>
            <div className="flex gap-2">
              <Select className="w-[140px]" value={expiry} onChange={(e) => setExpiry(+e.target.value)}>
                {[[3600, "1 hour"], [86400, "1 day"], [604800, "1 week"], [2592000, "30 days"]].map(([v, l]) => (
                  <option key={v} value={v}>{l}</option>
                ))}
              </Select>
              <Button
                icon={<Link2 className="h-3.5 w-3.5" />}
                loading={busy === "sign"}
                onClick={async () => {
                  setBusy("sign");
                  try {
                    setSigned((await op("storage.signed_url", { project: slug, bucket: bucket.id, path, expires_in: expiry })).signed_url);
                  } catch (e) {
                    toast.error(e);
                  } finally {
                    setBusy("");
                  }
                }}
              >
                Create link
              </Button>
            </div>
            {signed && <CopyField value={signed} className="mt-2" />}
          </div>
          <div>
            <div className="label mb-1.5">Rename / move</div>
            <div className="flex gap-2">
              <Input className="font-mono text-[12.5px]" value={newPath} onChange={(e) => setNewPath(e.target.value)} />
              <Button
                loading={busy === "move"}
                disabled={newPath === path || !newPath.trim()}
                onClick={async () => {
                  setBusy("move");
                  try {
                    await op("storage.object_move", { project: slug, bucket: bucket.id, from: path, to: newPath.trim() });
                    toast.success("Moved");
                    onChanged();
                    onClose();
                  } catch (e) {
                    toast.error(e);
                  } finally {
                    setBusy("");
                  }
                }}
              >
                Move
              </Button>
            </div>
          </div>
        </div>
        <div className="flex items-center gap-2 border-t bg-muted/30 px-5 py-3">
          <Button
            variant="danger-outline"
            icon={<Trash2 className="h-3.5 w-3.5" />}
            onClick={async () => {
              try {
                await run("storage.object_delete", { project: slug, bucket: bucket.id, paths: [path] }, { title: `Delete ${entry.name}?`, confirmLabel: "Delete file" });
                toast.success("Deleted");
                onChanged();
                onClose();
              } catch (e) {
                toast.error(e);
              }
            }}
          >
            Delete
          </Button>
          <span className="flex-1" />
          <a href={fileURL(slug, bucket.id, path, true)}>
            <Button variant="primary" icon={<Download className="h-3.5 w-3.5" />}>Download</Button>
          </a>
        </div>
      </div>
    </div>,
    document.body,
  );
}

function StorageSettings({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const cfg = useOp<any>("storage.config_get");
  const [f, setF] = useState<any>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (cfg.data) {
      const c = cfg.data.config;
      setF({ backend: c.backend, max_file_size_mb: c.max_file_size_mb, s3_endpoint: c.s3.endpoint, s3_region: c.s3.region, s3_bucket: c.s3.bucket, s3_prefix: c.s3.prefix, s3_access_key: c.s3.access_key, s3_secret_key: c.s3.secret_key });
    }
  }, [cfg.data]);
  const s = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => setF({ ...f, [k]: e.target.type === "number" ? +e.target.value : e.target.value });
  return (
    <Dialog
      open
      onClose={onClose}
      title="Storage settings"
      description="Applies to every project on this server."
      size="lg"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            loading={busy}
            disabled={!f}
            onClick={async () => {
              setBusy(true);
              try {
                await op("storage.config_set", f);
                toast.success("Storage settings saved");
                qc.invalidateQueries({ queryKey: ["op", "storage.config_get"] });
                onClose();
              } catch (e) {
                toast.error(e);
              } finally {
                setBusy(false);
              }
            }}
          >
            Save
          </Button>
        </>
      }
    >
      {!f ? (
        <Loading />
      ) : (
        <div className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Where files are stored">
              <Select value={f.backend} onChange={s("backend")}>
                <option value="local">This server's disk</option>
                <option value="s3">S3-compatible bucket (B2, R2, S3…)</option>
              </Select>
            </Field>
            <Field label="Max upload size (MB)">
              <Input type="number" min={1} value={f.max_file_size_mb} onChange={s("max_file_size_mb")} />
            </Field>
          </div>
          {f.backend === "local" ? (
            <Callout tone="neutral">
              Files live in <code>{cfg.data.local_dir}</code> and are included in project backups.
            </Callout>
          ) : (
            <>
              <Callout tone="warning">
                Switching does not move existing files. With S3 storage, backups contain the database but not the files (they already live in the bucket).
              </Callout>
              <div className="grid gap-3 sm:grid-cols-2">
                <Field label="Endpoint" className="sm:col-span-2"><Input value={f.s3_endpoint} onChange={s("s3_endpoint")} placeholder="https://s3.us-west-004.backblazeb2.com" /></Field>
                <Field label="Bucket"><Input value={f.s3_bucket} onChange={s("s3_bucket")} /></Field>
                <Field label="Region"><Input value={f.s3_region} onChange={s("s3_region")} placeholder="us-west-004" /></Field>
                <Field label="Prefix"><Input value={f.s3_prefix} onChange={s("s3_prefix")} placeholder="ivarabase/" /></Field>
                <Field label="Access key"><Input value={f.s3_access_key} onChange={s("s3_access_key")} autoComplete="off" /></Field>
                <Field label="Secret key" className="sm:col-span-2"><Input type="password" value={f.s3_secret_key} onChange={s("s3_secret_key")} autoComplete="off" /></Field>
              </div>
            </>
          )}
        </div>
      )}
    </Dialog>
  );
}
