"use client";

import { useEffect, useMemo, useRef } from "react";
import type { Evidence } from "@/lib/api";
import { classifyEvidence } from "@/lib/evidence";
import { stampUTC } from "@/lib/format";
import { AnomalyTable, MetricView } from "./MetricView";
import { ChangesList, JsonView, LogLines, ShiftTable, SuspectsList, TemplateTable, TopologyView } from "./Views";

export function EvidenceBody({ ev, incidentStart, suspectedChange }: { ev: Evidence; incidentStart?: number; suspectedChange?: string }) {
  const view = useMemo(() => classifyEvidence(ev), [ev]);
  switch (view.kind) {
    case "metric":
      return <MetricView series={view.series} stats={view.stats} anomalies={view.anomalies} incidentStart={incidentStart} />;
    case "anomalies":
      return <AnomalyTable stats={view.stats} anomalies={view.anomalies} />;
    case "shifts":
      return <ShiftTable shifts={view.shifts} />;
    case "templates":
      return (
        <div className="space-y-3">
          <TemplateTable templates={view.templates} />
          {view.lines.length > 0 && <LogLines lines={view.lines} />}
        </div>
      );
    case "lines":
      return <LogLines lines={view.lines} />;
    case "changes":
      return view.changes.length ? <ChangesList changes={view.changes} highlight={suspectedChange} /> : <p className="text-muted">No changes.</p>;
    case "suspects":
      return <SuspectsList suspects={view.suspects} />;
    case "topology":
      return <TopologyView edges={view.edges} />;
    default:
      return ev.data === undefined || ev.data === null || (Array.isArray(ev.data) && ev.data.length === 0) ? (
        <p className="text-[13px] text-muted">This evidence has no data payload beyond its summary.</p>
      ) : (
        <JsonView value={ev.data} label="Evidence data" />
      );
  }
}

function hasStructuredView(ev: Evidence): boolean {
  return classifyEvidence(ev).kind !== "json";
}

function isEmptyInput(v: unknown): boolean {
  return v === undefined || v === null || (typeof v === "object" && Object.keys(v as object).length === 0);
}

export function EvidencePanel({
  evidence,
  order,
  selected,
  onSelect,
  onClose,
  incidentStart,
  suspectedChange,
}: {
  evidence: Record<string, Evidence>;
  order: string[];
  selected: string;
  onSelect: (id: string) => void;
  onClose: () => void;
  incidentStart?: number;
  suspectedChange?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const closeRef = useRef<HTMLButtonElement>(null);
  const ev = evidence[selected];
  const idx = order.indexOf(selected);
  const prev = idx > 0 ? order[idx - 1] : undefined;
  const next = idx >= 0 && idx < order.length - 1 ? order[idx + 1] : undefined;

  // Remember the element that opened the panel so focus can return to it.
  const opener = useRef<Element | null>(null);
  useEffect(() => {
    opener.current = document.activeElement;
    closeRef.current?.focus();
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = prevOverflow;
      if (opener.current instanceof HTMLElement) opener.current.focus();
    };
  }, []);

  useEffect(() => {
    ref.current?.querySelector("[data-scroll]")?.scrollTo({ top: 0 });
  }, [selected]);

  function onKeyDown(e: React.KeyboardEvent) {
    if (e.key === "Escape") {
      e.stopPropagation();
      onClose();
      return;
    }
    if ((e.key === "[" || e.key === "ArrowLeft") && e.altKey && prev) onSelect(prev);
    if ((e.key === "]" || e.key === "ArrowRight") && e.altKey && next) onSelect(next);
    if (e.key === "Tab" && ref.current) {
      const f = ref.current.querySelectorAll<HTMLElement>('button:not([disabled]), a[href], summary, [tabindex]:not([tabindex="-1"])');
      if (!f.length) return;
      const first = f[0];
      const last = f[f.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last?.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first?.focus();
      }
    }
  }

  const titleId = "evidence-panel-title";
  return (
    <div className="fixed inset-0 z-40 flex justify-end" onKeyDown={onKeyDown}>
      <div className="absolute inset-0 bg-black/30 backdrop-blur-[1px]" aria-hidden="true" onClick={onClose} />
      <div ref={ref} role="dialog" aria-modal="true" aria-labelledby={titleId} className="panel-in relative flex h-full w-full max-w-[760px] flex-col border-l border-line-strong bg-bg shadow-pop">
        <div className="flex items-center gap-2 border-b border-line px-4 py-2.5">
          <span id={titleId} className="font-mono text-[13px] font-semibold">
            {selected}
          </span>
          {ev && (
            <span className="truncate text-[12px] text-muted">
              {ev.kind}
              {ev.tool ? ` · ${ev.tool}` : ""}
            </span>
          )}
          <div className="ml-auto flex items-center gap-1">
            <button
              type="button"
              disabled={!prev}
              onClick={() => prev && onSelect(prev)}
              aria-label={prev ? `Previous evidence ${prev}` : "No previous evidence"}
              title="Previous (Alt+←)"
              className="grid size-7 place-items-center rounded-md text-muted hover:bg-hover hover:text-fg disabled:opacity-30"
            >
              ‹
            </button>
            <button
              type="button"
              disabled={!next}
              onClick={() => next && onSelect(next)}
              aria-label={next ? `Next evidence ${next}` : "No next evidence"}
              title="Next (Alt+→)"
              className="grid size-7 place-items-center rounded-md text-muted hover:bg-hover hover:text-fg disabled:opacity-30"
            >
              ›
            </button>
            <button ref={closeRef} type="button" onClick={onClose} aria-label="Close evidence panel" title="Close (Esc)" className="grid size-7 place-items-center rounded-md text-muted hover:bg-hover hover:text-fg">
              <svg viewBox="0 0 16 16" className="size-3.5" aria-hidden="true">
                <path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
              </svg>
            </button>
          </div>
        </div>
        <div data-scroll className="flex-1 overflow-y-auto overscroll-contain px-4 py-4">
          {!ev ? (
            <p className="text-muted">Evidence {selected} is not in this report yet.</p>
          ) : (
            <div className="space-y-4">
              <p className="text-[14px] leading-relaxed text-fg">{ev.summary || <span className="text-muted">No summary.</span>}</p>
              <div className="flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted">
                {ev.at && <span className="tabular">recorded {stampUTC(ev.at)}</span>}
              </div>
              {!isEmptyInput(ev.input) && (
                <div>
                  <h3 className="mb-1 text-[10px] font-semibold tracking-[0.08em] text-faint uppercase">Input</h3>
                  <code className="block rounded-md border border-line bg-panel-2 px-3 py-1.5 font-mono text-[11px] break-all text-fg-2">{JSON.stringify(ev.input)}</code>
                </div>
              )}
              <div>
                <h3 className="mb-1.5 text-[10px] font-semibold tracking-[0.08em] text-faint uppercase">Data</h3>
                <EvidenceBody ev={ev} incidentStart={incidentStart} suspectedChange={suspectedChange} />
              </div>
              {ev.data !== undefined && hasStructuredView(ev) && (
                <details className="group">
                  <summary className="cursor-pointer text-[12px] text-muted hover:text-fg">
                    <span className="inline-block transition-transform group-open:rotate-90" aria-hidden="true">
                      ▸
                    </span>{" "}
                    Raw JSON
                  </summary>
                  <div className="mt-1.5">
                    <JsonView value={ev.data} />
                  </div>
                </details>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
