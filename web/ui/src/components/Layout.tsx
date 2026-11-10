import {
  Activity,
  ArchiveRestore,
  BookOpen,
  Boxes,
  ChevronRight,
  CircleCheckBig,
  Code2,
  Plug,
  Database,
  FileClock,
  GitBranch,
  HardDrive,
  KeyRound,
  LayoutDashboard,
  LineChart as LineChartIcon,
  LogOut,
  Menu,
  Monitor,
  Moon,
  ScrollText,
  Sun,
  Table2,
  TerminalSquare,
  UserRound,
  UsersRound,
  X,
  Zap,
  Blocks,
  Webhook,
  Github,
  Star,
} from "lucide-react";
import { Component, useEffect, useState, type ErrorInfo, type ReactNode } from "react";
import { Link, NavLink, useLocation, useNavigate, useParams } from "react-router-dom";
import { http, isUnreachable, useOp, type Me, type Project, REPO_URL } from "../lib/api";
import { useTheme, type ThemeMode } from "../lib/theme";
import { Badge, Button, cn } from "./ui";

const LAST = "ivara.lastProject";

export function Logo({ className }: { className?: string }) {
  return (
    <span className={cn("grid h-7 w-7 place-items-center rounded-[8px] bg-primary text-primary-foreground shadow-sm", className)}>
      <svg viewBox="0 0 32 32" className="h-[18px] w-[18px]" fill="currentColor">
        <path d="M7 7h5v18H7zM15 7h5l6 9-6 9h-5l6-9z" />
      </svg>
    </span>
  );
}

function ThemeToggle() {
  const { mode, setMode } = useTheme();
  const opts: { id: ThemeMode; icon: ReactNode; label: string }[] = [
    { id: "light", icon: <Sun className="h-3.5 w-3.5" />, label: "Light" },
    { id: "dark", icon: <Moon className="h-3.5 w-3.5" />, label: "Dark" },
    { id: "system", icon: <Monitor className="h-3.5 w-3.5" />, label: "System" },
  ];
  return (
    <div className="inline-flex rounded-md border bg-card p-0.5">
      {opts.map((o) => (
        <button
          key={o.id}
          title={o.label}
          aria-label={o.label + " theme"}
          onClick={() => setMode(o.id)}
          className={cn("grid h-6 w-7 place-items-center rounded-[5px] transition-colors", mode === o.id ? "bg-accent text-foreground" : "text-muted-foreground hover:text-foreground")}
        >
          {o.icon}
        </button>
      ))}
    </div>
  );
}

function NavItem({ to, icon, children, end, extra }: { to: string; icon: ReactNode; children: ReactNode; end?: boolean; extra?: ReactNode }) {
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) =>
        cn(
          "group flex items-center gap-2.5 rounded-md px-2.5 py-[7px] text-[13.5px] transition-colors",
          isActive ? "bg-primary/10 font-semibold text-primary" : "text-foreground/75 hover:bg-accent/70 hover:text-foreground",
        )
      }
    >
      <span className="shrink-0 opacity-80 [&>svg]:h-[15px] [&>svg]:w-[15px]">{icon}</span>
      <span className="truncate">{children}</span>
      {extra && <span className="ml-auto">{extra}</span>}
    </NavLink>
  );
}

function Section({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="mt-4 first:mt-1">
      <div className="label mb-1.5 px-2.5">{label}</div>
      <div className="flex flex-col gap-px">{children}</div>
    </div>
  );
}

