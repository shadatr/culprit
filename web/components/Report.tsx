"use client";

import { useState } from "react";
import type { Evidence, Hypothesis, Incident, Report, TimelineEntry } from "@/lib/api";
import { between, compact, parseTime, pct, stampUTC, clockUTC } from "@/lib/format";
import { renderReportMarkdown } from "@/lib/markdown";
import { Card, ConfidenceBar, EvidenceChip, SectionTitle } from "./ui";

/** The get_change evidence for a change id, if the investigator fetched it. */
function findChangeEvidence(evidence: Record<string, Evidence>, changeId: string): Evidence | undefined {
  return Object.values(evidence).find((e) => {
    if (e.kind !== "change") return false;
    const input = e.input as { id?: unknown } | undefined;
    const data = e.data as { id?: unknown } | undefined;
    return input?.id === changeId || data?.id === changeId;
  });
}

export function HypothesisCard({
  h,
  rank,
  evidence,
  onOpen,
  selected,
}: {
  h: Hypothesis;
  rank: number;
  evidence: Record<string, Evidence>;
  onOpen: (id: string) => void;
  selected?: string;
}) {
  const top = rank === 1;
  const changeEv = h.change_id ? findChangeEvidence(evidence, h.change_id) : undefined;
  const headingId = `hyp-${rank}`;
  return (
    <article aria-labelledby={headingId} className={`rounded-lg border bg-panel p-4 shadow-card ${top ? "border-accent-line" : "border-line"}`}>
      <div className="flex items-start gap-3">
        <span className={`mt-0.5 grid size-6 shrink-0 place-items-center rounded-md font-mono text-[12px] font-semibold tabular ${top ? "bg-accent text-accent-ink" : "bg-panel-2 text-muted"}`} aria-label={`Rank ${rank}`}>
          {rank}
        </span>
        <div className="min-w-0 flex-1">
          <h3 id={headingId} className={`leading-snug font-semibold ${top ? "text-[15px]" : "text-[14px]"}`}>
            {h.title}
          </h3>
          <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-[12px] text-muted">
            {h.service && (
              <span>
                service <span className="font-mono text-fg-2">{h.service}</span>
              </span>
            )}
            {h.category && <span className="rounded border border-line-strong px-1.5 leading-[18px] text-fg-2">{h.category}</span>}
            {h.change_id && (
              <span className="min-w-0 break-all">
                change{" "}
                {changeEv ? (
                  <button type="button" className="font-mono text-accent underline-offset-2 hover:underline" onClick={() => onOpen(changeEv.id)} aria-label={`Open change ${h.change_id} (evidence ${changeEv.id})`}>
                    {h.change_id}
                  </button>
                ) : (
                  <span className="font-mono text-fg-2">{h.change_id}</span>
                )}
              </span>
            )}
          </div>
        </div>
        <div className="w-20 shrink-0 text-right sm:w-28">
          <div className={`font-mono text-[15px] font-semibold tabular ${top ? "text-accent" : "text-fg-2"}`}>{pct(h.confidence)}</div>
          <ConfidenceBar value={h.confidence} className="mt-1" label={`Confidence ${pct(h.confidence)}`} />
        </div>
      </div>
      {h.explanation && <p className="mt-3 text-[13px] leading-relaxed text-fg-2">{h.explanation}</p>}
      {h.next_steps.length > 0 && (
        <div className="mt-3">
          <h4 className="text-[10px] font-semibold tracking-[0.08em] text-faint uppercase">Next steps</h4>
          <ol className="mt-1 space-y-1 text-[13px]">
            {h.next_steps.map((s, i) => (
              <li key={i} className="flex gap-2">
                <span className="w-4 shrink-0 text-right font-mono text-[11px] leading-5 text-faint tabular">{i + 1}.</span>
                <span>{s}</span>
              </li>
            ))}
          </ol>
        </div>
      )}
      {h.evidence_ids.length > 0 && (
        <div className="mt-3 flex flex-wrap items-center gap-1 border-t border-line pt-3">
          <span className="mr-1 text-[11px] text-muted">Evidence</span>
          {h.evidence_ids.map((id) => (
            <EvidenceChip key={id} id={id} onOpen={onOpen} known={id in evidence} title={evidence[id]?.summary} active={selected === id} />
          ))}
        </div>
      )}
    </article>
  );
}

