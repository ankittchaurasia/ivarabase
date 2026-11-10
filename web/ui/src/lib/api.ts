// Every screen calls the same operations the REST API, CLI and MCP use:
// POST /api/v1/ops/{name}. Errors always have {code, message, hint}.
import { useQuery, type UseQueryOptions } from "@tanstack/react-query";

export class ApiError extends Error {
  status: number;
  code: string;
  hint?: string;
  details?: unknown;
  constructor(status: number, body: any) {
    const e = body?.error ?? {};
    super(e.message || `HTTP ${status}`);
    this.status = status;
    this.code = e.code || "http_" + status;
    this.hint = e.hint;
    this.details = e.details;
  }
}

export const isUnreachable = (e: unknown) => e instanceof ApiError && e.code === "server_unreachable";

export async function http<T = any>(method: string, path: string, body?: unknown): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Ivara": "1" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, {
      error: { code: "server_unreachable", message: "Cannot reach the IvaraBase server", hint: "Check that `ivarabase serve` is running." },
    });
  }
  const text = await res.text();
  let data: any = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { error: { code: "bad_response", message: text.slice(0, 300) } };
  }
  if (res.status === 401 && !path.endsWith("/auth/login")) {
    window.dispatchEvent(new Event("ivara:unauthorized"));
  }
  if (res.status === 202 && data?.status === "approval_required") return data as T;
  if (!res.ok) throw new ApiError(res.status, data);
  return data as T;
}

export function op<T = any>(name: string, args: Record<string, unknown> = {}): Promise<T> {
  const clean: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(args)) if (v !== undefined) clean[k] = v;
  return http<T>("POST", "/api/v1/ops/" + name, clean);
}

export function useOp<T = any>(
  name: string,
  args: Record<string, unknown> = {},
  opts: Partial<UseQueryOptions<T, ApiError>> = {},
) {
  return useQuery<T, ApiError>({
    queryKey: ["op", name, args],
    queryFn: () => op<T>(name, args),
    ...opts,
  });
}

export const isConfirmation = (e: unknown): e is ApiError => e instanceof ApiError && e.code === "confirmation_required";
export class Cancelled extends Error {
  constructor() {
    super("cancelled");
  }
}
export const isCancelled = (e: unknown) => e instanceof Cancelled;

// ------------------------------------------------------------------ shared types
export interface Project {
  slug: string;
  name: string;
  notes: string;
  status: string;
  created_at: string;
  size_bytes: number;
  connections: number;
  last_backup_at: string | null;
}
export interface Me {
  kind: string;
  name: string;
  level: string;
  version?: string;
  repo?: string;
}

export const REPO_URL = "https://github.com/ankittchaurasia/ivarabase";
export interface QueryResult {
  columns: { name: string; type: string }[];
  rows: unknown[][];
  row_count: number;
  truncated: boolean;
  ms: number;
  command?: string;
}
export interface Column {
  name: string;
  type: string;
  nullable: boolean;
  default?: string;
  primary_key: boolean;
  references?: string;
  generated: boolean;
}
export interface Backup {
  id: string;
  project: string;
  project_name: string;
  created_at: string;
  kind: string;
  size_bytes: number;
  local: boolean;
  offsite: boolean;
  verified: boolean | null;
  verified_at: string | null;
  verify_detail: string;
  tables: number;
}