export function Layout({ children }: { children: ReactNode }) {
  const params = useParams();
  const loc = useLocation();
  const nav = useNavigate();
  const [open, setOpen] = useState(false);
  const me = useOp<Me>("me", {}, { staleTime: 60_000 });
  const projects = useOp<Project[]>("projects.list", {}, { refetchInterval: 30_000 });
  const pending = useOp<any[]>("approvals.list", { status: "pending" }, { refetchInterval: 20_000 });

  const urlSlug = params.slug;
  const [last, setLast] = useState<string | null>(() => localStorage.getItem(LAST));
  useEffect(() => {
    if (urlSlug) {
      localStorage.setItem(LAST, urlSlug);
      setLast(urlSlug);
    }
  }, [urlSlug]);
  useEffect(() => setOpen(false), [loc.pathname]);

  const list = projects.data ?? [];
  const slug = urlSlug ?? (last && list.some((p) => p.slug === last) ? last : undefined);
  const current = list.find((p) => p.slug === slug);
  const pendingCount = pending.data?.length ?? 0;

  const crumbs = breadcrumbs(loc.pathname, current?.name);

  const sidebar = (
    <aside className="flex h-full w-[256px] flex-col border-r bg-sidebar">
      <div className="flex items-center gap-2.5 px-4 pb-3 pt-4">
        <Logo />
        <div className="leading-tight">
          <div className="text-[15px] font-semibold tracking-tight">IvaraBase</div>
          <div className="font-mono text-[9.5px] uppercase tracking-[0.18em] text-muted-foreground">self-hosted</div>
        </div>
        <button className="ml-auto rounded-md p-1 text-muted-foreground hover:bg-accent lg:hidden" onClick={() => setOpen(false)} aria-label="Close menu">
          <X className="h-4 w-4" />
        </button>
      </div>

      <div className="px-3">
        <label className="group relative flex cursor-pointer items-center gap-2 rounded-md border bg-card px-2.5 py-2 shadow-sm transition-colors hover:border-input">
          <span className="grid h-6 w-6 shrink-0 place-items-center rounded bg-muted font-mono text-[11px] font-semibold uppercase text-muted-foreground">
            {current ? current.name.slice(0, 2) : <Boxes className="h-3.5 w-3.5" />}
          </span>
          <span className="min-w-0 flex-1">
            <span className="block truncate text-[13px] font-medium">{current ? current.name : "Select project"}</span>
            {current && <span className="block truncate font-mono text-[10.5px] text-muted-foreground">{current.slug}</span>}
          </span>
          <select
            className="absolute inset-0 h-full w-full cursor-pointer opacity-0"
            value={slug ?? ""}
            aria-label="Switch project"
            onChange={(e) => e.target.value && nav(e.target.value === "__new" ? "/projects" : "/p/" + e.target.value)}
          >
            <option value="">Select project…</option>
            {list.map((p) => (
              <option key={p.slug} value={p.slug}>
                {p.name} ({p.slug})
              </option>
            ))}
            <option value="__new">+ New project</option>
          </select>
          <ChevronRight className="h-3.5 w-3.5 rotate-90 text-muted-foreground" />
        </label>
      </div>

      <nav className="scroll-thin flex-1 overflow-y-auto px-3 pb-4 pt-1">
        {slug && (
          <Section label="Project">
            <NavItem to={`/p/${slug}`} end icon={<Activity />}>Overview</NavItem>
            <NavItem to={`/p/${slug}/api`} icon={<Plug />}>API</NavItem>
            <NavItem to={`/p/${slug}/auth`} icon={<UsersRound />}>Authentication</NavItem>
            <NavItem to={`/p/${slug}/tables`} icon={<Table2 />}>Table editor</NavItem>
            <NavItem to={`/p/${slug}/sql`} icon={<TerminalSquare />}>SQL editor</NavItem>
            <NavItem to={`/p/${slug}/migrations`} icon={<GitBranch />}>Migrations</NavItem>
            <NavItem to={`/p/${slug}/extensions`} icon={<Blocks />}>Extensions</NavItem>
            <NavItem to={`/p/${slug}/storage`} icon={<HardDrive />}>Storage</NavItem>
            <NavItem to={`/p/${slug}/functions`} icon={<Zap />}>Functions</NavItem>
            <NavItem to={`/p/${slug}/webhooks`} icon={<Webhook />}>Webhooks</NavItem>
            <NavItem to={`/p/${slug}/secrets`} icon={<KeyRound />}>Secrets</NavItem>
            <NavItem to={`/p/${slug}/backups`} icon={<ArchiveRestore />}>Backups</NavItem>
          </Section>
        )}
        <Section label="Platform">
          <NavItem to="/" end icon={<LayoutDashboard />}>Dashboard</NavItem>
          <NavItem to="/projects" icon={<Database />}>Projects</NavItem>
          <NavItem to="/monitoring" icon={<LineChartIcon />}>Monitoring</NavItem>
          <NavItem to="/backups" icon={<ArchiveRestore />}>Backups</NavItem>
          <NavItem to="/approvals" icon={<CircleCheckBig />} extra={pendingCount ? <Badge tone="warning">{pendingCount}</Badge> : null}>
            Approvals
          </NavItem>
          <NavItem to="/tokens" icon={<KeyRound />}>Access tokens</NavItem>
          <NavItem to="/audit" icon={<ScrollText />}>Audit log</NavItem>
          <NavItem to="/docs" icon={<Code2 />}>API &amp; MCP</NavItem>
        </Section>
      </nav>

      <div className="flex items-center gap-2 border-t px-3 py-3">
        <Link to="/account" className="flex min-w-0 flex-1 items-center gap-2 rounded-md px-1.5 py-1 hover:bg-accent/70">
          <span className="grid h-7 w-7 shrink-0 place-items-center rounded-full bg-primary/15 text-primary">
            <UserRound className="h-3.5 w-3.5" />
          </span>
          <span className="min-w-0">
            <span className="block truncate text-[13px] font-medium">{me.data?.name ?? "…"}</span>
            <span className="block font-mono text-[10px] uppercase tracking-wider text-muted-foreground">owner</span>
          </span>
        </Link>
        <ThemeToggle />
      </div>
      <a
        href={me.data?.repo ?? REPO_URL}
        target="_blank"
        rel="noreferrer"
        className="flex items-center gap-1.5 border-t px-4 py-2 text-[11.5px] text-muted-foreground hover:bg-accent/50 hover:text-foreground"
        title="Docs, releases and issues on GitHub"
      >
        <Github className="h-3.5 w-3.5" />
        <span className="font-mono">v{(me.data?.version ?? "").replace(/^v/, "")}</span>
        <span className="ml-auto inline-flex items-center gap-1">
          <Star className="h-3 w-3" /> Star on GitHub
        </span>
      </a>
    </aside>
  );

  if (isUnreachable(me.error)) return <ServerDown error={me.error} onRetry={me.refetch} retrying={me.isFetching} />;

  return (
    <div className="flex min-h-full">
      <div className="sticky top-0 hidden h-screen lg:block">{sidebar}</div>
      {open && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div className="absolute inset-0 bg-black/40 backdrop-blur-[1px]" onClick={() => setOpen(false)} />
          <div className="absolute inset-y-0 left-0 animate-fade-in shadow-pop">{sidebar}</div>
        </div>
      )}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-30 flex h-[52px] items-center gap-2 border-b bg-background/80 px-3 backdrop-blur-md sm:px-5" style={{ paddingTop: "env(safe-area-inset-top)" }}>
          <button className="rounded-md p-1.5 text-muted-foreground hover:bg-accent lg:hidden" onClick={() => setOpen(true)} aria-label="Open menu">
            <Menu className="h-[18px] w-[18px]" />
          </button>
          <Logo className="h-6 w-6 lg:hidden" />
          <nav className="flex min-w-0 items-center gap-1.5 text-[13px]">
            {crumbs.map((c, i) => (
              <span key={i} className="flex min-w-0 items-center gap-1.5">
                {i > 0 && <span className="text-muted-foreground/50">/</span>}
                {c.to ? (
                  <Link to={c.to} className={cn("truncate", i === crumbs.length - 1 ? "font-medium" : "text-muted-foreground hover:text-foreground")}>
                    {c.label}
                  </Link>
                ) : (
                  <span className={cn("truncate", i === crumbs.length - 1 ? "font-medium" : "text-muted-foreground")}>{c.label}</span>
                )}
              </span>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-1.5">
            {pendingCount > 0 && (
              <Link to="/approvals" className="flex items-center gap-1.5 rounded-md border border-warning/30 bg-warning/10 px-2 py-1 text-[12px] font-medium text-warning">
                <FileClock className="h-3.5 w-3.5" /> {pendingCount} waiting
              </Link>
            )}
            <Link to="/docs" className="hidden items-center gap-1.5 rounded-md px-2 py-1 text-[12.5px] text-muted-foreground hover:bg-accent hover:text-foreground sm:flex">
              <BookOpen className="h-3.5 w-3.5" /> Docs
            </Link>
            <button
              className="rounded-md p-1.5 text-muted-foreground hover:bg-accent hover:text-foreground"
              title="Sign out"
              aria-label="Sign out"
              onClick={async () => {
                await http("POST", "/api/v1/auth/logout");
                window.location.href = "/login";
              }}
            >
              <LogOut className="h-4 w-4" />
            </button>
          </div>
        </header>
        <main className="mx-auto w-full max-w-[1240px] flex-1 px-4 pb-24 pt-6 sm:px-7 sm:pt-8">
          <PageBoundary key={loc.pathname}>{children}</PageBoundary>
        </main>
      </div>
    </div>
  );
}

function breadcrumbs(path: string, projectName?: string): { label: string; to?: string }[] {
  const parts = path.split("/").filter(Boolean);
  const out: { label: string; to?: string }[] = [{ label: "IvaraBase", to: "/" }];
  const names: Record<string, string> = {
    projects: "Projects", backups: "Backups", approvals: "Approvals", tokens: "Access tokens", audit: "Audit log", docs: "API & MCP", api: "API", auth: "Authentication", storage: "Storage", monitoring: "Monitoring", account: "Account",
    tables: "Table editor", sql: "SQL editor", migrations: "Migrations", extensions: "Extensions", functions: "Functions", webhooks: "Webhooks", secrets: "Secrets",
  };
  if (parts[0] === "p" && parts[1]) {
    out.push({ label: projectName ?? parts[1], to: "/p/" + parts[1] });
    if (parts[2]) out.push({ label: names[parts[2]] ?? parts[2], to: `/p/${parts[1]}/${parts[2]}` });
    if (parts[3]) out.push({ label: decodeURIComponent(parts[3]) });
  } else if (parts[0]) {
    out.push({ label: names[parts[0]] ?? parts[0] });
  } else {
    out.push({ label: "Dashboard" });
  }
  return out;
}

/** Full-page state when the Go server is not running (common with `make ui-dev`). */
function ServerDown({ error, onRetry, retrying }: { error: unknown; onRetry: () => void; retrying: boolean }) {
  const e = error as { message?: string; hint?: string };
  useEffect(() => {
    const t = setInterval(onRetry, 4000); // reconnect on its own once the server is up
    return () => clearInterval(t);
  }, [onRetry]);
  return (
    <div className="grid min-h-full place-items-center p-6">
      <div className="w-full max-w-md rounded-lg border bg-card p-6 shadow-soft">
        <Logo />
        <h1 className="mt-5 font-sans text-[26px] font-semibold tracking-tight">Can't reach the server</h1>
        <p className="mt-1.5 text-[13.5px] text-muted-foreground">{e?.message}</p>
        <div className="mt-5 rounded-md border bg-muted/50 p-3 text-[13px]">
          <div className="label mb-1.5">Start it</div>
          <code className="block">./bin/ivarabase serve</code>
          <div className="mt-2 text-[12px] text-muted-foreground">or run server and UI together with <code>make dev</code></div>
        </div>
        <div className="mt-5 flex items-center gap-3">
          <Button variant="primary" onClick={onRetry} loading={retrying}>
            Retry
          </Button>
          <span className="text-[12px] text-muted-foreground">Retrying every few seconds…</span>
        </div>
      </div>
    </div>
  );
}

/** A crash inside one page shows an error card instead of blanking the whole app. */
class PageBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null };
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("page crashed", error, info.componentStack);
  }
  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="mx-auto max-w-lg rounded-lg border border-destructive/25 bg-card p-6 shadow-soft">
        <div className="label text-destructive">Page error</div>
        <h1 className="mt-1 font-sans text-[24px] font-semibold">This page crashed</h1>
        <pre className="mt-3 overflow-auto whitespace-pre-wrap rounded-md bg-muted/50 p-3 text-[12px]">{this.state.error.message}</pre>
        <div className="mt-4 flex gap-2">
          <Button variant="primary" onClick={() => this.setState({ error: null })}>Try again</Button>
          <Button onClick={() => (window.location.href = "/")}>Go to dashboard</Button>
        </div>
      </div>
    );
  }
}
