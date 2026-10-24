"use client";

import { CartesianGrid, Line, LineChart, ReferenceArea, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { type AnomalyView, type SeriesView, type StatsView, labelText, seriesKey } from "@/lib/evidence";
import { clockUTC, metricValue, shortClockUTC } from "@/lib/format";

const COLORS = ["var(--c-1)", "var(--c-2)", "var(--c-3)", "var(--c-4)"];
const MAX_CHARTS = 6;

function matchStats(s: SeriesView, stats: StatsView[]): StatsView | undefined {
  const exact = stats.find((st) => seriesKey(st.metric, st.labels) === s.key);
  if (exact) return exact;
  if (stats.length === 1) return stats[0];
  return stats.find((st) => st.metric === s.name && (st.labels.service ?? "") === (s.labels.service ?? ""));
}

function matchAnomaly(s: SeriesView, st: StatsView | undefined, an: AnomalyView[]): AnomalyView | undefined {
  if (st?.anomaly) return st.anomaly;
  return an.find((a) => seriesKey(a.metric, a.labels) === s.key) ?? an.find((a) => a.metric === s.name && (a.labels.service ?? a.service ?? "") === (s.labels.service ?? ""));
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="min-w-0">
      <dt className="text-[10px] tracking-wide text-faint uppercase">{label}</dt>
      <dd className={`font-mono text-[12px] tabular ${tone ?? "text-fg-2"}`}>{value}</dd>
    </div>
  );
}

function SeriesChart({ s, color, stats, anomaly, incidentStart }: { s: SeriesView; color: string; stats?: StatsView; anomaly?: AnomalyView; incidentStart?: number }) {
  const data = s.points.map((p) => ({ t: p.t, v: p.v }));
  const first = data[0]?.t ?? 0;
  const last = data[data.length - 1]?.t ?? 0;
  const inRange = (t?: number) => t !== undefined && t >= first && t <= last;
  const band = stats && stats.before_p95 > 0 ? { y1: Math.min(stats.before_mean, stats.before_p95), y2: Math.max(stats.before_mean, stats.before_p95) } : null;
  const change = stats?.change_percent;
  const desc = [
    `${s.name}${labelText(s.labels)}`,
    stats ? `before mean ${metricValue(stats.before_mean)}, after mean ${metricValue(stats.after_mean)}` : "",
    anomaly?.onset ? `onset at ${clockUTC(anomaly.onset)} UTC` : "",
  ]
    .filter(Boolean)
    .join("; ");

  return (
    <figure className="rounded-md border border-line bg-panel">
      <figcaption className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 border-b border-line px-3 py-2">
        <span className="min-w-0 font-mono text-[12px] break-all text-fg">
          {s.name}
          <span className="text-muted">{labelText(s.labels)}</span>
        </span>
        {anomaly && (
          <span className="text-[11px] whitespace-nowrap text-muted">
            anomaly {anomaly.direction} · score <span className="tabular">{anomaly.score.toFixed(1)}</span>
          </span>
        )}
      </figcaption>
      <div className="h-44 px-1 pt-2" role="img" aria-label={desc}>
        <ResponsiveContainer width="100%" height="100%">
          <LineChart data={data} margin={{ top: 14, right: 12, bottom: 0, left: 0 }}>
            <CartesianGrid stroke="var(--c-grid)" vertical={false} />
            <XAxis
              dataKey="t"
              type="number"
              scale="time"
              domain={["dataMin", "dataMax"]}
              tickFormatter={(t: number) => shortClockUTC(t)}
              tick={{ fill: "var(--c-axis)", fontSize: 10 }}
              stroke="var(--c-grid)"
              tickLine={false}
              minTickGap={40}
            />
            <YAxis width={48} tickFormatter={(v: number) => metricValue(v)} tick={{ fill: "var(--c-axis)", fontSize: 10 }} stroke="var(--c-grid)" tickLine={false} axisLine={false} />
            {band && <ReferenceArea y1={band.y1} y2={band.y2} fill="var(--c-band)" fillOpacity={1} stroke="none" ifOverflow="extendDomain" />}
            {inRange(incidentStart) && (
              <ReferenceLine x={incidentStart} stroke="var(--c-incident)" strokeWidth={1.25} />
            )}
            {inRange(anomaly?.onset) && (
              <ReferenceLine x={anomaly?.onset} stroke="var(--fg-2)" strokeDasharray="3 3" />
            )}
            <Tooltip
              cursor={{ stroke: "var(--line-strong)" }}
              isAnimationActive={false}
              content={({ active, payload }) => {
                const p = active ? payload?.[0]?.payload : undefined;
                if (!p) return null;
                return (
                  <div className="rounded-md border border-line-strong bg-panel px-2.5 py-1.5 text-[11px] shadow-pop">
                    <div className="text-muted tabular">{clockUTC(p.t as number)} UTC</div>
                    <div className="font-mono text-fg tabular">{metricValue(p.v as number)}</div>
                  </div>
                );
              }}
            />
            <Line type="monotone" dataKey="v" stroke={color} strokeWidth={1.6} dot={false} isAnimationActive={false} activeDot={{ r: 3 }} />
          </LineChart>
        </ResponsiveContainer>
      </div>
      {stats && (
        <dl className="grid grid-cols-3 gap-x-3 gap-y-2 border-t border-line px-3 py-2 sm:grid-cols-5">
          <Stat label="Before mean" value={metricValue(stats.before_mean)} />
          <Stat label="Before p95" value={metricValue(stats.before_p95)} />
          <Stat label="After mean" value={metricValue(stats.after_mean)} />
          <Stat label="After max" value={metricValue(stats.after_max)} />
          <Stat
            label="Change"
            value={change === undefined ? "–" : `${change > 0 ? "+" : ""}${change.toFixed(Math.abs(change) < 10 ? 1 : 0)}%`}
            tone={change !== undefined && Math.abs(change) >= 50 ? "text-err" : undefined}
          />
        </dl>
      )}
      <div className="flex flex-wrap gap-x-4 gap-y-1 px-3 pb-2 text-[10px] text-muted">
        {inRange(incidentStart) && (
          <span className="inline-flex items-center gap-1.5">
            <span aria-hidden="true" className="h-2.5 w-px bg-[var(--c-incident)]" /> incident start
          </span>
        )}
        {inRange(anomaly?.onset) && (
          <span className="inline-flex items-center gap-1.5">
            <span aria-hidden="true" className="h-2.5 w-0 border-l border-dashed border-fg-2" /> onset {clockUTC(anomaly?.onset)}
          </span>
        )}
        {band && (
          <span className="inline-flex items-center gap-1.5">
            <span aria-hidden="true" className="h-2 w-3 rounded-sm bg-[var(--c-band)]" /> baseline mean–p95
          </span>
        )}
      </div>
    </figure>
  );
}

