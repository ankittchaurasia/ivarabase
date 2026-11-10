import { LogOut, Monitor, Moon, Sun } from "lucide-react";
import { useState } from "react";
import { Button, Card, cn, Field, Input, PageHeader, toast } from "../components/ui";
import { http, op, useOp, type Me } from "../lib/api";
import { useTheme, type ThemeMode } from "../lib/theme";

export function Account() {
  const me = useOp<Me>("me");
  const { mode, setMode } = useTheme();
  const [cur, setCur] = useState("");
  const [next, setNext] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <>
      <PageHeader eyebrow="Account" title={me.data?.name ?? "Account"} description="Owner account for this IvaraBase server." />
      <div className="grid max-w-4xl gap-5 lg:grid-cols-2">
        <Card title="Appearance" description="Cream light, warm dark, or follow the system.">
          <div className="grid grid-cols-3 gap-2">
            {([
              ["light", Sun, "Light"],
              ["dark", Moon, "Dark"],
              ["system", Monitor, "System"],
            ] as [ThemeMode, typeof Sun, string][]).map(([id, Icon, label]) => (
              <button
                key={id}
                onClick={() => setMode(id)}
                className={cn(
                  "flex flex-col items-center gap-2 rounded-md border p-3 text-[12.5px] transition",
                  mode === id ? "border-primary bg-primary/[0.06] font-medium text-primary ring-2 ring-primary/15" : "text-muted-foreground hover:bg-accent/50",
                )}
              >
                <Icon className="h-4 w-4" />
                {label}
              </button>
            ))}
          </div>
        </Card>

        <Card title="Change password">
          <form
            className="space-y-3"
            onSubmit={async (e) => {
              e.preventDefault();
              setBusy(true);
              try {
                await op("account.change_password", { current_password: cur, new_password: next });
                toast.success("Password changed");
                setCur("");
                setNext("");
              } catch (err) {
                toast.error(err);
              } finally {
                setBusy(false);
              }
            }}
          >
            <Field label="Current password">
              <Input type="password" autoComplete="current-password" value={cur} onChange={(e) => setCur(e.target.value)} />
            </Field>
            <Field label="New password" hint="At least 10 characters">
              <Input type="password" autoComplete="new-password" minLength={10} value={next} onChange={(e) => setNext(e.target.value)} />
            </Field>
            <Button variant="primary" type="submit" loading={busy}>
              Update password
            </Button>
          </form>
        </Card>

        <Card title="Session">
          <Button
            icon={<LogOut className="h-4 w-4" />}
            onClick={async () => {
              await http("POST", "/api/v1/auth/logout");
              window.location.href = "/login";
            }}
          >
            Sign out
          </Button>
        </Card>
      </div>
    </>
  );
}
