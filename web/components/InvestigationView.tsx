"use client";

import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";
import useSWR from "swr";
import { type Evidence, TERMINAL, getInvestigation } from "@/lib/api";
import { type Phase, useInvestigationEvents } from "@/lib/events";
import { parseTime, stampUTC, relativeTime } from "@/lib/format";
import { CopyMarkdown, ReportSections, UsageFooter } from "@/components/Report";
import { StepFeed } from "@/components/StepFeed";
import { EvidencePanel } from "@/components/evidence/EvidencePanel";
import { Card, SectionTitle, Spinner, StatusPill, useNow } from "@/components/ui";

function evidenceNumber(id: string): number {
  const m = /^E(\d+)$/.exec(id);
  return m ? Number(m[1]) : Number.MAX_SAFE_INTEGER;
}

const PHASES: { key: Phase; label: string }[] = [
  { key: "triage", label: "Triage" },
  { key: "investigating", label: "Investigating" },
  { key: "reporting", label: "Reporting" },
  { key: "done", label: "Done" },
];

function PhaseBar({ phase }: { phase: Phase }) {
  const idx = PHASES.findIndex((p) => p.key === phase);
  return (
    <ol className="flex flex-wrap items-center gap-x-2 gap-y-1 text-[12px]" aria-label="Progress">
      {PHASES.map((p, i) => {
        const state = i < idx ? "done" : i === idx ? "current" : "todo";
        return (
          <li key={p.key} className="flex items-center gap-2" aria-current={state === "current" ? "step" : undefined}>
            <span className={`inline-flex items-center gap-1.5 ${state === "current" ? "font-medium text-accent" : state === "done" ? "text-fg-2" : "text-faint"}`}>
              {state === "current" && phase !== "done" ? <Spinner className="size-3" /> : <span aria-hidden="true" className={`size-1.5 rounded-full ${state === "todo" ? "bg-line-strong" : state === "done" ? "bg-fg-2" : "bg-accent"}`} />}
              {p.label}
            </span>
            {i < PHASES.length - 1 && (
              <span aria-hidden="true" className="h-px w-4 bg-line-strong" />
            )}
          </li>
        );
      })}
    </ol>
  );
}