export function MetricView({ series, stats, anomalies, incidentStart }: { series: SeriesView[]; stats: StatsView[]; anomalies: AnomalyView[]; incidentStart?: number }) {
  const shown = series.slice(0, MAX_CHARTS);
  return (
    <div className="space-y-3">
      {shown.map((s, i) => {
        const st = matchStats(s, stats);
        return <SeriesChart key={s.key + i} s={s} color={COLORS[i % COLORS.length] ?? "var(--c-1)"} stats={st} anomaly={matchAnomaly(s, st, anomalies)} incidentStart={incidentStart} />;
      })}
      {series.length > MAX_CHARTS && <p className="text-[12px] text-muted">{series.length - MAX_CHARTS} more series not shown; see raw data below.</p>}
    </div>
  );
}

export function AnomalyTable({ stats, anomalies }: { stats: StatsView[]; anomalies: AnomalyView[] }) {
  return (
    <div className="space-y-4">
      {anomalies.length > 0 && (
        <div className="overflow-x-auto rounded-md border border-line">
          <table className="w-full min-w-[480px] text-[12px]">
            <caption className="sr-only">Anomalous series</caption>
            <thead className="bg-panel-2 text-left text-[10px] tracking-wide text-faint uppercase">
              <tr>
                <th scope="col" className="px-3 py-1.5 font-medium">Series</th>
                <th scope="col" className="px-3 py-1.5 font-medium">Onset (UTC)</th>
                <th scope="col" className="px-3 py-1.5 text-right font-medium">Baseline → peak</th>
                <th scope="col" className="px-3 py-1.5 text-right font-medium">Score</th>
              </tr>
            </thead>
            <tbody>
              {anomalies.map((a, i) => (
                <tr key={i} className="border-t border-line">
                  <td className="px-3 py-2 font-mono break-all">
                    {a.metric}
                    <span className="text-muted">{labelText(a.labels)}</span>
                  </td>
                  <td className="px-3 py-2 tabular">{clockUTC(a.onset)}</td>
                  <td className="px-3 py-2 text-right font-mono tabular whitespace-nowrap">
                    {metricValue(a.baseline)} <span className={a.direction === "down" ? "text-ok" : "text-err"}>{a.direction === "down" ? "↓" : "↑"}</span> {metricValue(a.peak)}
                  </td>
                  <td className="px-3 py-2 text-right tabular">{a.score.toFixed(1)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {stats.length > 0 && anomalies.length === 0 && (
        <ul className="space-y-1 text-[12px]">
          {stats.map((s, i) => (
            <li key={i} className="font-mono">
              {s.metric}
              {labelText(s.labels)}: {metricValue(s.before_mean)} → {metricValue(s.after_mean)} ({s.change_percent.toFixed(1)}%)
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