export function Timeline({ entries, evidence, onOpen, incidentStart }: { entries: TimelineEntry[]; evidence: Record<string, Evidence>; onOpen: (id: string) => void; incidentStart?: number }) {
  if (!entries.length) return null;
  return (
    <ol className="relative">
      {entries.map((t, i) => {
        const ms = parseTime(t.t);
        const isStart = incidentStart !== undefined && ms === incidentStart;
        const rel = ms !== undefined && incidentStart !== undefined ? relMinutes(ms - incidentStart) : "";
        return (
          <li key={i} className="grid grid-cols-[64px_minmax(0,1fr)] gap-3 border-b border-line py-2 last:border-b-0 sm:grid-cols-[120px_minmax(0,1fr)]">
            <div className="text-[12px] tabular">
              <time dateTime={t.t} title={stampUTC(t.t)} className="font-mono text-fg-2">
                {clockUTC(ms)}
              </time>
              {rel && <div className={`font-mono text-[10px] ${isStart ? "text-err" : "text-faint"}`}>{rel}</div>}
            </div>
            <div className="flex min-w-0 flex-wrap items-start justify-between gap-x-3 gap-y-1">
              <p className={`min-w-0 flex-1 text-[13px] ${isStart ? "font-medium text-err" : ""}`}>{t.text}</p>
              {t.evidence_ids.length > 0 && (
                <span className="flex flex-wrap gap-1">
                  {t.evidence_ids.map((id) => (
                    <EvidenceChip key={id} id={id} onOpen={onOpen} known={id in evidence} title={evidence[id]?.summary} />
                  ))}
                </span>
              )}
            </div>
          </li>
        );
      })}
    </ol>
  );
}

function relMinutes(ms: number): string {
  if (Math.abs(ms) < 30_000) return "T0";
  const m = Math.round(ms / 60_000);
  if (Math.abs(m) >= 120) return `T${m > 0 ? "+" : "−"}${Math.round(Math.abs(m) / 60)}h`;
  return `T${m > 0 ? "+" : "−"}${Math.abs(m)}m`;
}

export function EvidenceIndex({ order, evidence, onOpen, selected }: { order: string[]; evidence: Record<string, Evidence>; onOpen: (id: string) => void; selected?: string }) {
  if (!order.length) return null;
  return (
    <ul className="divide-y divide-line">
      {order.map((id) => {
        const e = evidence[id];
        if (!e) return null;
        return (
          <li key={id}>
            <button
              type="button"
              onClick={() => onOpen(id)}
              aria-label={`Open evidence ${id}: ${e.summary}`}
              className={`grid w-full grid-cols-[44px_minmax(0,1fr)] gap-3 px-1 py-2 text-left transition-colors hover:bg-hover focus-visible:outline-offset-[-2px] sm:grid-cols-[44px_110px_minmax(0,1fr)] ${selected === id ? "bg-accent-dim" : ""}`}
            >
              <span className="font-mono text-[12px] font-medium text-accent">{id}</span>
              <span className="hidden truncate font-mono text-[11px] leading-5 text-muted sm:block">{e.tool || e.kind}</span>
              <span className="min-w-0 truncate text-[13px] text-fg-2">{e.summary}</span>
            </button>
          </li>
        );
      })}
    </ul>
  );
}

export function UsageFooter({ report }: { report: Report }) {
  const u = report.usage;
  const items: [string, string][] = [
    ["Steps", String(u.steps)],
    ["Tool calls", String(u.tool_calls)],
    ["Tokens", `${compact(u.prompt_tokens + u.completion_tokens)} (${compact(u.prompt_tokens)} in · ${compact(u.completion_tokens)} out)`],
    ["Model", report.model || "–"],
    ["Duration", between(report.started_at, report.finished_at)],
  ];
  return (
    <footer className="mt-8 border-t border-line pt-4">
      <dl className="flex flex-wrap gap-x-8 gap-y-3 text-[12px]">
        {items.map(([k, v]) => (
          <div key={k}>
            <dt className="text-[10px] tracking-[0.08em] text-faint uppercase">{k}</dt>
            <dd className={`mt-0.5 tabular ${k === "Model" ? "font-mono" : ""} text-fg-2`}>{v}</dd>
          </div>
        ))}
      </dl>
    </footer>
  );
}

