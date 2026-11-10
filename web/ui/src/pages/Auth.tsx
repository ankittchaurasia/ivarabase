import { useQueryClient } from "@tanstack/react-query";
import { Apple, Ban, CheckCircle2, ExternalLink, Github, Inbox, KeyRound, Link2, LogOut, Mail, MessageSquare, Plus, Search, Send, ShieldCheck, Smartphone, Trash2, UserPlus, UserRound, Users, X } from "lucide-react";
import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { useParams, useSearchParams } from "react-router-dom";
import {
  Badge,
  Button,
  Callout,
  Card,
  Checkbox,
  cn,
  CopyField,
  DataTable,
  Dialog,
  Empty,
  ErrorBox,
  Field,
  Input,
  Loading,
  PageHeader,
  Switch,
  Tabs,
  Textarea,
  toast,
  useRunner,
} from "../components/ui";
import { Code } from "../components/Code";
import { isCancelled, op, useOp } from "../lib/api";
import { ago, date, pretty } from "../lib/format";

type Tab = "users" | "providers" | "urls" | "emails" | "mailbox" | "sms" | "logs";

export function Auth() {
  const { slug = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") as Tab) || "users";
  const cfg = useOp<any>("auth.config_get", { project: slug });
  return (
    <>
      <PageHeader
        eyebrow="Authentication"
        title="Authentication"
        description="Sign-up and sign-in for your app's users: email and password, magic links, phone codes, anonymous users, MFA, Google, GitHub, Apple and any OpenID Connect provider. Works with supabase-js auth."
      />
      <Tabs
        tabs={[
          { id: "users", label: "Users" },
          { id: "providers", label: "Sign-in methods" },
          { id: "urls", label: "URL configuration" },
          { id: "emails", label: "Emails" },
          { id: "mailbox", label: "Mailbox" },
          { id: "sms", label: "SMS log" },
          { id: "logs", label: "Logs" },
        ]}
        value={tab}
        onChange={(t) => setParams({ tab: t }, { replace: true })}
      />
      {cfg.error && <ErrorBox error={cfg.error} />}
      {tab === "users" && <UsersTab slug={slug} />}
      {tab !== "users" && tab !== "mailbox" && tab !== "sms" && tab !== "logs" && !cfg.data && !cfg.error && <Loading />}
      {tab === "providers" && cfg.data && <ProvidersTab slug={slug} data={cfg.data} />}
      {tab === "urls" && cfg.data && <UrlsTab slug={slug} data={cfg.data} />}
      {tab === "emails" && cfg.data && <EmailsTab slug={slug} data={cfg.data} />}
      {tab === "mailbox" && <MailboxTab slug={slug} smtp={!!cfg.data?.config?.smtp?.host} />}
      {tab === "sms" && <SmsTab slug={slug} provider={cfg.data?.config?.phone?.provider ?? ""} />}
      {tab === "logs" && <LogsTab slug={slug} />}
    </>
  );
}

// ------------------------------------------------------------------ users

function providersOf(u: any): string[] {
  return u.app_metadata?.providers ?? [u.app_metadata?.provider ?? "email"];
}

function UsersTab({ slug }: { slug: string }) {
  const [q, setQ] = useState("");
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(1);
  const users = useOp<{ users: any[]; total: number }>("auth.users_list", { project: slug, search: search || undefined, page, per_page: 50 }, { placeholderData: (p) => p });
  const [open, setOpen] = useState<string | null>(null);
  const [adding, setAdding] = useState<"create" | "invite" | null>(null);

  useEffect(() => {
    const t = setTimeout(() => {
      setSearch(q.trim());
      setPage(1);
    }, 300);
    return () => clearTimeout(t);
  }, [q]);

  const total = users.data?.total ?? 0;
  return (
    <>
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <div className="relative min-w-[220px] flex-1 sm:max-w-sm">
          <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input className="pl-9" placeholder="Search by email, phone or user id" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <span className="flex-1" />
        <Button icon={<Send className="h-4 w-4" />} onClick={() => setAdding("invite")}>
          Invite
        </Button>
        <Button variant="primary" icon={<UserPlus className="h-4 w-4" />} onClick={() => setAdding("create")}>
          Add user
        </Button>
      </div>
      <Card flush>
        {users.isLoading ? (
          <Loading />
        ) : users.error ? (
          <ErrorBox error={users.error} className="m-4" />
        ) : !users.data?.users.length ? (
          <Empty icon={<Users className="h-5 w-5" />} title={search ? "No users match" : "No users yet"}>
            {search ? "Try another search." : "Users appear here when they sign up through your app, or add one yourself."}
          </Empty>
        ) : (
          <DataTable
            rows={users.data.users}
            rowKey={(u) => u.id}
            onRowClick={(u) => setOpen(u.id)}
            columns={[
              {
                key: "email",
                label: "User",
                render: (u) => (
                  <div className="flex items-center gap-3">
                    <span className="grid h-8 w-8 shrink-0 place-items-center rounded-full bg-primary/10 font-mono text-[11px] font-semibold uppercase text-primary">
                      {(u.user_metadata?.name || u.email || "?").slice(0, 2)}
                    </span>
                    <div className="min-w-0">
                      <div className="truncate font-medium">{u.email || (u.phone ? "+" + u.phone : u.is_anonymous ? "Anonymous user" : "")}</div>
                      <div className="truncate font-mono text-[11px] text-muted-foreground">{u.id}</div>
                    </div>
                  </div>
                ),
              },
              { key: "prov", label: "Providers", render: (u) => <div className="flex gap-1">{providersOf(u).map((p) => <Badge key={p}>{p}</Badge>)}</div> },
              {
                key: "state",
                label: "Status",
                render: (u) =>
                  u.banned_until && new Date(u.banned_until) > new Date() ? (
                    <Badge tone="danger">banned</Badge>
                  ) : u.email_confirmed_at ? (
                    <Badge tone="success">confirmed</Badge>
                  ) : u.invited_at ? (
                    <Badge tone="info">invited</Badge>
                  ) : (
                    <Badge tone="warning">unconfirmed</Badge>
                  ),
              },
              { key: "created", label: "Created", render: (u) => <span className="whitespace-nowrap text-muted-foreground" title={date(u.created_at)}>{ago(u.created_at)}</span> },
              { key: "last", label: "Last sign-in", render: (u) => <span className="whitespace-nowrap text-muted-foreground">{ago(u.last_sign_in_at)}</span> },
            ]}
          />
        )}
        {total > 50 && (
          <div className="flex items-center justify-between border-t px-4 py-2.5 text-[12.5px] text-muted-foreground">
            <span>{total} users</span>
            <div className="flex gap-2">
              <Button size="sm" disabled={page === 1} onClick={() => setPage(page - 1)}>Prev</Button>
              <Button size="sm" disabled={page * 50 >= total} onClick={() => setPage(page + 1)}>Next</Button>
            </div>
          </div>
        )}
      </Card>
      {open && <UserSheet slug={slug} id={open} onClose={() => setOpen(null)} />}
      {adding && <AddUserDialog slug={slug} mode={adding} onClose={() => setAdding(null)} />}
    </>
  );
}

function AddUserDialog({ slug, mode, onClose }: { slug: string; mode: "create" | "invite"; onClose: () => void }) {
  const qc = useQueryClient();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  async function go() {
    setBusy(true);
    setError(null);
    try {
      if (mode === "invite") {
        await op("auth.send_link", { project: slug, email, type: "invite" });
        toast.success(`Invite sent to ${email}`);
      } else {
        await op("auth.user_create", { project: slug, email, password: password || undefined, email_confirm: confirm });
        toast.success(`Created ${email}`);
      }
      qc.invalidateQueries({ queryKey: ["op", "auth.users_list"] });
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
      title={mode === "invite" ? "Invite a user" : "Add a user"}
      description={mode === "invite" ? "They get an email with a link to set up their account." : "Create the account directly."}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} onClick={go} disabled={!email}>
            {mode === "invite" ? "Send invite" : "Create user"}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <Field label="Email">
          <Input type="email" autoFocus value={email} onChange={(e) => setEmail(e.target.value)} />
        </Field>
        {mode === "create" && (
          <>
            <Field label="Password" hint="Leave empty for a passwordless user (magic link or OAuth only)">
              <Input type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />
            </Field>
            <Checkbox checked={confirm} onChange={(e) => setConfirm(e.target.checked)} label="Mark email as confirmed" />
          </>
        )}
        <ErrorBox error={error} />
      </div>
    </Dialog>
  );
}

function UserSheet({ slug, id, onClose }: { slug: string; id: string; onClose: () => void }) {
  const qc = useQueryClient();
  const run = useRunner();
  const u = useOp<any>("auth.user_get", { project: slug, id });
  const [busy, setBusy] = useState("");
  useEffect(() => {
    const esc = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    document.addEventListener("keydown", esc);
    return () => document.removeEventListener("keydown", esc);
  }, [onClose]);

  const act = async (key: string, fn: () => Promise<unknown>, msg: string) => {
    setBusy(key);
    try {
      await fn();
      toast.success(msg);
      qc.invalidateQueries({ queryKey: ["op", "auth.users_list"] });
      qc.invalidateQueries({ queryKey: ["op", "auth.user_get"] });
    } catch (e) {
      if (!isCancelled(e)) toast.error(e);
    } finally {
      setBusy("");
    }
  };
  const d = u.data;
  const banned = d?.banned_until && new Date(d.banned_until) > new Date();

  return createPortal(
    <div className="fixed inset-0 z-50 flex justify-end bg-black/30 backdrop-blur-[1px]" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="flex h-full w-full max-w-[560px] animate-fade-in flex-col border-l bg-card shadow-pop">
        <div className="flex items-center gap-2 border-b px-5 py-4">
          <div className="min-w-0 flex-1">
            <div className="label">User</div>
            <div className="truncate text-[15px] font-semibold">{d?.email ?? "…"}</div>
          </div>
          <Button variant="ghost" size="icon" onClick={onClose} aria-label="Close">
            <X className="h-4 w-4" />
          </Button>
        </div>
        <div className="scroll-thin flex-1 space-y-5 overflow-y-auto px-5 py-4">
          {u.isLoading && <Loading />}
          <ErrorBox error={u.error} />
          {d && (
            <>
              <dl className="grid grid-cols-2 gap-3 text-[13px]">
                {[
                  ["User ID", <code key="i" className="break-all text-[11.5px]">{d.id}</code>],
                  ["Status", banned ? <Badge tone="danger">banned until {date(d.banned_until)}</Badge> : d.email_confirmed_at ? <Badge tone="success">confirmed</Badge> : <Badge tone="warning">unconfirmed</Badge>],
                  ["Created", date(d.created_at)],
                  ["Last sign-in", d.last_sign_in_at ? date(d.last_sign_in_at) : "never"],
                  ["Providers", providersOf(d).join(", ")],
                  ["Pending email", d.new_email ?? "–"],
                  ["Phone", d.phone ? "+" + d.phone + (d.phone_confirmed_at ? "" : " (unconfirmed)") : "–"],
                  ["MFA", d.factors?.filter((f: any) => f.status === "verified").length ? `${d.factors.filter((f: any) => f.status === "verified").length} authenticator app(s)` : d.is_anonymous ? "anonymous user" : "off"],
                ].map(([k, v], i) => (
                  <div key={i} className="rounded-md border bg-muted/40 px-3 py-2">
                    <dt className="label">{k}</dt>
                    <dd className="mt-0.5">{v}</dd>
                  </div>
                ))}
              </dl>

              <div>
                <div className="label mb-2">Actions</div>
                <div className="flex flex-wrap gap-2">
                  {!d.email_confirmed_at && (
                    <Button size="sm" icon={<CheckCircle2 className="h-3.5 w-3.5" />} loading={busy === "confirm"} onClick={() => act("confirm", () => op("auth.user_update", { project: slug, id, email_confirm: true }), "Email confirmed")}>
                      Confirm email
                    </Button>
                  )}
                  <Button size="sm" icon={<Link2 className="h-3.5 w-3.5" />} loading={busy === "magic"} onClick={() => act("magic", () => op("auth.send_link", { project: slug, email: d.email, type: "magiclink" }), "Magic link sent")}>
                    Send magic link
                  </Button>
                  <Button size="sm" icon={<Mail className="h-3.5 w-3.5" />} loading={busy === "rec"} onClick={() => act("rec", () => op("auth.send_link", { project: slug, email: d.email, type: "recovery" }), "Recovery email sent")}>
                    Send password reset
                  </Button>
                  {d.factors?.length > 0 && (
                    <Button
                      size="sm"
                      icon={<ShieldCheck className="h-3.5 w-3.5" />}
                      loading={busy === "mfa"}
                      onClick={() =>
                        act("mfa", () => run("auth.user_mfa_reset", { project: slug, id }, { title: "Remove this user's MFA factors?", confirmLabel: "Remove factors" }), "MFA factors removed")
                      }
                    >
                      Reset MFA
                    </Button>
                  )}
                  <Button size="sm" icon={<LogOut className="h-3.5 w-3.5" />} loading={busy === "out"} onClick={() => act("out", () => op("auth.user_signout", { project: slug, id }), "Signed out everywhere")}>
                    Sign out everywhere
                  </Button>
                  {banned ? (
                    <Button size="sm" loading={busy === "ban"} onClick={() => act("ban", () => op("auth.user_update", { project: slug, id, ban_duration: "none" }), "User unbanned")}>
                      Unban
                    </Button>
                  ) : (
                    <Button size="sm" variant="danger-outline" icon={<Ban className="h-3.5 w-3.5" />} loading={busy === "ban"} onClick={() => act("ban", () => op("auth.user_update", { project: slug, id, ban_duration: "876000h" }), "User banned and signed out")}>
                      Ban
                    </Button>
                  )}
                </div>
              </div>

              <div>
                <div className="label mb-2">Sessions ({d.sessions?.length ?? 0})</div>
                {d.sessions?.length ? (
                  <ul className="divide-y rounded-md border">
                    {d.sessions.map((s: any) => (
                      <li key={s.id} className="px-3 py-2 text-[12.5px]">
                        <div className="flex items-center gap-2">
                          <Badge>{s.method}</Badge>
                          <span className="text-muted-foreground">active {ago(s.last_active_at)}</span>
                          <span className="ml-auto font-mono text-[11px] text-muted-foreground">{s.ip}</span>
                        </div>
                        <div className="mt-0.5 truncate text-[11.5px] text-muted-foreground">{s.user_agent}</div>
                      </li>
                    ))}
                  </ul>
                ) : (
                  <p className="text-[12.5px] text-muted-foreground">No active sessions.</p>
                )}
              </div>

              <div>
                <div className="label mb-2">User metadata</div>
                <Code lang="json" code={pretty(d.user_metadata)} className="max-h-48" />
              </div>
              <div>
                <div className="label mb-2">App metadata</div>
                <Code lang="json" code={pretty(d.app_metadata)} className="max-h-48" />
              </div>
              <div>
                <div className="label mb-2">Identities</div>
                <ul className="space-y-1.5">
                  {d.identities?.map((i: any) => (
                    <li key={i.identity_id} className="flex items-center gap-2 text-[12.5px]">
                      <Badge>{i.provider}</Badge>
                      <span className="truncate">{i.email}</span>
                      <span className="ml-auto text-muted-foreground">{ago(i.created_at)}</span>
                    </li>
                  ))}
                </ul>
              </div>
            </>
          )}
        </div>
        <div className="flex items-center border-t bg-muted/30 px-5 py-3">
          <Button
            variant="danger-outline"
            icon={<Trash2 className="h-3.5 w-3.5" />}
            disabled={!d}
            onClick={async () => {
              try {
                await run("auth.user_delete", { project: slug, id }, { title: `Delete ${d?.email}?`, confirmLabel: "Delete user" });
                toast.success("User deleted");
                qc.invalidateQueries({ queryKey: ["op", "auth.users_list"] });
                onClose();
              } catch (e) {
                toast.error(e);
              }
            }}
          >
            Delete user
          </Button>
          <span className="flex-1" />
          <Button onClick={onClose}>Close</Button>
        </div>
      </div>
    </div>,
    document.body,
  );
}

// ------------------------------------------------------------------ settings tabs

function useSaver(slug: string) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const save = async (args: Record<string, unknown>, msg = "Saved") => {
    setBusy(true);
    try {
      await op("auth.config_set", { project: slug, ...args });
      toast.success(msg);
      qc.invalidateQueries({ queryKey: ["op", "auth.config_get"] });
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy(false);
    }
  };
  return { save, busy };
}

