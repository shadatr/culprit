// Evidence.Data is an opaque payload owned by the backend tools. This module
// recognises the shapes produced by the analysis packages (anomaly.Stats,
// anomaly.Anomaly, logpattern.Shift/Template, correlate.ScoredChange/
// ScoredService, core.Series/Change/LogLine) wherever they appear in the
// payload, so renderers stay useful even if the envelope changes.

import { type Evidence, isObj, num, str } from "./api";

type Obj = Record<string, unknown>;

export interface PointView {
  t: number; // epoch ms
  v: number;
}
export interface SeriesView {
  key: string;
  name: string;
  labels: Record<string, string>;
  points: PointView[];
}
export interface AnomalyView {
  metric: string;
  labels: Record<string, string>;
  service?: string;
  direction: string;
  onset?: number;
  baseline: number;
  peak: number;
  score: number;
}
export interface StatsView {
  metric: string;
  labels: Record<string, string>;
  before_mean: number;
  before_p95: number;
  after_mean: number;
  after_max: number;
  change_percent: number;
  anomaly?: AnomalyView;
}
export interface TemplateView {
  pattern: string;
  count: number;
  level?: string;
  service?: string;
  sample: string;
  first?: number;
  last?: number;
}
export interface ShiftView {
  template: TemplateView;
  baseline_rate: number;
  incident_rate: number;
  isNew: boolean;
  score: number;
  first_in_incident?: number;
}
export interface ChangeView {
  id: string;
  kind: string;
  service?: string;
  t?: number;
  author?: string;
  summary: string;
  diff?: string;
  url?: string;
}
export interface ScoredChangeView {
  change: ChangeView;
  score?: number;
  reasons: string[];
}
export interface SuspectView {
  service: string;
  score: number;
  earliest_onset?: number;
  reasons: string[];
}
export interface LogLineView {
  t?: number;
  service: string;
  level?: string;
  message: string;
}

export type EvidenceView =
  | { kind: "metric"; series: SeriesView[]; stats: StatsView[]; anomalies: AnomalyView[] }
  | { kind: "anomalies"; stats: StatsView[]; anomalies: AnomalyView[] }
  | { kind: "shifts"; shifts: ShiftView[] }
  | { kind: "templates"; templates: TemplateView[]; lines: LogLineView[] }
  | { kind: "lines"; lines: LogLineView[] }
  | { kind: "changes"; changes: ScoredChangeView[] }
  | { kind: "suspects"; suspects: SuspectView[] }
  | { kind: "topology"; edges: Record<string, string[]> }
  | { kind: "json" };

// ---- time ------------------------------------------------------------------

/** Accepts RFC 3339 strings, epoch seconds or epoch milliseconds. */
export function toMs(v: unknown): number | undefined {
  if (typeof v === "number" && Number.isFinite(v)) return v < 1e12 ? v * 1000 : v;
  if (typeof v === "string" && v !== "") {
    if (/^\d+(\.\d+)?$/.test(v)) return toMs(Number(v));
    const ms = Date.parse(v);
    if (Number.isNaN(ms)) return undefined;
    // Go's zero time ("0001-01-01T00:00:00Z") means "unset".
    return ms < 0 ? undefined : ms;
  }
  return undefined;
}

// ---- shape predicates ------------------------------------------------------

const isSeries = (o: Obj) => Array.isArray(o.points);
const isStats = (o: Obj) => typeof o.before_mean === "number" || typeof o.after_mean === "number";
const isAnomaly = (o: Obj) => "onset" in o && "score" in o && ("direction" in o || "peak" in o);
const isShift = (o: Obj) => isObj(o.template) && ("incident_rate" in o || "baseline_rate" in o);
const isScoredChange = (o: Obj) => isObj(o.change) && "score" in o;
const isChange = (o: Obj) => typeof o.id === "string" && typeof o.summary === "string" && ("kind" in o || "t" in o || "diff" in o);
const isSuspect = (o: Obj) => typeof o.service === "string" && "score" in o && ("reasons" in o || "earliest_onset" in o) && !("change" in o) && !("metric" in o);
const isTemplate = (o: Obj) => typeof o.pattern === "string" && "count" in o;
const isLogLine = (o: Obj) => typeof o.message === "string" && "t" in o;

/** Finds objects matching pred anywhere in v (not descending into matches). */
export function collect(v: unknown, pred: (o: Obj) => boolean, depth = 4): Obj[] {
  const out: Obj[] = [];
  const walk = (x: unknown, d: number) => {
    if (d < 0) return;
    if (Array.isArray(x)) {
      for (const item of x) walk(item, d - 1);
    } else if (isObj(x)) {
      if (pred(x)) {
        out.push(x);
        return;
      }
      for (const val of Object.values(x)) walk(val, d - 1);
    }
  };
  walk(v, depth);
  return out;
}

// ---- normalisers -----------------------------------------------------------

function labels(v: unknown): Record<string, string> {
  if (!isObj(v)) return {};
  const out: Record<string, string> = {};
  for (const [k, x] of Object.entries(v)) out[k] = str(x);
  return out;
}

function points(v: unknown): PointView[] {
  if (!Array.isArray(v)) return [];
  const out: PointView[] = [];
  for (const p of v) {
    let t: number | undefined;
    let val: number;
    if (Array.isArray(p)) {
      t = toMs(p[0]);
      val = num(p[1], NaN);
    } else if (isObj(p)) {
      t = toMs(p.t ?? p.time ?? p.ts);
      val = num(p.v ?? p.value, NaN);
    } else continue;
    if (t !== undefined && Number.isFinite(val)) out.push({ t, v: val });
  }
  return out.sort((a, b) => a.t - b.t);
}

