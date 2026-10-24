"use client";

import { useEffect, useRef } from "react";
import type { Evidence } from "@/lib/api";
import type { Connection, Step } from "@/lib/events";
import { clockUTC, parseTime, stampUTC, statusLabel } from "@/lib/format";
import { EvidenceChip, Spinner } from "./ui";

function argText(v: unknown): string {
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

function Args({ args, name }: { args: unknown; name: string }) {
  if (name === "submit_report") return null;
  if (args === null || typeof args !== "object" || Array.isArray(args)) {
    return args === undefined || args === null ? null : <code className="font-mono text-[12px] break-all text-fg-2">{argText(args)}</code>;
  }
  const entries = Object.entries(args as Record<string, unknown>);
  if (!entries.length) return null;
  return (
    <span className="flex flex-wrap gap-x-3 gap-y-0.5 font-mono text-[12px]">
      {entries.map(([k, v]) => (
        <span key={k} className="min-w-0 break-all">
          <span className="text-muted">{k}=</span>
          <span className="text-fg-2">{argText(v)}</span>
        </span>
      ))}
    </span>
  );
}

function Time({ at }: { at: string }) {
  const t = parseTime(at);
  return (
    <time dateTime={at} title={stampUTC(at)} className="shrink-0 font-mono text-[10px] text-faint tabular">
      {clockUTC(t)}
    </time>
  );
}

function Dot({ tone }: { tone: "accent" | "muted" | "ok" | "err" | "warn" | "busy" }) {
  if (tone === "busy")
    return (
      <span className="absolute top-[3px] -left-[22px] grid size-[15px] place-items-center rounded-full bg-bg text-accent">
        <Spinner className="size-3" />
      </span>
    );
  const cls = { accent: "bg-accent", muted: "bg-faint", ok: "bg-ok", err: "bg-err", warn: "bg-warn" }[tone];
  return <span aria-hidden="true" className={`absolute top-[7px] -left-[18px] size-[7px] rounded-full ring-4 ring-bg ${cls}`} />;
}

function StepRow({ step, evidence, onOpen }: { step: Step; evidence: Record<string, Evidence>; onOpen: (id: string) => void }) {
  const chip = (id: string) => <EvidenceChip key={id} id={id} onOpen={onOpen} known={id in evidence} title={evidence[id]?.summary} />;
  switch (step.kind) {
    case "started":
      return (
        <li className="step-in relative">
          <Dot tone="muted" />
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-[13px] text-fg-2">Investigation started</span>
            <Time at={step.at} />
          </div>
          {step.incident.window.start && (
            <p className="text-[12px] text-muted tabular">
              window {clockUTC(parseTime(step.incident.window.start))}–{clockUTC(parseTime(step.incident.window.end))} UTC
            </p>
          )}
        </li>
      );
    case "triage":
      return (
        <li className="step-in relative">
          <Dot tone="accent" />
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-[13px] font-medium">Triage</span>
            <Time at={step.at} />
          </div>
          <p className="mt-0.5 text-[13px] leading-relaxed text-fg-2">{step.summary}</p>
          {step.evidence_ids.length > 0 && <div className="mt-1.5 flex flex-wrap gap-1">{step.evidence_ids.map(chip)}</div>}
        </li>
      );
    case "thinking": {
      const firstLine = step.text.split(/(?<=[.!?])\s/)[0] ?? step.text;
      return (
        <li className="step-in relative">
          <Dot tone="muted" />
          <details className="group">
            <summary className="flex cursor-pointer items-baseline justify-between gap-3 rounded-sm">
              <span className="min-w-0 text-[13px] text-muted italic group-open:not-italic">
                <span className="mr-1 inline-block text-faint not-italic transition-transform group-open:rotate-90" aria-hidden="true">
                  ▸
                </span>
                <span className="sr-only">Reasoning: </span>
                <span className="group-open:hidden">{firstLine.length > 140 ? `${firstLine.slice(0, 140)}…` : firstLine}</span>
                <span className="hidden text-faint group-open:inline">Reasoning</span>
              </span>
              <Time at={step.at} />
            </summary>
            <p className="mt-1 text-[13px] leading-relaxed whitespace-pre-wrap text-fg-2">{step.text}</p>
          </details>
        </li>
      );
    }
    case "tool": {
      const r = step.result;
      const pending = !r;
      return (
        <li className="step-in relative">
          <Dot tone={pending ? "busy" : r.error ? "err" : step.name === "submit_report" ? "ok" : "accent"} />
          <div className="flex items-baseline justify-between gap-3">
            <span className="min-w-0">
              <code className="rounded bg-panel-2 px-1.5 py-px font-mono text-[12px] font-medium text-fg">{step.name}</code>
              {pending && <span className="ml-2 text-[12px] text-muted">running…</span>}
            </span>
            <Time at={step.at} />
          </div>
          <div className="mt-1">
            <Args args={step.args} name={step.name} />
          </div>
          {r && (
            <div className="mt-1.5 flex items-start gap-2 text-[13px]">
              <span aria-hidden="true" className="mt-px text-faint">
                ↳
              </span>
              <div className="min-w-0 flex-1">
                {r.error ? <p className="text-err">{r.error}</p> : <p className="leading-relaxed text-fg-2">{r.summary || <span className="text-muted">done</span>}</p>}
              </div>
              {r.evidence_id && chip(r.evidence_id)}
            </div>
          )}
        </li>
      );
    }
    case "error":
      return (
        <li className="step-in relative">
          <Dot tone="err" />
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-[13px] font-medium text-err">Error</span>
            <Time at={step.at} />
          </div>
          <p className="text-[13px] break-words text-err">{step.message}</p>
        </li>
      );
    case "report":
      return (
        <li className="step-in relative">
          <Dot tone="ok" />
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-[13px] font-medium">
              Report ready · {step.hypotheses} hypothes{step.hypotheses === 1 ? "is" : "es"}
            </span>
            <Time at={step.at} />
          </div>
        </li>
      );
    case "finished":
      return (
        <li className="step-in relative">
          <Dot tone={step.status === "completed" ? "ok" : step.status === "failed" ? "err" : "warn"} />
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-[13px] text-fg-2">Finished: {statusLabel(step.status)}</span>
            <Time at={step.at} />
          </div>
        </li>
      );
  }
}

function describe(step: Step | undefined): string {
  if (!step) return "";
  switch (step.kind) {
    case "started":
      return "Investigation started";
    case "triage":
      return "Triage complete";
    case "thinking":
      return "Investigator is reasoning";
    case "tool":
      return step.result ? `${step.name} returned${step.result.evidence_id ? ` evidence ${step.result.evidence_id}` : ""}` : `Calling ${step.name}`;
    case "error":
      return `Error: ${step.message}`;
    case "report":
      return "Report ready";
    case "finished":
      return `Finished: ${statusLabel(step.status)}`;
  }
}

export function StepFeed({
  steps,
  evidence,
  onOpen,
  live,
  connection,
  maxHeight,
}: {
  steps: Step[];
  evidence: Record<string, Evidence>;
  onOpen: (id: string) => void;
  live: boolean;
  connection: Connection;
  maxHeight?: string;
}) {
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  useEffect(() => {
    const el = scroller.current;
    if (!el || !live || !stick.current) return;
    el.scrollTop = el.scrollHeight;
  }, [steps.length, steps, live]);

  const onScroll = () => {
    const el = scroller.current;
    if (!el) return;
    stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
  };

  const last = steps[steps.length - 1];
  return (
    <div>
      <div ref={scroller} onScroll={onScroll} className="overflow-y-auto overscroll-contain pr-1" style={maxHeight ? { maxHeight } : undefined}>
        {steps.length === 0 ? (
          <p className="flex items-center gap-2 py-2 text-[13px] text-muted">
            {connection === "unavailable" ? (
              "No event log available for this investigation."
            ) : (
              <>
                <Spinner /> Waiting for the investigator…
              </>
            )}
          </p>
        ) : (
          <ol className="relative ml-[22px] space-y-4 border-l border-line pt-0.5 pb-1 pl-[14px]" aria-label="Investigation steps">
            {steps.map((s) => (
              <StepRow key={`${s.kind}-${s.seq}-${s.at}`} step={s} evidence={evidence} onOpen={onOpen} />
            ))}
          </ol>
        )}
      </div>
      {live && (
        <p className="sr-only" aria-live="polite">
          {describe(last)}
        </p>
      )}
      {connection === "reconnecting" && (
        <p className="mt-2 flex items-center gap-2 text-[12px] text-warn" role="status">
          <Spinner className="size-3" /> Connection lost, reconnecting…
        </p>
      )}
    </div>
  );
}
