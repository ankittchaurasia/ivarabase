import { useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { useParams } from "react-router-dom";
import { Button, Card, DataTable, Empty, ErrorBox, Field, Input, Loading, PageHeader, toast } from "../components/ui";
import { op, useOp } from "../lib/api";
import { ago } from "../lib/format";

export function Secrets() {
  const { slug = "" } = useParams();
  const qc = useQueryClient();
  const secrets = useOp<{ name: string; updated_at: string }[]>("secrets.list", { project: slug });
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);

  async function save(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await op("secrets.set", { project: slug, name: name.trim(), value });
      toast.success(`Saved ${name}`);
      setName("");
      setValue("");
      qc.invalidateQueries({ queryKey: ["op", "secrets.list"] });
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader eyebrow="Compute" title="Secrets" description="Encrypted at rest and exposed to this project's functions as environment variables. Values are never shown again." />
      <Card title="Add or replace a secret" className="mb-5">
        <form onSubmit={save} className="grid gap-4 sm:grid-cols-[1fr_1.4fr_auto] sm:items-end">
          <Field label="Name" hint="UPPER_SNAKE_CASE">
            <Input className="font-mono" placeholder="STRIPE_KEY" value={name} onChange={(e) => setName(e.target.value.toUpperCase())} pattern="[A-Z_][A-Z0-9_]*" required />
          </Field>
          <Field label="Value" hint="Functions restart to pick it up">
            <Input type="password" autoComplete="off" value={value} onChange={(e) => setValue(e.target.value)} required />
          </Field>
          <Button variant="primary" type="submit" loading={busy} icon={<Plus className="h-4 w-4" />} className="sm:mb-[22px]">
            Save secret
          </Button>
        </form>
      </Card>
      <Card flush>
        {secrets.isLoading ? (
          <Loading />
        ) : secrets.error ? (
          <ErrorBox error={secrets.error} className="m-4" />
        ) : !secrets.data?.length ? (
          <Empty icon={<KeyRound className="h-5 w-5" />} title="No secrets yet">Add API keys and tokens your functions need.</Empty>
        ) : (
          <DataTable
            rows={secrets.data}
            rowKey={(s) => s.name}
            columns={[
              { key: "name", label: "Name", render: (s) => <code className="font-semibold">{s.name}</code> },
              { key: "value", label: "Value", render: () => <span className="font-mono text-muted-foreground">••••••••</span> },
              { key: "updated", label: "Updated", render: (s) => <span className="text-muted-foreground">{ago(s.updated_at)}</span> },
              {
                key: "x",
                label: "",
                className: "text-right",
                render: (s) => (
                  <Button
                    size="sm"
                    variant="ghost"
                    icon={<Trash2 className="h-3.5 w-3.5" />}
                    onClick={async () => {
                      if (!confirm(`Delete ${s.name}?`)) return;
                      try {
                        await op("secrets.delete", { project: slug, name: s.name });
                        qc.invalidateQueries({ queryKey: ["op", "secrets.list"] });
                      } catch (e) {
                        toast.error(e);
                      }
                    }}
                  >
                    Delete
                  </Button>
                ),
              },
            ]}
          />
        )}
      </Card>
    </>
  );
}
