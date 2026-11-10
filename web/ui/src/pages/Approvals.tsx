import { useQueryClient } from "@tanstack/react-query";
import { Bot, Check, CircleCheckBig, X } from "lucide-react";
import { useState } from "react";
import { Badge, Button, Card, Empty, ErrorBox, Loading, PageHeader, Tabs, toast } from "../components/ui";
import { Code } from "../components/Code";
import { op, useOp } from "../lib/api";
import { ago, date, pretty } from "../lib/format";

const tone = (s: string) => ({ pending: "warning", executed: "success", rejected: "neutral", failed: "danger", running: "info" })[s] as any;

export function Approvals() {
  const qc = useQueryClient();
  const [status, setStatus] = useState<"pending" | "all">("pending");
  const list = useOp<any[]>("approvals.list", { status }, { refetchInterval: 10_000 });
  const [busy, setBusy] = useState("");

  async function decide(id: string, decision: "approve" | "reject") {
    setBusy(id + decision);
    try {
      const r = await op("approvals.decide", { id, decision });
      if (r.status === "failed") toast.error(new Error("Approved, but it failed: " + (r.error?.message ?? "")));
      else toast.success(decision === "approve" ? "Approved and executed" : "Rejected");
      qc.invalidateQueries();
    } catch (e) {
      toast.error(e);
    } finally {
      setBusy("");
    }
  }

  return (
    <>
      <PageHeader eyebrow="Access" title="Approvals" description="Destructive requests from tokens that need your OK. Approving runs the operation exactly as the agent asked." />
      <Tabs tabs={[{ id: "pending", label: "Waiting" }, { id: "all", label: "History" }]} value={status} onChange={setStatus} />
      {list.isLoading ? (
        <Loading />
      ) : list.error ? (
        <ErrorBox error={list.error} />
      ) : !list.data?.length ? (
        <Card>
          <Empty icon={<CircleCheckBig className="h-5 w-5" />} title={status === "pending" ? "Nothing waiting for you" : "No approvals yet"}>
            When an agent's token tries something destructive, it shows up here with a preview.
          </Empty>
        </Card>
      ) : (
        <div className="space-y-4">
          {list.data.map((a) => (
            <Card key={a.id}>
              <div className="flex flex-wrap items-start gap-3">
                <span className="grid h-9 w-9 shrink-0 place-items-center rounded-full bg-warning/10 text-warning">
                  <Bot className="h-4 w-4" />
                </span>
                <div className="min-w-[200px] flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <code className="text-[14px] font-semibold">{a.op}</code>
                    {a.project && <Badge>{a.project}</Badge>}
                    <Badge tone={tone(a.status)}>{a.status}</Badge>
                  </div>
                  <div className="mt-0.5 text-[12.5px] text-muted-foreground" title={date(a.created_at)}>
                    requested by <span className="font-medium text-foreground/80">{a.actor}</span> · {ago(a.created_at)}
                    {a.decided_by && <> · decided by {a.decided_by}</>}
                  </div>
                </div>
                {a.status === "pending" && (
                  <div className="flex w-full gap-2 sm:w-auto [&>*]:flex-1 sm:[&>*]:flex-none">
                    <Button icon={<X className="h-3.5 w-3.5" />} loading={busy === a.id + "reject"} onClick={() => decide(a.id, "reject")}>
                      Reject
                    </Button>
                    <Button variant="danger" icon={<Check className="h-3.5 w-3.5" />} loading={busy === a.id + "approve"} onClick={() => decide(a.id, "approve")}>
                      Approve &amp; run
                    </Button>
                  </div>
                )}
              </div>
              {a.preview && Object.keys(a.preview).length > 0 && (
                <div className="mt-4">
                  <div className="label mb-1.5">What will happen</div>
                  <Code lang="json" code={pretty(a.preview)} className="max-h-60" />
                </div>
              )}
              <details className="mt-3 text-[12.5px]">
                <summary className="cursor-pointer text-muted-foreground hover:text-foreground">Arguments</summary>
                <Code lang="json" code={pretty(a.args)} className="mt-2" />
              </details>
              {a.error && <ErrorBox error={new Error(a.error)} className="mt-3" />}
              {a.status === "executed" && a.result && (
                <details className="mt-2 text-[12.5px]">
                  <summary className="cursor-pointer text-muted-foreground hover:text-foreground">Result</summary>
                  <Code lang="json" code={pretty(a.result)} className="mt-2" />
                </details>
              )}
            </Card>
          ))}
        </div>
      )}
    </>
  );
}
