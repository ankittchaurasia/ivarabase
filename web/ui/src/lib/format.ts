export function bytes(n?: number | null): string {
  if (n === null || n === undefined || n < 0) return "–";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return (i ? v.toFixed(v < 10 ? 1 : 0) : v) + " " + u[i];
}

export function ago(s?: string | null): string {
  if (!s) return "never";
  const d = (Date.now() - new Date(s).getTime()) / 1000;
  const span = (x: number) =>
    x < 60 ? "a moment" : x < 3600 ? Math.floor(x / 60) + "m" : x < 86400 ? Math.floor(x / 3600) + "h" : Math.floor(x / 86400) + "d";
  if (d < 0) return "in " + span(-d);
  return d < 60 ? "just now" : span(d) + " ago";
}

export const date = (s?: string | null) => (s ? new Date(s).toLocaleString() : "–");
export const pretty = (v: unknown) => JSON.stringify(v, null, 2);

export function cell(v: unknown): string {
  if (v === null || v === undefined) return "NULL";
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}

export async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    // http on a LAN address has no async clipboard
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.append(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
}
