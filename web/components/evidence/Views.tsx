"use client";

import { type LogLineView, type ScoredChangeView, type ShiftView, type SuspectView, type TemplateView, patternSegments } from "@/lib/evidence";
import { clockUTC, compact, rate, stampUTC } from "@/lib/format";
import { ConfidenceBar } from "@/components/ui";

export function Pattern({ pattern, className = "" }: { pattern: string; className?: string }) {
  return (
    <code className={`font-mono text-[12px] leading-5 break-words text-fg ${className}`}>
      {patternSegments(pattern).map((seg, i) =>
        seg.wild ? (
          <span key={i} className="rounded-sm bg-accent-dim px-0.5 text-accent" aria-label="variable">
            {seg.text}
          </span>
        ) : (
          <span key={i}>{seg.text}</span>
        ),
      )}
    </code>
  );
}

function LevelTag({ level }: { level?: string }) {
  if (!level) return null;
  const l = level.toLowerCase();
  const tone = l.startsWith("err") || l === "fatal" || l === "critical" ? "text-err" : l.startsWith("warn") ? "text-warn" : "text-muted";
  return <span className={`font-mono text-[10px] uppercase ${tone}`}>{level}</span>;
}

function NewBadge() {
  return <span className="rounded border border-err/40 bg-err-dim px-1 text-[10px] leading-4 font-semibold tracking-wide text-err">NEW</span>;
}

function ratio(s: ShiftView): string {
  if (s.isNew || s.baseline_rate === 0) return s.incident_rate > 0 ? "new" : "–";
  const r = s.incident_rate / s.baseline_rate;
  return r >= 10 ? `×${Math.round(r)}` : `×${r.toFixed(1)}`;
}