export function InvestigationView({ id }: { id: string }) {
  const { data: inv, error, mutate } = useSWR(id ? ["investigation", id] : null, () => getInvestigation(id), {
    refreshInterval: (latest) => (latest && TERMINAL.has(latest.status) ? 0 : 5_000),
  });
  const onFinished = useCallback(() => void mutate(), [mutate]);
  const { state, connection } = useInvestigationEvents(id, { enabled: Boolean(inv), onFinished });
  const now = useNow(30_000);

  const [selected, setSelected] = useState<string | null>(null);

  // Deep link: ?e=E7 opens that evidence.
  useEffect(() => {
    const e = new URLSearchParams(window.location.search).get("e");
    if (e) setSelected(e);
  }, []);
  const open = useCallback((eid: string) => {
    setSelected(eid);
    const url = new URL(window.location.href);
    url.searchParams.set("e", eid);
    window.history.replaceState(null, "", url);
  }, []);
  const close = useCallback(() => {
    setSelected(null);
    const url = new URL(window.location.href);
    url.searchParams.delete("e");
    window.history.replaceState(null, "", url);
  }, []);

  const report = state.report ?? inv?.report;
  const incident = state.incident ?? inv?.incident;
  const incidentStart = parseTime(incident?.started_at);

  const { evidence, order } = useMemo(() => {
    const map: Record<string, Evidence> = {};
    for (const e of report?.evidence ?? []) map[e.id] = e;
    for (const [k, v] of Object.entries(state.evidence)) map[k] ??= v;
    return { evidence: map, order: Object.keys(map).sort((a, b) => evidenceNumber(a) - evidenceNumber(b) || a.localeCompare(b)) };
  }, [report, state.evidence]);

  const status = (() => {
    if (!inv) return "queued";
    if (TERMINAL.has(inv.status)) return inv.status;
    if (state.finalStatus) return state.finalStatus;
    if (state.steps.length) return "running";
    return inv.status;
  })();
  const live = !TERMINAL.has(status);
  const errorText = inv?.error || (status === "failed" ? state.errors[state.errors.length - 1] : undefined);

  if (error) {
    const notFound = typeof error === "object" && error && "status" in error && (error as { status: number }).status === 404;
    return (
      <div className="py-10">
        <Link href="/" className="text-[13px] text-muted hover:text-fg">
          ← Investigations
        </Link>
        <Card className="mt-4 border-err/30 px-5 py-6" role="alert">
          <h1 className="font-semibold text-err">{notFound ? "Investigation not found" : "Could not load investigation"}</h1>
          <p className="mt-1 text-fg-2">{error instanceof Error ? error.message : String(error)}</p>
        </Card>
      </div>
    );
  }

  if (!inv) {
    return (
      <div className="flex items-center gap-2 py-10 text-muted" aria-busy="true">
        <Spinner /> Loading investigation…
      </div>
    );
  }

  const feed = (
    <StepFeed steps={state.steps} evidence={evidence} onOpen={open} live={live} connection={connection} maxHeight={report ? "calc(100dvh - 180px)" : undefined} />
  );

  return (
    <div>
      <Link href="/" className="rounded text-[13px] text-muted hover:text-fg">
        ← Investigations
      </Link>

      <header className="mt-3 mb-6 flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="text-xl font-semibold tracking-tight break-words">{incident?.title}</h1>
            <StatusPill status={status} />
          </div>
          <dl className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 text-[12px] text-muted">
            {incident?.service && (
              <div className="flex gap-1">
                <dt>service</dt>
                <dd className="font-mono text-fg-2">{incident.service}</dd>
              </div>
            )}
            <div className="flex gap-1">
              <dt>started</dt>
              <dd className="text-fg-2 tabular" title={stampUTC(incident?.started_at)}>
                {stampUTC(incident?.started_at)} <span className="text-faint">({relativeTime(incident?.started_at, now)})</span>
              </dd>
            </div>
            {incident?.source && (
              <div className="flex gap-1">
                <dt>source</dt>
                <dd className="text-fg-2">{incident.source}</dd>
              </div>
            )}
            <div className="flex gap-1">
              <dt className="sr-only">id</dt>
              <dd className="font-mono text-faint">{inv.id}</dd>
            </div>
          </dl>
        </div>
        {report && <CopyMarkdown report={report} incident={incident ? { ...incident, id: incident.id ?? inv.id } : undefined} />}
      </header>

      {errorText && (
        <div role="alert" className="mb-6 rounded-lg border border-err/30 bg-err-dim px-4 py-3 text-[13px]">
          <p className="font-medium text-err">Investigation {status === "failed" ? "failed" : "reported an error"}</p>
          <p className="mt-0.5 break-words text-fg-2">{errorText}</p>
        </div>
      )}
      {status === "invalid_report" && (
        <div role="status" className="mb-6 rounded-lg border border-warn/30 bg-warn-dim px-4 py-3 text-[13px] text-fg-2">
          The agent could not produce a report that passed validation. Steps and evidence gathered so far are shown below.
        </div>
      )}
      {status === "budget_exceeded" && (
        <div role="status" className="mb-6 rounded-lg border border-warn/30 bg-warn-dim px-4 py-3 text-[13px] text-fg-2">
          The investigation hit its step, time or token budget. Hypotheses may be incomplete.
        </div>
      )}

      {report ? (
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,1fr)_380px]">
          <ReportSections report={report} evidence={evidence} order={order} onOpen={open} selected={selected ?? undefined} incidentStart={incidentStart} />
          <aside aria-labelledby="log-title" className="min-w-0 lg:sticky lg:top-16 lg:self-start">
            <Card className="p-4">
              <SectionTitle id="log-title" aside={<span className="text-[12px] text-muted tabular">{state.steps.length} events</span>}>
                Investigation log
              </SectionTitle>
              {feed}
            </Card>
          </aside>
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,1fr)_320px]">
          <Card className="p-4 sm:p-5" aria-labelledby="live-title">
            <SectionTitle id="live-title" aside={live ? <PhaseBar phase={state.phase === "waiting" ? "triage" : state.phase} /> : undefined}>
              {live ? "Live investigation" : "Investigation log"}
            </SectionTitle>
            {feed}
          </Card>
          <aside className="space-y-4">
            <Card className="p-4">
              <SectionTitle>Evidence so far</SectionTitle>
              {order.length ? (
                <ul className="flex flex-wrap gap-1.5">
                  {order.map((eid) => (
                    <li key={eid}>
                      <button
                        type="button"
                        onClick={() => open(eid)}
                        title={evidence[eid]?.summary}
                        aria-label={`Open evidence ${eid}: ${evidence[eid]?.summary ?? ""}`}
                        className="step-in rounded border border-accent-line bg-accent-dim px-1.5 py-px font-mono text-[11px] text-accent hover:border-accent"
                      >
                        {eid}
                      </button>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="text-[13px] text-muted">None yet.</p>
              )}
            </Card>
            {live && <p className="px-1 text-[12px] leading-relaxed text-muted">The report appears here when the investigator submits it. Every hypothesis will cite the evidence above.</p>}
          </aside>
        </div>
      )}

      {report && <UsageFooter report={report} />}

      {selected && (
        <EvidencePanel
          evidence={evidence}
          order={order}
          selected={selected}
          onSelect={open}
          onClose={close}
          incidentStart={incidentStart}
          suspectedChange={report?.hypotheses[0]?.change_id}
        />
      )}
    </div>
  );
}