function ProvidersTab({ slug, data }: { slug: string; data: any }) {
  const c = data.config;
  const { save, busy } = useSaver(slug);
  const [f, setF] = useState({
    disable_signup: c.disable_signup, confirm_email: c.confirm_email, password_min_length: c.password_min_length, jwt_expiry: c.jwt_expiry,
    google_enabled: c.google.enabled, google_client_id: c.google.client_id, google_client_secret: c.google.client_secret,
    github_enabled: c.github.enabled, github_client_id: c.github.client_id, github_client_secret: c.github.client_secret,
    apple_enabled: c.apple?.enabled ?? false, apple_client_id: c.apple?.client_id ?? "", apple_team_id: c.apple?.team_id ?? "",
    apple_key_id: c.apple?.key_id ?? "", apple_private_key: c.apple?.private_key ?? "", apple_bundle_ids: (c.apple?.bundle_ids ?? []).join("\n"),
    anonymous_users: !!c.anonymous_users,
    phone_enabled: !!c.phone?.enabled, phone_autoconfirm: !!c.phone?.autoconfirm, sms_provider: c.phone?.provider ?? "",
    twilio_account_sid: c.phone?.twilio_account_sid ?? "", twilio_auth_token: c.phone?.twilio_auth_token ?? "", twilio_from: c.phone?.twilio_from ?? "",
    twilio_messaging_service_sid: c.phone?.twilio_messaging_service_sid ?? "", sms_webhook_url: c.phone?.webhook_url ?? "", sms_webhook_secret: c.phone?.webhook_secret ?? "",
    sms_template: c.phone?.template ?? "", sms_otp_expiry: c.phone?.otp_expiry ?? 300,
    sms_test_otp: Object.entries(c.phone?.test_otp ?? {}).map(([k, v]) => `+${k}=${v}`).join("\n"),
    mfa_totp_enabled: !c.mfa?.disable_totp, mfa_issuer: c.mfa?.issuer ?? "",
    oidc_providers: (c.oidc ?? []) as { name: string; label: string; enabled: boolean; issuer: string; client_id: string; client_secret: string; scopes: string }[],
  });
  const payload = () => ({
    ...f,
    apple_bundle_ids: f.apple_bundle_ids.split(/[\s,]+/).filter(Boolean),
    sms_test_otp: Object.fromEntries(
      f.sms_test_otp
        .split("\n")
        .map((l) => l.split("=").map((x) => x.trim()))
        .filter((p) => p.length === 2 && p[0] && p[1]),
    ),
  });
  const setOidc = (i: number, k: string, v: unknown) => set("oidc_providers", f.oidc_providers.map((p, j) => (j === i ? { ...p, [k]: v } : p)));
  const publicCallback = /^https:\/\//.test(data.callback_url) && !/\/\/(localhost|127\.|\[::1\])/.test(data.callback_url);
  const set = (k: string, v: unknown) => setF({ ...f, [k]: v });
  // a plain function (not a component) so inputs keep focus while typing
  const oauth = ({ id, name, icon, consoleUrl }: { id: "google" | "github"; name: string; icon: React.ReactNode; consoleUrl: string }) => (
    <Card
      title={<span className="flex items-center gap-2">{icon} {name}</span>}
      actions={<Switch checked={(f as any)[id + "_enabled"]} onChange={(v) => set(id + "_enabled", v)} label={<span className="text-[12.5px]">{(f as any)[id + "_enabled"] ? "Enabled" : "Disabled"}</span>} />}
    >
      <div className="space-y-3">
        <Field label="Client ID">
          <Input value={(f as any)[id + "_client_id"]} onChange={(e) => set(id + "_client_id", e.target.value)} />
        </Field>
        <Field label="Client secret">
          <Input type="password" autoComplete="off" value={(f as any)[id + "_client_secret"]} onChange={(e) => set(id + "_client_secret", e.target.value)} />
        </Field>
        <div>
          <div className="label mb-1.5">Callback URL - paste into {name}</div>
          <CopyField value={data.callback_url} />
        </div>
        <a href={consoleUrl} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-[12.5px] text-primary hover:underline">
          Create credentials <ExternalLink className="h-3 w-3" />
        </a>
      </div>
    </Card>
  );
  return (
    <div className="space-y-5">
      <Card title={<span className="flex items-center gap-2"><Mail className="h-4 w-4 text-primary" /> Email</span>} description="Email + password, magic links and 6-digit codes are always available.">
        <div className="space-y-4">
          <Switch checked={!f.disable_signup} onChange={(v) => set("disable_signup", !v)} label="Allow new users to sign up" />
          <Switch checked={f.confirm_email} onChange={(v) => set("confirm_email", v)} label="Require email confirmation before password sign-in" />
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Minimum password length">
              <Input type="number" min={6} max={72} value={f.password_min_length} onChange={(e) => set("password_min_length", +e.target.value)} />
            </Field>
            <Field label="Access token lifetime (seconds)" hint="Refresh tokens keep users signed in; this is how often clients refresh">
              <Input type="number" min={300} value={f.jwt_expiry} onChange={(e) => set("jwt_expiry", +e.target.value)} />
            </Field>
          </div>
        </div>
      </Card>
      <div className="grid gap-5 lg:grid-cols-2">
        {oauth({ id: "google", name: "Google", consoleUrl: "https://console.cloud.google.com/apis/credentials", icon: <span className="grid h-5 w-5 place-items-center rounded bg-muted text-[12px] font-bold">G</span> })}
        {oauth({ id: "github", name: "GitHub", consoleUrl: "https://github.com/settings/developers", icon: <Github className="h-4 w-4" /> })}
        <Card
          className="lg:col-span-2"
          title={<span className="flex items-center gap-2"><Apple className="h-4 w-4" /> Apple</span>}
          description="Web sign-in redirects to Apple. iOS and macOS apps sign in natively with signInWithIdToken and only need their bundle ID."
          actions={<Switch checked={f.apple_enabled} onChange={(v) => set("apple_enabled", v)} label={<span className="text-[12.5px]">{f.apple_enabled ? "Enabled" : "Disabled"}</span>} />}
        >
          <div className="grid gap-5 lg:grid-cols-2">
            <div className="space-y-3">
              <div className="label">Web sign-in</div>
              <Field label="Services ID" hint="Identifiers → Services IDs, e.g. com.example.web">
                <Input value={f.apple_client_id} onChange={(e) => set("apple_client_id", e.target.value)} placeholder="com.example.web" />
              </Field>
              <div className="grid gap-3 sm:grid-cols-2">
                <Field label="Team ID">
                  <Input value={f.apple_team_id} onChange={(e) => set("apple_team_id", e.target.value.trim())} placeholder="ABCDE12345" className="font-mono" />
                </Field>
                <Field label="Key ID">
                  <Input value={f.apple_key_id} onChange={(e) => set("apple_key_id", e.target.value.trim())} placeholder="XYZ9876543" className="font-mono" />
                </Field>
              </div>
              <Field label="Private key (.p8)" hint="Paste the whole AuthKey file. IvaraBase signs Apple's client secret itself, so nothing expires every 6 months.">
                <Textarea
                  rows={4}
                  spellCheck={false}
                  autoComplete="off"
                  className="font-mono text-[12px]"
                  value={f.apple_private_key}
                  onFocus={() => f.apple_private_key === "********" && set("apple_private_key", "")}
                  onChange={(e) => set("apple_private_key", e.target.value)}
                  placeholder={"-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----"}
                />
              </Field>
              <div>
                <div className="label mb-1.5">Return URL - add to the Services ID</div>
                <CopyField value={data.callback_url} />
              </div>
              {!publicCallback && (
                <Callout tone="warning" title="Needs a public HTTPS domain">
                  Apple only redirects to registered https domains, not localhost or an IP. Native sign-in works without one.
                </Callout>
              )}
            </div>
            <div className="space-y-3">
              <div className="label">Native apps</div>
              <Field label="Bundle IDs" hint="One per line. Apps with these IDs can call signInWithIdToken({ provider: 'apple', token, nonce }).">
                <Textarea rows={4} spellCheck={false} className="font-mono text-[12.5px]" value={f.apple_bundle_ids} onChange={(e) => set("apple_bundle_ids", e.target.value)} placeholder="com.example.ios" />
              </Field>
              <a href="https://developer.apple.com/account/resources/identifiers/list/serviceId" target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-[12.5px] text-primary hover:underline">
                Apple Developer identifiers <ExternalLink className="h-3 w-3" />
              </a>
            </div>
          </div>
        </Card>
      </div>

      <div className="grid gap-5 lg:grid-cols-2">
        <Card
          title={<span className="flex items-center gap-2"><UserRound className="h-4 w-4 text-primary" /> Anonymous sign-ins</span>}
          description="supabase.auth.signInAnonymously() gives guests a real user id (role authenticated, is_anonymous in the JWT). They become permanent users when they add an email or phone."
          actions={<Switch checked={f.anonymous_users} onChange={(v) => set("anonymous_users", v)} label={<span className="text-[12.5px]">{f.anonymous_users ? "Enabled" : "Disabled"}</span>} />}
        >
          <p className="text-[12.5px] text-muted-foreground">
            Tip: tell guests and real users apart in policies with <code>(auth.jwt()-&gt;&gt;'is_anonymous')::boolean</code>.
          </p>
        </Card>
        <Card
          title={<span className="flex items-center gap-2"><ShieldCheck className="h-4 w-4 text-primary" /> Multi-factor (TOTP)</span>}
          description="Authenticator apps through supabase.auth.mfa. Verified sessions get aal2 in the JWT; require it in RLS with (auth.jwt()->>'aal') = 'aal2'."
          actions={<Switch checked={f.mfa_totp_enabled} onChange={(v) => set("mfa_totp_enabled", v)} label={<span className="text-[12.5px]">{f.mfa_totp_enabled ? "Enabled" : "Disabled"}</span>} />}
        >
          <Field label="Issuer" hint="Name shown in the authenticator app. Default: your site URL's host.">
            <Input value={f.mfa_issuer} onChange={(e) => set("mfa_issuer", e.target.value)} placeholder="example.com" />
          </Field>
        </Card>
      </div>
      <Card
        title={<span className="flex items-center gap-2"><Smartphone className="h-4 w-4 text-primary" /> Phone</span>}
        description="Sign-in with a phone number and a 6-digit SMS code (signInWithOtp / verifyOtp), or phone + password."
        actions={<Switch checked={f.phone_enabled} onChange={(v) => set("phone_enabled", v)} label={<span className="text-[12.5px]">{f.phone_enabled ? "Enabled" : "Disabled"}</span>} />}
      >
        <div className="grid gap-5 lg:grid-cols-2">
          <div className="space-y-3">
            <Field label="SMS provider">
              <select className="h-9 w-full rounded-md border bg-background px-2.5 text-[13.5px]" value={f.sms_provider} onChange={(e) => set("sms_provider", e.target.value)}>
                <option value="">None: codes only appear in the SMS log (development)</option>
                <option value="twilio">Twilio</option>
                <option value="webhook">Webhook (any other provider)</option>
              </select>
            </Field>
            {f.sms_provider === "twilio" && (
              <>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="Account SID">
                    <Input className="font-mono" value={f.twilio_account_sid} onChange={(e) => set("twilio_account_sid", e.target.value.trim())} placeholder="AC..." />
                  </Field>
                  <Field label="Auth token">
                    <Input type="password" autoComplete="off" value={f.twilio_auth_token} onChange={(e) => set("twilio_auth_token", e.target.value)} />
                  </Field>
                </div>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="From number">
                    <Input value={f.twilio_from} onChange={(e) => set("twilio_from", e.target.value.trim())} placeholder="+15551234567" />
                  </Field>
                  <Field label="or Messaging Service SID">
                    <Input className="font-mono" value={f.twilio_messaging_service_sid} onChange={(e) => set("twilio_messaging_service_sid", e.target.value.trim())} placeholder="MG..." />
                  </Field>
                </div>
              </>
            )}
            {f.sms_provider === "webhook" && (
              <>
                <Field label="Webhook URL" hint="Receives POST {phone, otp, message, kind}. Send the SMS from there (MessageBird, Vonage, AWS SNS...).">
                  <Input value={f.sms_webhook_url} onChange={(e) => set("sms_webhook_url", e.target.value.trim())} placeholder="https://example.com/send-sms" />
                </Field>
                <Field label="Signing secret" hint="Header X-Ivarabase-Signature: sha256=HMAC(secret, timestamp + '.' + body), with X-Ivarabase-Timestamp">
                  <Input type="password" autoComplete="off" value={f.sms_webhook_secret} onChange={(e) => set("sms_webhook_secret", e.target.value)} />
                </Field>
              </>
            )}
            <Switch checked={f.phone_autoconfirm} onChange={(v) => set("phone_autoconfirm", v)} label="Phone + password sign-ups don't need a code first" />
          </div>
          <div className="space-y-3">
            <Field label="Message" hint="{{ .Code }} is the 6-digit code">
              <Input value={f.sms_template} onChange={(e) => set("sms_template", e.target.value)} placeholder="Your code is {{ .Code }}" />
            </Field>
            <Field label="Code lifetime (seconds)">
              <Input type="number" min={60} max={3600} value={f.sms_otp_expiry} onChange={(e) => set("sms_otp_expiry", +e.target.value)} />
            </Field>
            <Field label="Test numbers" hint="One per line: +15551234567=123456. These numbers get the fixed code and nothing is sent (app store review, automated tests).">
              <Textarea rows={3} spellCheck={false} className="font-mono text-[12.5px]" value={f.sms_test_otp} onChange={(e) => set("sms_test_otp", e.target.value)} />
            </Field>
          </div>
        </div>
      </Card>
      <Card
        title={<span className="flex items-center gap-2"><KeyRound className="h-4 w-4 text-primary" /> OpenID Connect providers</span>}
        description="Keycloak, Auth0, Okta, Microsoft Entra, Authentik, Zitadel, GitLab... Apps use signInWithOAuth({ provider: '<name>' }) or signInWithIdToken."
        actions={
          <Button
            size="sm"
            icon={<Plus className="h-3.5 w-3.5" />}
            onClick={() => set("oidc_providers", [...f.oidc_providers, { name: "", label: "", enabled: true, issuer: "", client_id: "", client_secret: "", scopes: "" }])}
          >
            Add provider
          </Button>
        }
      >
        {!f.oidc_providers.length ? (
          <p className="text-[12.5px] text-muted-foreground">No providers yet. You need the issuer URL (the part before /.well-known/openid-configuration), a client ID and a secret.</p>
        ) : (
          <div className="space-y-4">
            {f.oidc_providers.map((p, i) => (
              <div key={i} className="rounded-md border p-3">
                <div className="mb-3 flex items-center justify-between gap-2">
                  <Switch checked={p.enabled} onChange={(v) => setOidc(i, "enabled", v)} label={<span className="text-[12.5px] font-medium">{p.label || p.name || "New provider"}</span>} />
                  <Button size="sm" variant="ghost" icon={<Trash2 className="h-3.5 w-3.5" />} onClick={() => set("oidc_providers", f.oidc_providers.filter((_, j) => j !== i))}>
                    Remove
                  </Button>
                </div>
                <div className="grid gap-3 sm:grid-cols-2">
                  <Field label="Name" hint="What apps pass as provider">
                    <Input className="font-mono" value={p.name} onChange={(e) => setOidc(i, "name", e.target.value.toLowerCase().replace(/[^a-z0-9_-]/g, ""))} placeholder="keycloak" />
                  </Field>
                  <Field label="Label">
                    <Input value={p.label} onChange={(e) => setOidc(i, "label", e.target.value)} placeholder="Company SSO" />
                  </Field>
                  <Field label="Issuer URL" className="sm:col-span-2">
                    <Input value={p.issuer} onChange={(e) => setOidc(i, "issuer", e.target.value.trim())} placeholder="https://sso.example.com/realms/main" />
                  </Field>
                  <Field label="Client ID">
                    <Input value={p.client_id} onChange={(e) => setOidc(i, "client_id", e.target.value.trim())} />
                  </Field>
                  <Field label="Client secret">
                    <Input type="password" autoComplete="off" value={p.client_secret} onChange={(e) => setOidc(i, "client_secret", e.target.value)} />
                  </Field>
                  <Field label="Scopes" hint="Default: openid email profile">
                    <Input value={p.scopes} onChange={(e) => setOidc(i, "scopes", e.target.value)} placeholder="openid email profile" />
                  </Field>
                </div>
              </div>
            ))}
            <div>
              <div className="label mb-1.5">Redirect / callback URL - register it with each provider</div>
              <CopyField value={data.callback_url} />
            </div>
          </div>
        )}
      </Card>
      <Button variant="primary" loading={busy} onClick={() => save(payload())}>
        Save sign-in settings
      </Button>
    </div>
  );
}