export function ShiftTable({ shifts }: { shifts: ShiftView[] }) {
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="w-full min-w-[560px] text-[12px]">
        <caption className="sr-only">Log template shifts between baseline and incident</caption>
        <thead className="bg-panel-2 text-left text-[10px] tracking-wide text-faint uppercase">
          <tr>
            <th scope="col" className="px-3 py-1.5 font-medium">Pattern</th>
            <th scope="col" className="px-3 py-1.5 text-right font-medium whitespace-nowrap">Baseline</th>
            <th scope="col" className="px-3 py-1.5 text-right font-medium whitespace-nowrap">Incident</th>
            <th scope="col" className="px-3 py-1.5 text-right font-medium">Change</th>
          </tr>
        </thead>
        <tbody>
          {shifts.map((s, i) => (
            <tr key={s.template.pattern + i} className="border-t border-line align-top">
              <td className="px-3 py-2">
                <div className="mb-0.5 flex flex-wrap items-center gap-2">
                  {s.isNew && <NewBadge />}
                  <LevelTag level={s.template.level} />
                  {s.template.service && <span className="font-mono text-[10px] text-muted">{s.template.service}</span>}
                  {s.first_in_incident && <span className="text-[10px] text-faint tabular">first {clockUTC(s.first_in_incident)}</span>}
                </div>
                <Pattern pattern={s.template.pattern} />
                {s.template.sample && (
                  <p className="mt-1 font-mono text-[11px] break-all text-muted" title="Sample line">
                    {s.template.sample}
                  </p>
                )}
              </td>
              <td className="px-3 py-2 text-right font-mono tabular whitespace-nowrap text-muted">{rate(s.baseline_rate)}</td>
              <td className="px-3 py-2 text-right font-mono tabular whitespace-nowrap">{rate(s.incident_rate)}</td>
              <td className={`px-3 py-2 text-right font-mono tabular whitespace-nowrap ${s.isNew || s.incident_rate > s.baseline_rate * 2 ? "text-err" : "text-muted"}`}>{ratio(s)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function TemplateTable({ templates }: { templates: TemplateView[] }) {
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="w-full min-w-[440px] text-[12px]">
        <caption className="sr-only">Log templates</caption>
        <thead className="bg-panel-2 text-left text-[10px] tracking-wide text-faint uppercase">
          <tr>
            <th scope="col" className="px-3 py-1.5 font-medium">Template</th>
            <th scope="col" className="px-3 py-1.5 text-right font-medium">Count</th>
          </tr>
        </thead>
        <tbody>
          {templates.map((t, i) => (
            <tr key={t.pattern + i} className="border-t border-line align-top">
              <td className="px-3 py-2">
                <div className="mb-0.5 flex items-center gap-2">
                  <LevelTag level={t.level} />
                  {t.service && <span className="font-mono text-[10px] text-muted">{t.service}</span>}
                </div>
                <Pattern pattern={t.pattern} />
                {t.sample && <p className="mt-1 font-mono text-[11px] break-all text-muted">{t.sample}</p>}
              </td>
              <td className="px-3 py-2 text-right font-mono tabular">{compact(t.count)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function LogLines({ lines }: { lines: LogLineView[] }) {
  const shown = lines.slice(0, 50);
  return (
    <div className="overflow-x-auto rounded-md border border-line bg-panel-2">
      <ol className="min-w-max py-1.5 font-mono text-[11px] leading-5" aria-label="Log lines">
        {shown.map((l, i) => (
          <li key={i} className="flex gap-3 px-3">
            <span className="text-faint tabular">{clockUTC(l.t)}</span>
            {l.service && <span className="text-muted">{l.service}</span>}
            <LevelTag level={l.level} />
            <span className="text-fg-2">{l.message}</span>
          </li>
        ))}
      </ol>
      {lines.length > shown.length && <p className="border-t border-line px-3 py-1.5 text-[11px] text-muted">{lines.length - shown.length} more lines in raw data</p>}
    </div>
  );
}

function Diff({ diff }: { diff: string }) {
  const lines = diff.split("\n");
  return (
    <pre className="max-h-96 overflow-auto rounded-md border border-line bg-panel-2 py-1.5 font-mono text-[11px] leading-5" aria-label="Diff">
      <code className="block min-w-max">
        {lines.map((l, i) => {
          const cls = l.startsWith("+++") || l.startsWith("---") ? "text-muted" : l.startsWith("+") ? "bg-[var(--diff-add)] text-ok" : l.startsWith("-") ? "bg-[var(--diff-del)] text-err" : l.startsWith("@@") ? "text-accent" : "text-fg-2";
          return (
            <span key={i} className={`block px-3 ${cls}`}>
              {l || " "}
            </span>
          );
        })}
      </code>
    </pre>
  );
}

export function ChangesList({ changes, highlight }: { changes: ScoredChangeView[]; highlight?: string }) {
  return (
    <ol className="space-y-2.5">
      {changes.map(({ change: c, score, reasons }, i) => (
        <li key={c.id + i} className={`rounded-md border bg-panel p-3 ${highlight && c.id === highlight ? "border-accent-line" : "border-line"}`}>
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="rounded border border-line-strong px-1 font-mono text-[10px] text-muted uppercase">{c.kind}</span>
            <span className="font-mono text-[12px] font-medium break-all">{c.id}</span>
            {highlight && c.id === highlight && <span className="text-[10px] font-medium text-accent">suspected</span>}
            {score !== undefined && (
              <span className="ml-auto flex w-28 items-center gap-2">
                <ConfidenceBar value={score} className="flex-1" label={`Correlation score ${score.toFixed(2)}`} />
                <span className="font-mono text-[11px] text-muted tabular">{score.toFixed(2)}</span>
              </span>
            )}
          </div>
          <p className="mt-1 text-[13px] text-fg">{c.summary}</p>
          <p className="mt-0.5 text-[11px] text-muted tabular">
            {[c.service, c.t ? stampUTC(c.t) : "", c.author ? `by ${c.author}` : ""].filter(Boolean).join(" · ")}
            {c.url && (
              <>
                {" · "}
                <a href={c.url} target="_blank" rel="noreferrer noopener" className="text-accent underline-offset-2 hover:underline">
                  source
                </a>
              </>
            )}
          </p>
          {reasons.length > 0 && (
            <ul className="mt-2 space-y-0.5 text-[12px] text-fg-2">
              {reasons.map((r, j) => (
                <li key={j} className="flex gap-2">
                  <span aria-hidden="true" className="text-faint">
                    –
                  </span>
                  {r}
                </li>
              ))}
            </ul>
          )}
          {c.diff && (
            <details className="mt-2 group" open={changes.length === 1}>
              <summary className="cursor-pointer text-[12px] text-muted hover:text-fg">
                <span className="inline-block transition-transform group-open:rotate-90" aria-hidden="true">
                  ▸
                </span>{" "}
                Diff
              </summary>
              <div className="mt-1.5">
                <Diff diff={c.diff} />
              </div>
            </details>
          )}
        </li>
      ))}
    </ol>
  );
}

export function SuspectsList({ suspects }: { suspects: SuspectView[] }) {
  const max = Math.max(...suspects.map((s) => s.score), 0.0001);
  return (
    <ol className="space-y-3" aria-label="Suspect services, most likely first">
      {suspects.map((s, i) => (
        <li key={s.service + i}>
          <div className="flex items-baseline justify-between gap-3">
            <span className="font-mono text-[13px] font-medium">{s.service}</span>
            <span className="font-mono text-[12px] text-muted tabular">{s.score.toFixed(2)}</span>
          </div>
          <div className="mt-1 h-2 overflow-hidden rounded-sm bg-panel-2" aria-hidden="true">
            <div className={`h-full rounded-sm ${i === 0 ? "bg-accent" : "bg-fg-2/35"}`} style={{ width: `${Math.max(2, (s.score / max) * 100)}%` }} />
          </div>
          {(s.reasons.length > 0 || s.earliest_onset) && (
            <p className="mt-1 text-[12px] text-muted">
              {[s.earliest_onset ? `earliest onset ${clockUTC(s.earliest_onset)}` : "", ...s.reasons].filter(Boolean).join(" · ")}
            </p>
          )}
        </li>
      ))}
    </ol>
  );
}

export function TopologyView({ edges }: { edges: Record<string, string[]> }) {
  return (
    <ul className="space-y-1 font-mono text-[12px]">
      {Object.entries(edges).map(([from, to]) => (
        <li key={from} className="flex flex-wrap gap-x-2">
          <span className="font-medium">{from}</span>
          <span className="text-faint" aria-label="calls">
            →
          </span>
          <span className="text-fg-2">{to.length ? to.join(", ") : "nothing"}</span>
        </li>
      ))}
    </ul>
  );
}

export function JsonView({ value, label = "Raw JSON" }: { value: unknown; label?: string }) {
  let text: string;
  try {
    text = JSON.stringify(value, null, 2) ?? "null";
  } catch {
    text = String(value);
  }
  return (
    <pre className="max-h-[28rem] overflow-auto rounded-md border border-line bg-panel-2 px-3 py-2 font-mono text-[11px] leading-5 text-fg-2" aria-label={label}>
      <code>{text}</code>
    </pre>
  );
}
