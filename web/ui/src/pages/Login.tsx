import { useQueryClient } from "@tanstack/react-query";
import { ArrowRight, Database, ShieldCheck, Sparkles, Github } from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Logo } from "../components/Layout";
import { Button, ErrorBox, Field, Input } from "../components/ui";
import { http, REPO_URL } from "../lib/api";

export function Login() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await http("POST", "/api/v1/auth/login", { username, password });
      qc.clear();
      nav("/", { replace: true });
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid min-h-full lg:grid-cols-[1.1fr_1fr]">
      <div className="relative hidden overflow-hidden border-r bg-sidebar lg:flex lg:flex-col lg:justify-between lg:p-12">
        <div className="pointer-events-none absolute -left-24 -top-24 h-[420px] w-[420px] rounded-full bg-primary/10 blur-3xl" />
        <div className="pointer-events-none absolute -bottom-32 right-0 h-[360px] w-[360px] rounded-full bg-warning/10 blur-3xl" />
        <div className="relative flex items-center gap-2.5">
          <Logo />
          <span className="text-[15px] font-semibold">IvaraBase</span>
        </div>
        <div className="relative max-w-md">
          <h1 className="font-sans text-[40px] font-semibold leading-[1.1] tracking-tight">Your own Postgres platform, run by you and your agents.</h1>
          <ul className="mt-8 space-y-4 text-[14px] text-foreground/80">
            <li className="flex gap-3"><Database className="mt-0.5 h-4 w-4 shrink-0 text-primary" /> A database, role, functions and backups per project, in under a second.</li>
            <li className="flex gap-3"><Sparkles className="mt-0.5 h-4 w-4 shrink-0 text-primary" /> Every action is an API call, an MCP tool and a CLI command.</li>
            <li className="flex gap-3"><ShieldCheck className="mt-0.5 h-4 w-4 shrink-0 text-primary" /> Destructive agent actions wait for your approval.</li>
          </ul>
        </div>
        <div className="label relative">self-hosted · macOS &amp; Linux</div>
      </div>

      <div className="flex items-center justify-center p-6">
        <form onSubmit={submit} className="w-full max-w-[360px]">
          <div className="mb-8 flex items-center gap-2.5 lg:hidden">
            <Logo />
            <span className="text-[15px] font-semibold">IvaraBase</span>
          </div>
          <h2 className="font-sans text-[28px] font-semibold tracking-tight">Welcome back</h2>
          <p className="mt-1 text-[13.5px] text-muted-foreground">Sign in to your dashboard.</p>
          <div className="mt-7 space-y-4">
            <Field label="Username">
              <Input autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} />
            </Field>
            <Field label="Password">
              <Input type="password" autoComplete="current-password" autoFocus value={password} onChange={(e) => setPassword(e.target.value)} />
            </Field>
            <ErrorBox error={error} />
            <Button variant="primary" type="submit" className="w-full" loading={busy}>
              Sign in <ArrowRight className="h-4 w-4" />
            </Button>
          </div>
          <p className="mt-6 text-[12.5px] text-muted-foreground">
            Forgot it? Run <code className="rounded bg-muted px-1 py-0.5">ivarabase reset-password</code> on the server.
          </p>
          <a href={REPO_URL} target="_blank" rel="noreferrer" className="mt-3 inline-flex items-center gap-1.5 text-[12.5px] text-muted-foreground hover:text-foreground">
            <Github className="h-3.5 w-3.5" /> IvaraBase on GitHub
          </a>
        </form>
      </div>
    </div>
  );
}