function UrlsTab({ slug, data }: { slug: string; data: any }) {
  const c = data.config;
  const { save, busy } = useSaver(slug);
  const [site, setSite] = useState(c.site_url);
  const [urls, setUrls] = useState((c.redirect_urls ?? []).join("\n"));
  return (
    <div className="grid gap-5 lg:grid-cols-2">
      <Card title="Site URL" description="Where users land after confirming an email or signing in, when no redirect is given.">
        <Field label="Site URL">
          <Input value={site} onChange={(e) => setSite(e.target.value)} placeholder="https://myapp.com" />
        </Field>
        <div className="mt-4">
          <div className="label mb-1.5">Auth endpoint</div>
          <CopyField value={data.auth_url} />
        </div>
      </Card>
      <Card title="Redirect URLs" description="Other URLs allowed as redirectTo / emailRedirectTo. One per line; a trailing * matches anything after it.">
        <Textarea rows={6} value={urls} onChange={(e) => setUrls(e.target.value)} placeholder={"http://localhost:3000/*\nhttps://myapp.com/auth/callback"} />
      </Card>
      <div>
        <Button variant="primary" loading={busy} onClick={() => save({ site_url: site, redirect_urls: urls.split("\n").map((s: string) => s.trim()).filter(Boolean) })}>
          Save URLs
        </Button>
      </div>
    </div>
  );
}

