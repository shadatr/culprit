// Small, dependency-free formatters shared by pages and the Markdown renderer.

export function parseTime(s: string | undefined): number | undefined {
  if (!s) return undefined;
  const ms = Date.parse(s);
  return Number.isNaN(ms) || ms < 0 ? undefined : ms;
}

export function relativeTime(s: string | undefined, now: number = Date.now()): string {
  const t = parseTime(s);
  if (t === undefined) return "unknown";
  const diff = Math.round((now - t) / 1000);
  const abs = Math.abs(diff);
  const suffix = diff >= 0 ? "ago" : "from now";
  if (abs < 10) return "just now";
  if (abs < 60) return `${abs}s ${suffix}`;
  if (abs < 3600) return `${Math.floor(abs / 60)}m ${suffix}`;
  if (abs < 86400) return `${Math.floor(abs / 3600)}h ${suffix}`;
  return `${Math.floor(abs / 86400)}d ${suffix}`;
}

export function durationMs(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "–";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s % 60);
  if (m < 60) return rem ? `${m}m ${rem}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function between(a: string | undefined, b: string | undefined): string {
  const x = parseTime(a);
  const y = parseTime(b);
  return x === undefined || y === undefined ? "–" : durationMs(y - x);
}

export function pct(v: number, digits = 0): string {
  return `${(v * 100).toFixed(digits)}%`;
}

export function compact(n: number): string {
  if (!Number.isFinite(n)) return "–";
  const abs = Math.abs(n);
  if (abs >= 1e6) return `${(n / 1e6).toFixed(abs >= 1e7 ? 0 : 1)}M`;
  if (abs >= 1e3) return `${(n / 1e3).toFixed(abs >= 1e4 ? 0 : 1)}k`;
  return String(Math.round(n));
}

/** A sensible number for metric values that range from ratios to bytes. */
export function metricValue(v: number): string {
  if (!Number.isFinite(v)) return "–";
  const abs = Math.abs(v);
  if (abs === 0) return "0";
  if (abs >= 1e9) return `${(v / 1e9).toFixed(2)}G`;
  if (abs >= 1e6) return `${(v / 1e6).toFixed(2)}M`;
  if (abs >= 1e4) return `${(v / 1e3).toFixed(1)}k`;
  if (abs >= 100) return v.toFixed(0);
  if (abs >= 1) return v.toFixed(2);
  if (abs >= 0.01) return v.toFixed(3);
  return v.toPrecision(2);
}

export function rate(v: number): string {
  if (!Number.isFinite(v)) return "–";
  if (v === 0) return "0/min";
  if (v < 0.1) return `${v.toFixed(2)}/min`;
  if (v < 10) return `${v.toFixed(1)}/min`;
  return `${Math.round(v)}/min`;
}

const pad = (n: number) => String(n).padStart(2, "0");

/** HH:MM:SS in UTC; incidents are discussed in UTC. */
export function clockUTC(ms: number | undefined): string {
  if (ms === undefined) return "–";
  const d = new Date(ms);
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}`;
}

export function shortClockUTC(ms: number): string {
  const d = new Date(ms);
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
}

/** 2026-10-20 14:05:00 UTC */
export function stampUTC(s: string | number | undefined): string {
  const t = typeof s === "number" ? s : parseTime(s);
  if (t === undefined) return "–";
  const d = new Date(t);
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${clockUTC(t)} UTC`;
}

export function statusLabel(s: string): string {
  switch (s) {
    case "invalid_report":
      return "invalid report";
    case "budget_exceeded":
      return "budget exceeded";
    default:
      return s || "unknown";
  }
}
