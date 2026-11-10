import clsx from "clsx";
import { Check, Copy, Eye, EyeOff, Loader2, X } from "lucide-react";
import {
  createContext,
  forwardRef,
  useCallback,
  useContext,
  useEffect,
  useState,
  useSyncExternalStore,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";
import { createPortal } from "react-dom";
import { ApiError, Cancelled, isCancelled, isConfirmation, op } from "../lib/api";
import { copyText, pretty } from "../lib/format";
import { Code } from "./Code";

export const cn = clsx;

// ------------------------------------------------------------------ buttons
type Variant = "primary" | "secondary" | "ghost" | "danger" | "danger-outline";
const variants: Record<Variant, string> = {
  primary: "bg-primary text-primary-foreground hover:bg-primary/90 shadow-sm border border-primary",
  secondary: "bg-card text-foreground border border-input hover:bg-accent/70 shadow-[0_1px_0_rgb(0_0_0/0.03)]",
  ghost: "text-muted-foreground hover:text-foreground hover:bg-accent/70 border border-transparent",
  danger: "bg-destructive text-white hover:bg-destructive/90 border border-destructive",
  "danger-outline": "bg-card text-destructive border border-destructive/30 hover:bg-destructive/10",
};

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: "sm" | "md" | "icon";
  loading?: boolean;
  icon?: ReactNode;
}
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = "secondary", size = "md", loading, icon, className, children, disabled, ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      disabled={disabled || loading}
      className={cn(
        "focus-ring inline-flex shrink-0 select-none items-center justify-center gap-1.5 whitespace-nowrap rounded-md font-medium transition-colors disabled:pointer-events-none disabled:opacity-50",
        size === "sm" && "h-7 px-2.5 text-[12.5px]",
        size === "md" && "h-[34px] px-3.5 text-[13px]",
        size === "icon" && "h-8 w-8",
        variants[variant],
        className,
      )}
      {...rest}
    >
      {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : icon}
      {children}
    </button>
  );
});

// ------------------------------------------------------------------ inputs
export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(function Input({ className, ...rest }, ref) {
  return <input ref={ref} className={cn("field-input h-[34px]", className)} {...rest} />;
});
export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(function Textarea(
  { className, ...rest },
  ref,
) {
  return <textarea ref={ref} className={cn("field-input min-h-[80px] font-mono text-[12.5px] leading-relaxed", className)} {...rest} />;
});
export function Select({ className, children, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      className={cn(
        "field-input h-[34px] cursor-pointer appearance-none bg-[length:14px] bg-[right_10px_center] bg-no-repeat pr-8",
        "bg-[url(\"data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' fill='none' stroke='%23888' stroke-width='2'%3E%3Cpath d='m6 9 6 6 6-6'/%3E%3C/svg%3E\")]",
        className,
      )}
      {...rest}
    >
      {children}
    </select>
  );
}
export function Checkbox({ label, hint, className, ...rest }: InputHTMLAttributes<HTMLInputElement> & { label: ReactNode; hint?: ReactNode }) {
  return (
    <label className={cn("flex cursor-pointer items-start gap-2.5 text-[13.5px]", className)}>
      <input type="checkbox" className="mt-[3px] h-4 w-4 cursor-pointer rounded accent-[hsl(var(--primary))]" {...rest} />
      <span>
        {label}
        {hint && <span className="block text-xs text-muted-foreground">{hint}</span>}
      </span>
    </label>
  );
}
export function Switch({ checked, onChange, label }: { checked: boolean; onChange: (v: boolean) => void; label?: ReactNode }) {
  return (
    <label className="inline-flex cursor-pointer items-center gap-2.5 text-[13.5px]">
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        onClick={() => onChange(!checked)}
        className={cn("focus-ring relative h-5 w-9 rounded-full transition-colors", checked ? "bg-primary" : "bg-input")}
      >
        <span className={cn("absolute top-0.5 h-4 w-4 rounded-full bg-white shadow transition-all", checked ? "left-[18px]" : "left-0.5")} />
      </button>
      {label}
    </label>
  );
}
export function Field({ label, hint, children, className }: { label: ReactNode; hint?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <label className={cn("flex min-w-0 flex-col gap-1.5", className)}>
      <span className="text-[12.5px] font-medium text-foreground/80">{label}</span>
      {children}
      {hint && <span className="text-[11.5px] text-muted-foreground">{hint}</span>}
    </label>
  );
}