const TEMPLATES: [string, string][] = [
  ["confirmation", "Confirm signup"],
  ["magic_link", "Magic link"],
  ["recovery", "Reset password"],
  ["invite", "Invite user"],
  ["email_change", "Change email"],
];

function EmailsTab({ slug, data }: { slug: string; data: any }) {
  const c = data.config;
  const { save, busy } = useSaver(slug);
  const [smtp, setSmtp] = useState({
    smtp_host: c.smtp.host, smtp_port: c.smtp.port, smtp_user: c.smtp.user, smtp_pass: c.smtp.pass, smtp_from_email: c.smtp.from_email, smtp_from_name: c.smtp.from_name,
    email_rate_limit_per_hour: c.email_rate_limit_per_hour,
  });
  const [kind, setKind] = useState("confirmation");
  const [tpls, setTpls] = useState<Record<string, { subject: string; body: string }>>(c.templates);
  const [testTo, setTestTo] = useState("");
  const [testing, setTesting] = useState(false);
  const s = (k: string) => (e: React.ChangeEvent<HTMLInputElement>) => setSmtp({ ...smtp, [k]: e.target.type === "number" ? +e.target.value : e.target.value });
  return (
    <div className="space-y-5">
      {!c.smtp.host && (
        <Callout tone="info" title="No SMTP server yet">
          Auth emails are kept in the <strong>Mailbox</strong> tab, where you can open the links while developing. Add SMTP (Resend, Postmark, SES, Mailgun, Gmail…) to deliver real emails.
        </Callout>
      )}
      <Card title="SMTP" description="Port 587 uses STARTTLS, 465 uses TLS.">
        <div className="grid gap-4 sm:grid-cols-3">
          <Field label="Host" className="sm:col-span-2"><Input value={smtp.smtp_host} onChange={s("smtp_host")} placeholder="smtp.resend.com" /></Field>
          <Field label="Port"><Input type="number" value={smtp.smtp_port} onChange={s("smtp_port")} /></Field>
          <Field label="Username"><Input value={smtp.smtp_user} onChange={s("smtp_user")} autoComplete="off" /></Field>
          <Field label="Password"><Input type="password" value={smtp.smtp_pass} onChange={s("smtp_pass")} autoComplete="off" /></Field>
          <Field label="Emails per hour" hint="All auth emails for this project"><Input type="number" value={smtp.email_rate_limit_per_hour} onChange={s("email_rate_limit_per_hour")} /></Field>
          <Field label="Sender email"><Input value={smtp.smtp_from_email} onChange={s("smtp_from_email")} placeholder="no-reply@myapp.com" /></Field>
          <Field label="Sender name"><Input value={smtp.smtp_from_name} onChange={s("smtp_from_name")} placeholder="My App" /></Field>
        </div>
        <div className="mt-4 flex flex-wrap items-end gap-2">
          <Button variant="primary" loading={busy} onClick={() => save(smtp, "SMTP saved")}>Save SMTP</Button>
          <span className="flex-1" />
          <Input className="w-[220px]" placeholder="you@example.com" value={testTo} onChange={(e) => setTestTo(e.target.value)} />
          <Button
            loading={testing}
            disabled={!testTo}
            icon={<Send className="h-3.5 w-3.5" />}
            onClick={async () => {
              setTesting(true);
              try {
                await op("auth.test_email", { project: slug, to: testTo });
                toast.success("Test email sent");
              } catch (e) {
                toast.error(e);
              } finally {
                setTesting(false);
              }
            }}
          >
            Send test
          </Button>
        </div>
      </Card>

      <Card title="Email templates" description="HTML with Supabase-style variables.">
        <div className="mb-4 flex flex-wrap gap-1.5">
          {TEMPLATES.map(([k, label]) => (
            <button
              key={k}
              onClick={() => setKind(k)}
              className={cn("rounded-full border px-3 py-1 text-[12.5px] transition", kind === k ? "border-primary bg-primary/10 font-medium text-primary" : "text-muted-foreground hover:bg-accent/50")}
            >
              {label}
            </button>
          ))}
        </div>
        <div className="space-y-3">
          <Field label="Subject">
            <Input value={tpls[kind]?.subject ?? ""} onChange={(e) => setTpls({ ...tpls, [kind]: { ...tpls[kind], subject: e.target.value } })} />
          </Field>
          <Field label="Body (HTML)">
            <Textarea rows={9} value={tpls[kind]?.body ?? ""} onChange={(e) => setTpls({ ...tpls, [kind]: { ...tpls[kind], body: e.target.value } })} />
          </Field>
          <p className="text-[12px] text-muted-foreground">
            Variables: <code>{"{{ .ConfirmationURL }}"}</code> <code>{"{{ .Token }}"}</code> (6-digit code) <code>{"{{ .TokenHash }}"}</code> <code>{"{{ .SiteURL }}"}</code>{" "}
            <code>{"{{ .Email }}"}</code> <code>{"{{ .NewEmail }}"}</code> <code>{"{{ .RedirectTo }}"}</code> <code>{"{{ .Data.name }}"}</code>
          </p>
          <Button variant="primary" loading={busy} onClick={() => save({ templates: tpls }, "Templates saved")}>Save templates</Button>
        </div>
      </Card>
    </div>
  );
}

