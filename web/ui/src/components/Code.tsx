import { Check, Copy } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { copyText } from "../lib/format";
import { Button, cn } from "./ui";

// Read-only code blocks with syntax highlighting. Small tokenizers instead of
// CodeMirror keep the main bundle light (the editor loads lazily); colours are
// the same --syn-* variables the editor uses, so both themes match.

export type CodeLang = "ts" | "js" | "json" | "sh" | "yaml" | "sql" | "text";

type Kind = "keyword" | "string" | "number" | "comment" | "fn" | "type" | "prop" | "punct" | "var" | "flag";

const KW: Record<string, Set<string>> = {
  ts: new Set(
    "const let var function return await async import from export default if else for of in while new class extends interface type enum public private readonly try catch finally throw typeof as null undefined true false this".split(" "),
  ),
  sql: new Set(
    "select from where and or not insert into values update set delete create table alter drop add column primary key references on to grant revoke policy using with check enable row level security function returns language as begin end if exists replace index unique default null true false order by group limit offset join left inner returning schema extension publication view trigger for each execute procedure".split(
      " ",
    ),
  ),
  sh: new Set("sudo export cd if then fi for do done echo cat curl docker npm npx make git ivarabase claude".split(" ")),
};

const RULES: Record<Exclude<CodeLang, "text" | "js">, [RegExp, Kind | ((m: string, rest: string) => Kind | null)][]> = {
  ts: [
    [/^\/\/[^\n]*/, "comment"],
    [/^\/\*[\s\S]*?\*\//, "comment"],
    [/^`(?:\\[\s\S]|[^`\\])*`|^"(?:\\.|[^"\\\n])*"|^'(?:\\.|[^'\\\n])*'/, "string"],
    [/^\d[\d_.]*\b/, "number"],
    [/^[A-Za-z_$][\w$]*/, (m, rest) => (KW.ts.has(m) ? "keyword" : /^\s*\(/.test(rest) ? "fn" : /^[A-Z]/.test(m) ? "type" : /^\s*:/.test(rest) ? "prop" : null)],
    [/^[{}()[\];,.<>]/, "punct"],
  ],
  json: [
    [/^"(?:\\.|[^"\\])*"(?=\s*:)/, "prop"],
    [/^"(?:\\.|[^"\\])*"/, "string"],
    [/^-?\d[\d.eE+-]*/, "number"],
    [/^(?:true|false|null)\b/, "keyword"],
    [/^[{}[\],:]/, "punct"],
  ],
  sh: [
    [/^#[^\n]*/, "comment"],
    [/^"(?:\\.|[^"\\])*"|^'[^']*'/, "string"],
    [/^\$\{?[A-Za-z_][\w]*\}?/, "var"],
    [/^--?[A-Za-z][\w-]*/, "flag"],
    [/^[A-Za-z_][\w.-]*(?==)/, "prop"],
    [/^[A-Za-z_][\w./-]*/, (m) => (KW.sh.has(m) ? "fn" : null)],
    [/^[|&;\\]/, "punct"],
  ],
  yaml: [
    [/^#[^\n]*/, "comment"],
    [/^[A-Za-z_][\w.-]*(?=\s*:)/, "prop"],
    [/^"(?:\\.|[^"\\])*"|^'[^']*'/, "string"],
    [/^\$\{[^}]*\}/, "var"],
    [/^-?\d[\d.]*\b/, "number"],
    [/^(?:true|false|null)\b/, "keyword"],
    [/^[{}[\],:-]/, "punct"],
  ],
  sql: [
    [/^--[^\n]*/, "comment"],
    [/^'(?:''|[^'])*'/, "string"],
    [/^\$\$[\s\S]*?\$\$/, "string"],
    [/^\d[\d.]*\b/, "number"],
    [/^[A-Za-z_][\w]*/, (m, rest) => (KW.sql.has(m.toLowerCase()) ? "keyword" : /^\s*\(/.test(rest) ? "fn" : null)],
    [/^[(),;.]/, "punct"],
  ],
};

function tokenize(code: string, lang: CodeLang): ReactNode[] {
  const rules = RULES[(lang === "js" ? "ts" : lang) as keyof typeof RULES];
  if (!rules) return [code];
  const out: ReactNode[] = [];
  let plain = "";
  let i = 0;
  const flush = () => {
    if (plain) out.push(plain);
    plain = "";
  };
  while (i < code.length) {
    const rest = code.slice(i);
    let hit = false;
    for (const [re, kind] of rules) {
      const m = re.exec(rest);
      if (!m || !m[0]) continue;
      const k = typeof kind === "function" ? kind(m[0], rest.slice(m[0].length)) : kind;
      if (k) {
        flush();
        out.push(
          <span key={i} className={`syn-${k}`}>
            {m[0]}
          </span>,
        );
      } else {
        plain += m[0];
      }
      i += m[0].length;
      hit = true;
      break;
    }
    if (!hit) {
      plain += code[i];
      i++;
    }
  }
  flush();
  return out;
}

/** Highlighted, read-only code with an optional copy button. */
export function Code({
  code,
  lang = "text",
  copy = true,
  wrap = false,
  className,
}: {
  code: string;
  lang?: CodeLang;
  copy?: boolean;
  wrap?: boolean;
  className?: string;
}) {
  const [done, setDone] = useState(false);
  const nodes = useMemo(() => tokenize(code, lang), [code, lang]);
  return (
    <div className="group relative min-w-0">
      <pre
        className={cn(
          "scroll-thin overflow-auto rounded-md border bg-muted/40 p-3 font-mono text-[12px] leading-relaxed",
          wrap && "whitespace-pre-wrap break-words",
          copy && "pr-11",
          className,
        )}
      >
        <code>{nodes}</code>
      </pre>
      {copy && (
        <Button
          variant="ghost"
          size="icon"
          className="absolute right-1.5 top-1.5 h-7 w-7 opacity-70 hover:opacity-100"
          aria-label="Copy"
          onClick={async () => {
            await copyText(code);
            setDone(true);
            setTimeout(() => setDone(false), 1400);
          }}
        >
          {done ? <Check className="h-3.5 w-3.5 text-success" /> : <Copy className="h-3.5 w-3.5" />}
        </Button>
      )}
    </div>
  );
}