// ------------------------------------------------------------------ surfaces
export function Card({
  title,
  description,
  actions,
  children,
  className,
  flush,
}: {
  title?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  children?: ReactNode;
  className?: string;
  flush?: boolean;
}) {
  return (
    <section className={cn("min-w-0 rounded-lg border bg-card shadow-soft", className)}>
      {(title || actions) && (
        <header className={cn("flex flex-wrap items-start justify-between gap-3 px-5 pt-4", flush ? "pb-4 border-b" : "pb-1")}>
          <div className="min-w-0">
            {title && <h2 className="text-[14.5px] font-semibold tracking-tight">{title}</h2>}
            {description && <p className="mt-0.5 text-[12.5px] text-muted-foreground">{description}</p>}
          </div>
          {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={cn(!flush && "px-5 pb-5 pt-3")}>{children}</div>
    </section>
  );
}

type Tone = "neutral" | "success" | "warning" | "danger" | "info" | "primary";
const tones: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-border",
  success: "bg-success/10 text-success border-success/20",
  warning: "bg-warning/10 text-warning border-warning/25",
  danger: "bg-destructive/10 text-destructive border-destructive/25",
  info: "bg-info/10 text-info border-info/20",
  primary: "bg-primary/10 text-primary border-primary/25",
};
export function Badge({ tone = "neutral", children, className, title }: { tone?: Tone; children: ReactNode; className?: string; title?: string }) {
  return (
    <span title={title} className={cn("inline-flex items-center gap-1 whitespace-nowrap rounded-full border px-2 py-[1px] font-mono text-[10.5px] font-medium uppercase tracking-wide", tones[tone], className)}>
      {children}
    </span>
  );
}
export function Dot({ tone = "neutral" }: { tone?: Tone }) {
  const c = { neutral: "bg-muted-foreground", success: "bg-success", warning: "bg-warning", danger: "bg-destructive", info: "bg-info", primary: "bg-primary" }[tone];
  return <span className={cn("inline-block h-2 w-2 shrink-0 rounded-full", c)} />;
}

export function Stat({ label, value, hint, icon }: { label: ReactNode; value: ReactNode; hint?: ReactNode; icon?: ReactNode }) {
  return (
    <div className="rounded-lg border bg-card px-4 py-3.5 shadow-soft">
      <div className="flex items-center justify-between">
        <span className="label">{label}</span>
        <span className="text-muted-foreground">{icon}</span>
      </div>
      <div className="mt-1.5 truncate font-sans font-semibold text-[26px] leading-tight tracking-tight">{value}</div>
      {hint && <div className="mt-0.5 truncate text-xs text-muted-foreground">{hint}</div>}
    </div>
  );
}

export function PageHeader({ title, description, actions, eyebrow }: { title: ReactNode; description?: ReactNode; actions?: ReactNode; eyebrow?: ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0">
        {eyebrow && <div className="label mb-1.5">{eyebrow}</div>}
        <h1 className="font-sans text-[28px] font-semibold leading-tight tracking-tight sm:text-[30px]">{title}</h1>
        {description && <p className="mt-1 max-w-2xl text-[13.5px] text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

export function Spinner({ className }: { className?: string }) {
  return <Loader2 className={cn("h-4 w-4 animate-spin text-muted-foreground", className)} />;
}
export function Loading({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 py-10 text-sm text-muted-foreground">
      <Spinner /> {label}…
    </div>
  );
}
export function Empty({ icon, title, children, action }: { icon?: ReactNode; title: ReactNode; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center px-6 py-12 text-center">
      {icon && <div className="mb-3 grid h-11 w-11 place-items-center rounded-full bg-muted text-muted-foreground">{icon}</div>}
      <div className="font-medium">{title}</div>
      {children && <div className="mt-1 max-w-sm text-[13px] text-muted-foreground">{children}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

export function ErrorBox({ error, className }: { error: unknown; className?: string }) {
  if (!error || isCancelled(error)) return null;
  const e = error instanceof ApiError ? error : { code: "", message: String((error as Error)?.message ?? error), hint: undefined, details: undefined };
  return (
    <div className={cn("rounded-md border border-destructive/25 bg-destructive/[0.07] px-3.5 py-3 text-[13px]", className)}>
      <div className="text-destructive">
        {e.code && <span className="mr-1.5 font-mono text-[11.5px] font-semibold">{e.code}</span>}
        {e.message}
      </div>
      {e.hint && <div className="mt-1 text-foreground/75">{e.hint}</div>}
      {e.details != null && <Code lang="json" code={pretty(e.details)} copy={false} wrap className="mt-2 max-h-48 bg-transparent text-[11.5px]" />}
    </div>
  );
}

export function Callout({ tone = "info", title, children, className }: { tone?: Tone; title?: ReactNode; children: ReactNode; className?: string }) {
  const c = {
    neutral: "border-border bg-muted/60",
    info: "border-info/20 bg-info/[0.07]",
    success: "border-success/20 bg-success/[0.07]",
    warning: "border-warning/25 bg-warning/[0.08]",
    danger: "border-destructive/25 bg-destructive/[0.07]",
    primary: "border-primary/20 bg-primary/[0.07]",
  }[tone];
  return (
    <div className={cn("rounded-md border px-3.5 py-3 text-[13px] leading-relaxed", c, className)}>
      {title && <div className="mb-0.5 font-semibold">{title}</div>}
      {children}
    </div>
  );
}

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="rounded border bg-muted px-1.5 py-0.5 font-mono text-[10.5px] text-muted-foreground">{children}</kbd>;
}

export function CopyField({ value, display, secret, className }: { value: string; display?: string; secret?: boolean; className?: string }) {
  const [done, setDone] = useState(false);
  const [shown, setShown] = useState(!secret);
  return (
    <div className={cn("flex min-w-0 items-center gap-1 rounded-md border bg-muted/50 py-1 pl-3 pr-1", className)}>
      <code className="scroll-thin min-w-0 flex-1 overflow-x-auto whitespace-nowrap py-1 text-[12.5px]">
        {shown ? display ?? value : "•".repeat(Math.min(32, value.length))}
      </code>
      {secret && (
        <Button variant="ghost" size="icon" className="h-7 w-7" onClick={() => setShown(!shown)} aria-label={shown ? "Hide" : "Show"}>
          {shown ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
        </Button>
      )}
      <Button
        variant="ghost"
        size="icon"
        className="h-7 w-7"
        aria-label="Copy"
        onClick={async () => {
          await copyText(value);
          setDone(true);
          setTimeout(() => setDone(false), 1400);
        }}
      >
        {done ? <Check className="h-3.5 w-3.5 text-success" /> : <Copy className="h-3.5 w-3.5" />}
      </Button>
    </div>
  );
}

export function Tabs<T extends string>({ tabs, value, onChange }: { tabs: { id: T; label: ReactNode }[]; value: T; onChange: (v: T) => void }) {
  return (
    <div className="scroll-thin mb-4 flex gap-1 overflow-x-auto border-b">
      {tabs.map((t) => (
        <button
          key={t.id}
          onClick={() => onChange(t.id)}
          className={cn(
            "-mb-px whitespace-nowrap border-b-2 px-3 py-2 text-[13px] font-medium transition-colors",
            value === t.id ? "border-primary text-foreground" : "border-transparent text-muted-foreground hover:text-foreground",
          )}
        >
          {t.label}
        </button>
      ))}
    </div>
  );
}

// ------------------------------------------------------------------ table
export interface Col<T> {
  key: string;
  label: ReactNode;
  render?: (row: T) => ReactNode;
  className?: string;
}
export function DataTable<T>({
  columns,
  rows,
  onRowClick,
  empty,
  rowKey,
  maxHeight,
}: {
  columns: Col<T>[];
  rows: T[];
  onRowClick?: (row: T) => void;
  empty?: ReactNode;
  rowKey?: (row: T, i: number) => string | number;
  maxHeight?: string;
}) {
  if (!rows.length) return <div className="px-5 py-10 text-center text-[13px] text-muted-foreground">{empty ?? "Nothing here yet."}</div>;
  return (
    <div className="scroll-thin overflow-auto" style={{ maxHeight }}>
      <table className="w-full border-collapse text-[13px]">
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key} className="sticky top-0 z-[1] whitespace-nowrap border-b bg-muted/70 px-4 py-2 text-left font-mono text-[10.5px] font-medium uppercase tracking-[0.1em] text-muted-foreground backdrop-blur">
                {c.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr
              key={rowKey ? rowKey(r, i) : i}
              onClick={onRowClick ? (e) => !(e.target as HTMLElement).closest("button,a,input,select") && onRowClick(r) : undefined}
              className={cn("border-b last:border-b-0 transition-colors hover:bg-accent/40", onRowClick && "cursor-pointer")}
            >
              {columns.map((c) => (
                <td key={c.key} className={cn("px-4 py-2.5 align-middle", c.className)}>
                  {c.render ? c.render(r) : ((r as any)[c.key] as ReactNode)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ------------------------------------------------------------------ dialog
export function Dialog({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  size = "md",
}: {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
  size?: "sm" | "md" | "lg";
}) {
  useEffect(() => {
    if (!open) return;
    const esc = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    document.addEventListener("keydown", esc);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", esc);
      document.body.style.overflow = prev;
    };
  }, [open, onClose]);
  if (!open) return null;
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-end justify-center bg-black/40 backdrop-blur-[2px] sm:items-center sm:p-4" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        role="dialog"
        aria-modal="true"
        className={cn(
          "flex max-h-[92vh] w-full animate-fade-in flex-col rounded-t-xl border bg-card shadow-pop sm:rounded-xl",
          size === "sm" && "sm:max-w-md",
          size === "md" && "sm:max-w-xl",
          size === "lg" && "sm:max-w-3xl",
        )}
      >
        <div className="flex items-start gap-3 border-b px-5 py-4">
          <div className="min-w-0 flex-1">
            <h2 className="text-[15px] font-semibold">{title}</h2>
            {description && <p className="mt-0.5 text-[12.5px] text-muted-foreground">{description}</p>}
          </div>
          <Button variant="ghost" size="icon" className="-mr-2 -mt-1" onClick={onClose} aria-label="Close">
            <X className="h-4 w-4" />
          </Button>
        </div>
        <div className="scroll-thin overflow-y-auto px-5 py-4">{children}</div>
        {footer && <div className="flex flex-wrap justify-end gap-2 border-t bg-muted/30 px-5 py-3">{footer}</div>}
      </div>
    </div>,
    document.body,
  );
}

// ------------------------------------------------------------------ toasts
type ToastItem = { id: number; tone: "default" | "error" | "success"; text: string; hint?: string };
let toastItems: ToastItem[] = [];
const toastSubs = new Set<() => void>();
const emit = () => toastSubs.forEach((f) => f());
let nextId = 1;
function push(t: Omit<ToastItem, "id">) {
  const id = nextId++;
  toastItems = [...toastItems, { ...t, id }];
  emit();
  setTimeout(() => {
    toastItems = toastItems.filter((x) => x.id !== id);
    emit();
  }, t.tone === "error" ? 7000 : 3200);
}
export const toast = {
  success: (text: string) => push({ tone: "success", text }),
  info: (text: string) => push({ tone: "default", text }),
  error: (err: unknown) => {
    if (isCancelled(err)) return;
    if (err instanceof ApiError) push({ tone: "error", text: (err.code ? err.code + ": " : "") + err.message, hint: err.hint });
    else push({ tone: "error", text: String((err as Error)?.message ?? err) });
  },
};
export function Toaster() {
  const items = useSyncExternalStore(
    (cb) => {
      toastSubs.add(cb);
      return () => toastSubs.delete(cb);
    },
    () => toastItems,
  );
  return createPortal(
    <div className="pointer-events-none fixed bottom-4 right-4 z-[60] flex max-w-[calc(100vw-2rem)] flex-col gap-2">
      {items.map((t) => (
        <div
          key={t.id}
          className={cn(
            "pointer-events-auto w-[360px] max-w-full animate-fade-in rounded-lg border px-4 py-3 text-[13px] shadow-pop",
            t.tone === "error" ? "border-destructive/30 bg-card text-destructive" : "bg-foreground text-background",
          )}
        >
          <div className="flex items-start gap-2">
            {t.tone === "success" && <Check className="mt-0.5 h-4 w-4 shrink-0 text-primary" />}
            <div>
              {t.text}
              {t.hint && <div className="mt-0.5 text-foreground/70">{t.hint}</div>}
            </div>
          </div>
        </div>
      ))}
    </div>,
    document.body,
  );
}

// ------------------------------------------------------------------ confirmation flow
// Destructive operations answer 409 confirmation_required with a preview. The
// runner shows it and only re-sends with confirm=true when the owner agrees.
type ConfirmReq = { title: string; message: string; details?: unknown; confirmLabel: string; resolve: (ok: boolean) => void };
const ConfirmCtx = createContext<(r: Omit<ConfirmReq, "resolve">) => Promise<boolean>>(async () => false);

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [req, setReq] = useState<ConfirmReq | null>(null);
  const ask = useCallback((r: Omit<ConfirmReq, "resolve">) => new Promise<boolean>((resolve) => setReq({ ...r, resolve })), []);
  const done = (ok: boolean) => {
    req?.resolve(ok);
    setReq(null);
  };
  return (
    <ConfirmCtx.Provider value={ask}>
      {children}
      <Dialog
        open={!!req}
        onClose={() => done(false)}
        title={req?.title ?? ""}
        size="md"
        footer={
          <>
            <Button onClick={() => done(false)}>Cancel</Button>
            <Button variant="danger" onClick={() => done(true)}>
              {req?.confirmLabel}
            </Button>
          </>
        }
      >
        <Callout tone="warning">{req?.message}</Callout>
        {req?.details != null && (
          <Code lang="json" code={pretty(req.details)} copy={false} className="mt-3 max-h-72" />
        )}
      </Dialog>
    </ConfirmCtx.Provider>
  );
}

export function useConfirm() {
  return useContext(ConfirmCtx);
}

/** run(op, args) with the destructive-preview → confirm → retry flow built in. */
export function useRunner() {
  const ask = useConfirm();
  return useCallback(
    async function run<T = any>(name: string, args: Record<string, unknown> = {}, opts: { title?: string; confirmLabel?: string } = {}): Promise<T> {
      try {
        return await op<T>(name, args);
      } catch (e) {
        if (!isConfirmation(e)) throw e;
        const ok = await ask({
          title: opts.title ?? "Confirm destructive action",
          message: e.message + ". Review what will happen:",
          details: e.details,
          confirmLabel: opts.confirmLabel ?? "Confirm",
        });
        if (!ok) throw new Cancelled();
        return await op<T>(name, { ...args, confirm: true });
      }
    },
    [ask],
  );
}