function MailboxTab({ slug, smtp }: { slug: string; smtp: boolean }) {
  const mails = useOp<any[]>("auth.mails_list", { project: slug, limit: 100 }, { refetchInterval: 5_000 });
  const [view, setView] = useState<any>(null);
  return (
    <>
      <Callout tone="neutral" className="mb-4">
        Every auth email this project sends is listed here{smtp ? " (and delivered over SMTP)" : " - nothing is delivered until you add SMTP"}. Links and codes work, so you can test sign-up and
        magic links without a mail server.
      </Callout>
      <Card flush>
        {mails.isLoading ? (
          <Loading />
        ) : mails.error ? (
          <ErrorBox error={mails.error} className="m-4" />
        ) : !mails.data?.length ? (
          <Empty icon={<Inbox className="h-5 w-5" />} title="No emails yet">Sign up or request a magic link from your app and it appears here.</Empty>
        ) : (
          <DataTable
            rows={mails.data}
            rowKey={(m) => m.id}
            onRowClick={setView}
            columns={[
              { key: "at", label: "Sent", render: (m) => <span className="whitespace-nowrap text-muted-foreground" title={date(m.at)}>{ago(m.at)}</span> },
              { key: "to", label: "To", render: (m) => <span className="font-medium">{m.to}</span> },
              { key: "subject", label: "Subject", render: (m) => m.subject },
              { key: "kind", label: "Type", render: (m) => <Badge>{m.kind.replace("_", " ")}</Badge> },
              { key: "code", label: "Code", render: (m) => <code className="text-[13px] font-semibold">{(m.body.match(/<strong>(\d{6})<\/strong>/) || [])[1] ?? ""}</code> },
              { key: "state", label: "Delivery", render: (m) => (m.delivered ? <Badge tone="success">delivered</Badge> : m.error?.startsWith("no SMTP") ? <Badge>mailbox only</Badge> : <Badge tone="danger" title={m.error}>failed</Badge>) },
            ]}
          />
        )}
      </Card>
      <Dialog open={!!view} onClose={() => setView(null)} title={view?.subject ?? ""} description={view ? `To ${view.to} · ${date(view.at)}` : ""} size="lg"
        footer={view?.link && (
          <a href={view.link} target="_blank" rel="noreferrer">
            <Button variant="primary" icon={<ExternalLink className="h-3.5 w-3.5" />}>Open link</Button>
          </a>
        )}>
        {view && (
          <div className="space-y-3">
            {view.error && !view.delivered && <Callout tone={view.error.startsWith("no SMTP") ? "neutral" : "danger"}>{view.error}</Callout>}
            <iframe title="Email preview" sandbox="" srcDoc={view.body} className="h-[320px] w-full rounded-md border bg-white" />
            {view.link && <CopyField value={view.link} />}
          </div>
        )}
      </Dialog>
    </>
  );
}


