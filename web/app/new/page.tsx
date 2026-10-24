"use client";

import { useRouter } from "next/navigation";
import { useEffect, useId, useState } from "react";
import { buildIncident, createInvestigation } from "@/lib/api";
import { Card, Spinner } from "@/components/ui";

const LOOKBACK = [
  { label: "15m", minutes: 15 },
  { label: "30m", minutes: 30 },
  { label: "1h", minutes: 60 },
  { label: "2h", minutes: 120 },
  { label: "6h", minutes: 360 },
];
const AFTER = [
  { label: "5m", minutes: 5 },
  { label: "15m", minutes: 15 },
  { label: "30m", minutes: 30 },
  { label: "1h", minutes: 60 },
];

/** Local "YYYY-MM-DDTHH:MM" for <input type="datetime-local">. */
function localInputValue(d: Date): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

const input =
  "h-9 w-full rounded-md border border-line-strong bg-panel px-3 text-[14px] text-fg placeholder:text-faint focus-visible:border-accent focus-visible:outline-2 focus-visible:outline-offset-0 focus-visible:outline-accent";

function Segmented({ name, value, options, onChange, labelledBy }: { name: string; value: number; options: { label: string; minutes: number }[]; onChange: (v: number) => void; labelledBy: string }) {
  return (
    <div role="radiogroup" aria-labelledby={labelledBy} className="inline-flex flex-wrap rounded-md border border-line-strong bg-panel-2 p-0.5">
      {options.map((o) => {
        const checked = o.minutes === value;
        return (
          <label
            key={o.minutes}
            className={`relative cursor-pointer rounded px-3 py-1 text-[13px] tabular transition-colors has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-accent ${checked ? "bg-panel font-medium text-fg shadow-card" : "text-muted hover:text-fg"}`}
          >
            <input type="radio" name={name} value={o.minutes} checked={checked} onChange={() => onChange(o.minutes)} className="sr-only" />
            {o.label}
          </label>
        );
      })}
    </div>
  );
}

export default function NewInvestigationPage() {
  const router = useRouter();
  const ids = { title: useId(), service: useId(), start: useId(), lookback: useId(), after: useId(), err: useId() };
  const [title, setTitle] = useState("");
  const [service, setService] = useState("");
  const [start, setStart] = useState("");
  const [lookback, setLookback] = useState(60);
  const [after, setAfter] = useState(15);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Default the start to "now" on the client so the prerendered HTML matches.
  useEffect(() => {
    setStart((s) => s || localInputValue(new Date()));
  }, []);

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    if (!title.trim()) {
      setError("Give the incident a title, e.g. the alert name.");
      return;
    }
    const startDate = start ? new Date(start) : new Date();
    if (Number.isNaN(startDate.getTime())) {
      setError("Start time is not a valid date.");
      return;
    }
    setSubmitting(true);
    try {
      const id = await createInvestigation(buildIncident({ title, service, start: startDate, lookbackMinutes: lookback, afterMinutes: after }));
      router.push(`/investigations/${encodeURIComponent(id)}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSubmitting(false);
    }
  }

  const windowText = (() => {
    const s = start ? new Date(start) : null;
    if (!s || Number.isNaN(s.getTime())) return null;
    const fmt = (d: Date) => d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
    return `${fmt(new Date(s.getTime() - lookback * 60_000))} → ${fmt(new Date(s.getTime() + after * 60_000))}`;
  })();

  return (
    <div className="mx-auto max-w-xl">
      <h1 className="text-xl font-semibold tracking-tight">New investigation</h1>
      <p className="mt-0.5 text-[13px] text-muted">Describe the symptom. Culprit compares the lookback (baseline) with the time after the start.</p>

      <Card className="mt-5 p-5">
        <form onSubmit={onSubmit} noValidate aria-describedby={error ? ids.err : undefined} className="space-y-5">
          <div>
            <label htmlFor={ids.title} className="mb-1.5 block text-[13px] font-medium">
              Title
            </label>
            <input id={ids.title} className={input} value={title} onChange={(e) => setTitle(e.target.value)} placeholder="High p99 latency on frontend" required autoFocus autoComplete="off" />
          </div>
          <div>
            <label htmlFor={ids.service} className="mb-1.5 block text-[13px] font-medium">
              Service <span className="font-normal text-muted">(optional, the symptomatic one)</span>
            </label>
            <input id={ids.service} className={`${input} font-mono`} value={service} onChange={(e) => setService(e.target.value)} placeholder="frontend" autoComplete="off" spellCheck={false} />
          </div>
          <div>
            <label htmlFor={ids.start} className="mb-1.5 block text-[13px] font-medium">
              Started at <span className="font-normal text-muted">(your local time)</span>
            </label>
            <input id={ids.start} type="datetime-local" className={`${input} tabular`} value={start} onChange={(e) => setStart(e.target.value)} />
          </div>
          <div className="grid gap-5 sm:grid-cols-2">
            <div>
              <p id={ids.lookback} className="mb-1.5 text-[13px] font-medium">
                Lookback
              </p>
              <Segmented name="lookback" value={lookback} options={LOOKBACK} onChange={setLookback} labelledBy={ids.lookback} />
            </div>
            <div>
              <p id={ids.after} className="mb-1.5 text-[13px] font-medium">
                After start
              </p>
              <Segmented name="after" value={after} options={AFTER} onChange={setAfter} labelledBy={ids.after} />
            </div>
          </div>
          {windowText && (
            <p className="text-[12px] text-muted tabular">
              Analysis window: <span className="text-fg-2">{windowText}</span>
            </p>
          )}
          {error && (
            <p id={ids.err} role="alert" className="rounded-md border border-err/30 bg-err-dim px-3 py-2 text-[13px] text-err">
              {error}
            </p>
          )}
          <div className="flex items-center justify-end gap-2 border-t border-line pt-4">
            <button type="button" onClick={() => router.back()} className="h-8 rounded-md px-3 text-[13px] text-muted hover:bg-hover hover:text-fg">
              Cancel
            </button>
            <button
              type="submit"
              disabled={submitting}
              className="inline-flex h-8 items-center gap-2 rounded-md bg-accent px-3.5 text-[13px] font-medium text-accent-ink transition-opacity hover:opacity-90 disabled:opacity-60"
            >
              {submitting && <Spinner />}
              {submitting ? "Starting…" : "Start investigation"}
            </button>
          </div>
        </form>
      </Card>
    </div>
  );
}