export function labelText(l: Record<string, string>): string {
  const parts = Object.entries(l)
    .filter(([k]) => k !== "__name__")
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}="${v}"`);
  return parts.length ? `{${parts.join(",")}}` : "";
}

function anomaly(o: Obj): AnomalyView {
  const l = labels(o.labels);
  return {
    metric: str(o.metric),
    labels: l,
    service: str(o.service) || l.service || undefined,
    direction: str(o.direction, "up"),
    onset: toMs(o.onset),
    baseline: num(o.baseline),
    peak: num(o.peak),
    score: num(o.score),
  };
}

function stats(o: Obj): StatsView {
  return {
    metric: str(o.metric),
    labels: labels(o.labels),
    before_mean: num(o.before_mean),
    before_p95: num(o.before_p95),
    after_mean: num(o.after_mean),
    after_max: num(o.after_max),
    change_percent: num(o.change_percent),
    anomaly: isObj(o.anomaly) ? anomaly(o.anomaly) : undefined,
  };
}

function template(o: Obj): TemplateView {
  return {
    pattern: str(o.pattern),
    count: num(o.count),
    level: str(o.level) || undefined,
    service: str(o.service) || undefined,
    sample: str(o.sample),
    first: toMs(o.first),
    last: toMs(o.last),
  };
}

function change(o: Obj): ChangeView {
  return {
    id: str(o.id),
    kind: str(o.kind, "change"),
    service: str(o.service) || undefined,
    t: toMs(o.t),
    author: str(o.author) || undefined,
    summary: str(o.summary),
    diff: str(o.diff) || undefined,
    url: str(o.url) || undefined,
  };
}

function reasons(v: unknown): string[] {
  return Array.isArray(v) ? v.map((x) => str(x)).filter(Boolean) : [];
}

// ---- classification --------------------------------------------------------

export function seriesKey(name: string, l: Record<string, string>): string {
  return `${name}${labelText(l)}`;
}

export function classifyEvidence(ev: Pick<Evidence, "kind" | "data">): EvidenceView {
  const data = ev.data;
  if (data === undefined || data === null) return { kind: "json" };

  if (ev.kind === "topology" && isObj(data)) {
    const src = isObj(data.topology) ? data.topology : data;
    const entries = Object.entries(src);
    if (entries.length && entries.every(([, v]) => Array.isArray(v) || v === null)) {
      const edges: Record<string, string[]> = {};
      for (const [k, v] of entries) edges[k] = reasons(v);
      return { kind: "topology", edges };
    }
  }

  const series = collect(data, isSeries).map((o): SeriesView => {
    const name = str(o.name, str(o.metric, "series"));
    const l = labels(o.labels);
    return { key: seriesKey(name, l), name, labels: l, points: points(o.points) };
  });
  const st = collect(data, isStats).map(stats);
  const an = collect(data, (o) => isAnomaly(o) && !isStats(o)).map(anomaly);
  // Anomalies nested in stats count too.
  for (const s of st) if (s.anomaly && !an.some((a) => a.metric === s.anomaly?.metric && labelText(a.labels) === labelText(s.anomaly.labels))) an.push(s.anomaly);

  if (series.some((s) => s.points.length > 0)) return { kind: "metric", series: series.filter((s) => s.points.length > 0), stats: st, anomalies: an };

  const shifts = collect(data, isShift).map(
    (o): ShiftView => ({
      template: template(isObj(o.template) ? o.template : {}),
      baseline_rate: num(o.baseline_rate),
      incident_rate: num(o.incident_rate),
      isNew: o.new === true,
      score: num(o.score),
      first_in_incident: toMs(o.first_in_incident),
    }),
  );
  if (shifts.length) return { kind: "shifts", shifts };

  const scored = collect(data, isScoredChange).map(
    (o): ScoredChangeView => ({ change: change(isObj(o.change) ? o.change : {}), score: num(o.score), reasons: reasons(o.reasons) }),
  );
  if (scored.length) return { kind: "changes", changes: scored };
  const plain = collect(data, isChange).map((o): ScoredChangeView => ({ change: change(o), reasons: [] }));
  if (plain.length) return { kind: "changes", changes: plain };

  const suspects = collect(data, isSuspect).map(
    (o): SuspectView => ({ service: str(o.service), score: num(o.score), earliest_onset: toMs(o.earliest_onset), reasons: reasons(o.reasons) }),
  );
  if (suspects.length) return { kind: "suspects", suspects: suspects.sort((a, b) => b.score - a.score) };

  if (st.length || an.length) return { kind: "anomalies", stats: st, anomalies: an.sort((a, b) => b.score - a.score) };

  const lines = collect(data, isLogLine).map(
    (o): LogLineView => ({ t: toMs(o.t), service: str(o.service), level: str(o.level) || undefined, message: str(o.message) }),
  );
  const templates = collect(data, isTemplate).map(template);
  if (templates.length) return { kind: "templates", templates, lines };
  if (lines.length) return { kind: "lines", lines };

  return { kind: "json" };
}

/** Splits a log pattern into literal and wildcard segments for highlighting. */
export function patternSegments(pattern: string): { text: string; wild: boolean }[] {
  const out: { text: string; wild: boolean }[] = [];
  const parts = pattern.split(/(<\*>)/);
  for (const p of parts) {
    if (p === "") continue;
    out.push({ text: p, wild: p === "<*>" });
  }
  return out;
}