function SmsTab({ slug, provider }: { slug: string; provider: string }) {
  const sms = useOp<any[]>("auth.sms_list", { project: slug, limit: 100 }, { refetchInterval: 5_000 });
  return (
    <>
      <Callout tone="neutral" className="mb-4">
        Every sign-in code this project sends by SMS{provider ? ` (through ${provider})` : " - nothing is delivered until you pick an SMS provider under Sign-in methods"}. Without a provider the codes are shown here, so
        you can test phone sign-in during development.
      </Callout>
      <Card flush>
        {sms.isLoading ? (
          <Loading />
        ) : sms.error ? (
          <ErrorBox error={sms.error} className="m-4" />
        ) : !sms.data?.length ? (
          <Empty icon={<MessageSquare className="h-5 w-5" />} title="No messages yet">Call signInWithOtp({"{ phone }"}) from your app and the code appears here.</Empty>
        ) : (
          <DataTable
            rows={sms.data}
            rowKey={(m) => m.id}
            columns={[
              { key: "at", label: "Sent", render: (m) => <span className="whitespace-nowrap text-muted-foreground" title={date(m.at)}>{ago(m.at)}</span> },
              { key: "to", label: "To", render: (m) => <span className="font-medium">{m.to}</span> },
              { key: "body", label: "Message", render: (m) => <span className="text-[13px]">{m.body}</span> },
              { key: "kind", label: "Type", render: (m) => <Badge>{m.kind.replace("_", " ")}</Badge> },
              {
                key: "state",
                label: "Delivery",
                render: (m) => (m.delivered ? <Badge tone="success">delivered</Badge> : m.error?.startsWith("no SMS") ? <Badge>log only</Badge> : <Badge tone="danger" title={m.error}>failed</Badge>),
              },
            ]}
          />
        )}
      </Card>
    </>
  );
}

