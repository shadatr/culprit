"use client";

import { useEffect, useState } from "react";
import { statusLabel } from "@/lib/format";

const STATUS_STYLE: Record<string, { cls: string; dot: string; live?: boolean }> = {
  queued: { cls: "text-muted bg-panel-2 border-line-strong", dot: "bg-faint" },
  running: { cls: "text-accent bg-accent-dim border-accent-line", dot: "bg-accent", live: true },
  completed: { cls: "text-ok bg-ok-dim border-ok/30", dot: "bg-ok" },
  failed: { cls: "text-err bg-err-dim border-err/30", dot: "bg-err" },
  invalid_report: { cls: "text-warn bg-warn-dim border-warn/30", dot: "bg-warn" },
  budget_exceeded: { cls: "text-warn bg-warn-dim border-warn/30", dot: "bg-warn" },
};

export function StatusPill({ status, className = "" }: { status: string; className?: string }) {
  const s = STATUS_STYLE[status] ?? { cls: "text-muted bg-panel-2 border-line-strong", dot: "bg-faint" };
  return (
    <span className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] leading-4 font-medium whitespace-nowrap ${s.cls} ${className}`}>
      <span aria-hidden="true" className={`size-1.5 rounded-full ${s.dot} ${s.live ? "pulse" : ""}`} />
      <span>{statusLabel(status)}</span>
    </span>
  );
}

export function ConfidenceBar({ value, label, className = "" }: { value: number; label?: string; className?: string }) {
  const pct = Math.round(Math.min(1, Math.max(0, value)) * 100);
  const tone = value >= 0.6 ? "bg-accent" : value >= 0.25 ? "bg-fg-2/60" : "bg-faint/70";
  return (
    <div
      role="meter"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={pct}
      aria-label={label ?? "Confidence"}
      className={`h-1.5 overflow-hidden rounded-full bg-panel-2 ${className}`}
    >
      <div className={`h-full rounded-full ${tone}`} style={{ width: `${Math.max(pct, 2)}%` }} />
    </div>
  );
}

export function EvidenceChip({
  id,
  onOpen,
  known = true,
  active = false,
  title,
}: {
  id: string;
  onOpen?: (id: string) => void;
  known?: boolean;
  active?: boolean;
  title?: string;
}) {
  const base = "inline-flex items-center rounded border px-1.5 py-px font-mono text-[11px] leading-4 tabular";
  if (!onOpen || !known) {
    return (
      <span className={`${base} border-dashed border-line-strong text-faint`} title={known ? title : "Evidence not available"}>
        {id}
      </span>
    );
  }
  return (
    <button
      type="button"
      onClick={() => onOpen(id)}
      title={title}
      aria-label={`Open evidence ${id}${title ? `: ${title}` : ""}`}
      aria-pressed={active}
      className={`${base} cursor-pointer transition-colors ${active ? "border-accent bg-accent text-accent-ink" : "border-accent-line bg-accent-dim text-accent hover:border-accent"}`}
    >
      {id}
    </button>
  );
}

/** Re-renders every `ms` so relative times stay fresh. */
export function useNow(ms = 30_000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}

export function Spinner({ className = "size-3.5" }: { className?: string }) {
  return (
    <svg viewBox="0 0 16 16" className={`spin ${className}`} aria-hidden="true">
      <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" strokeOpacity="0.2" strokeWidth="2" />
      <path d="M14 8a6 6 0 0 0-6-6" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    </svg>
  );
}

export function SectionTitle({ children, aside, id }: { children: React.ReactNode; aside?: React.ReactNode; id?: string }) {
  return (
    <div className="mb-3 flex items-center justify-between gap-3">
      <h2 id={id} className="text-[11px] font-semibold tracking-[0.08em] text-muted uppercase">
        {children}
      </h2>
      {aside}
    </div>
  );
}

export function Card({ children, className = "", ...rest }: React.HTMLAttributes<HTMLElement> & { children: React.ReactNode }) {
  return (
    <section className={`rounded-lg border border-line bg-panel shadow-card ${className}`} {...rest}>
      {children}
    </section>
  );
}

export function Kbd({ children }: { children: React.ReactNode }) {
  return <kbd className="rounded border border-line-strong bg-panel-2 px-1 font-mono text-[10px] text-muted">{children}</kbd>;
}