export function CopyMarkdown({ report, incident }: { report: Report; incident?: Incident }) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  async function copy() {
    const md = renderReportMarkdown(report, incident);
    try {
      await navigator.clipboard.writeText(md);
      setState("copied");
    } catch {
      // Clipboard API needs a secure context; fall back to a hidden textarea.
      const ta = document.createElement("textarea");
      ta.value = md;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand("copy");
      ta.remove();
      setState(ok ? "copied" : "failed");
    }
    setTimeout(() => setState("idle"), 2000);
  }
  return (
    <button
      type="button"
      onClick={copy}
      className="inline-flex h-8 items-center gap-1.5 rounded-md border border-line-strong bg-panel px-3 text-[13px] text-fg-2 transition-colors hover:bg-hover hover:text-fg"
    >
      <svg viewBox="0 0 16 16" className="size-3.5" aria-hidden="true">
        {state === "copied" ? (
          <path d="M3 8.5l3 3 7-7" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
        ) : (
          <>
            <rect x="5" y="5" width="8.5" height="8.5" rx="1.5" fill="none" stroke="currentColor" strokeWidth="1.4" />
            <path d="M10.5 3V2.75A1.25 1.25 0 0 0 9.25 1.5H3.75A1.25 1.25 0 0 0 2.5 2.75v5.5A1.25 1.25 0 0 0 3.75 9.5H4" fill="none" stroke="currentColor" strokeWidth="1.4" />
          </>
        )}
      </svg>
      <span aria-live="polite">{state === "copied" ? "Copied" : state === "failed" ? "Copy failed" : "Copy as Markdown"}</span>
    </button>
  );
}

export function ReportSections({
  report,
  evidence,
  order,
  onOpen,
  selected,
  incidentStart,
}: {
  report: Report;
  evidence: Record<string, Evidence>;
  order: string[];
  onOpen: (id: string) => void;
  selected?: string;
  incidentStart?: number;
}) {
  return (
    <div className="space-y-6">
      <Card className="p-4 sm:p-5" aria-labelledby="summary-title">
        <SectionTitle id="summary-title">Summary</SectionTitle>
        <p className="text-[15px] leading-relaxed text-fg">{report.summary || <span className="text-muted">No summary.</span>}</p>
      </Card>

      <section aria-labelledby="hyp-title">
        <SectionTitle id="hyp-title" aside={<span className="text-[12px] text-muted">ranked, most likely first</span>}>
          Hypotheses
        </SectionTitle>
        {report.hypotheses.length ? (
          <div className="space-y-3">
            {report.hypotheses.map((h, i) => (
              <HypothesisCard key={i} h={h} rank={i + 1} evidence={evidence} onOpen={onOpen} selected={selected} />
            ))}
          </div>
        ) : (
          <p className="text-muted">The report has no hypotheses.</p>
        )}
      </section>

      {report.timeline.length > 0 && (
        <Card className="p-4 sm:p-5" aria-labelledby="timeline-title">
          <SectionTitle id="timeline-title" aside={<span className="text-[12px] text-muted">UTC</span>}>
            Timeline
          </SectionTitle>
          <Timeline entries={report.timeline} evidence={evidence} onOpen={onOpen} incidentStart={incidentStart} />
        </Card>
      )}

      {order.length > 0 && (
        <Card className="p-4 sm:p-5" aria-labelledby="evidence-title">
          <SectionTitle id="evidence-title" aside={<span className="text-[12px] text-muted">{order.length} items</span>}>
            Evidence
          </SectionTitle>
          <EvidenceIndex order={order} evidence={evidence} onOpen={onOpen} selected={selected} />
        </Card>
      )}
    </div>
  );
}