function LogsTab({ slug }: { slug: string }) {
  const logs = useOp<any[]>("auth.logs", { project: slug, limit: 200 }, { refetchInterval: 10_000 });
  const tone = (a: string) => (a === "login" ? "success" : a === "login_failed" ? "danger" : a === "user_signedup" ? "primary" : "neutral") as any;
  return (
    <Card flush>
      {logs.isLoading ? (
        <Loading />
      ) : logs.error ? (
        <ErrorBox error={logs.error} className="m-4" />
      ) : (
        <DataTable
          rows={logs.data ?? []}
          rowKey={(l) => l.id}
          empty="No auth events yet."
          columns={[
            { key: "at", label: "When", render: (l) => <span className="whitespace-nowrap text-muted-foreground" title={date(l.at)}>{ago(l.at)}</span> },
            { key: "action", label: "Event", render: (l) => <Badge tone={tone(l.payload.action)}>{String(l.payload.action).replace(/_/g, " ")}</Badge> },
            { key: "user", label: "User", render: (l) => l.payload.actor_username },
            { key: "ip", label: "IP", render: (l) => <span className="font-mono text-[12px] text-muted-foreground">{l.ip}</span> },
            { key: "extra", label: "Details", render: (l) => <span className="font-mono text-[11.5px] text-muted-foreground">{l.payload.provider ?? l.payload.type ?? l.payload.via ?? ""}</span> },
          ]}
        />
      )}
    </Card>
  );
}

