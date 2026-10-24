// Typed client for the Culprit HTTP API (PLAN.md §4.8). Types mirror
// internal/core/core.go. Every response is parsed defensively: Go encodes nil
// slices as null and the backend may add fields, so parsers normalise rather
// than trust.

export const API_BASE = (process.env.NEXT_PUBLIC_CULPRIT_API || "http://localhost:8090").replace(/\/+$/, "");
export const MOCK = process.env.NEXT_PUBLIC_CULPRIT_MOCK === "1";

export type Status = "queued" | "running" | "completed" | "failed" | "invalid_report" | "budget_exceeded";
export const STATUSES: readonly Status[] = ["queued", "running", "completed", "failed", "invalid_report", "budget_exceeded"];
export const TERMINAL: ReadonlySet<string> = new Set(["completed", "failed", "invalid_report", "budget_exceeded"]);

export interface Window {
  start: string;
  end: string;
}

export interface Incident {
  id?: string;
  title: string;
  service?: string;
  started_at: string;
  window: Window;
  labels?: Record<string, string>;
  source?: string;
}

export interface Evidence {
  id: string;
  kind: string;
  tool: string;
  input?: unknown;
  summary: string;
  data?: unknown;
  at: string;
}

export interface Hypothesis {
  title: string;
  explanation: string;
  service?: string;
  category?: string;
  change_id?: string;
  confidence: number;
  evidence_ids: string[];
  next_steps: string[];
}

export interface TimelineEntry {
  t: string;
  text: string;
  evidence_ids: string[];
}

export interface Usage {
  steps: number;
  tool_calls: number;
  prompt_tokens: number;
  completion_tokens: number;
}

export interface Report {
  incident_id: string;
  status: string;
  summary: string;
  timeline: TimelineEntry[];
  hypotheses: Hypothesis[];
  evidence: Evidence[];
  usage: Usage;
  model: string;
  started_at: string;
  finished_at: string;
}

export interface Investigation {
  id: string;
  incident: Incident;
  status: string;
  report?: Report;
  error?: string;
  created_at: string;
  updated_at: string;
}

export interface InvestigationSummary {
  id: string;
  title: string;
  service?: string;
  status: string;
  created_at: string;
  updated_at: string;
  top_hypothesis?: string;
  confidence?: number;
}

export interface CulpritEvent {
  seq: number;
  type: string;
  at: string;
  data: unknown;
}

// ---- primitive coercion -------------------------------------------------

type Obj = Record<string, unknown>;

export function isObj(v: unknown): v is Obj {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
export function str(v: unknown, fallback = ""): string {
  return typeof v === "string" ? v : typeof v === "number" ? String(v) : fallback;
}
function optStr(v: unknown): string | undefined {
  const s = str(v);
  return s === "" ? undefined : s;
}
export function num(v: unknown, fallback = 0): number {
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string" && v.trim() !== "" && Number.isFinite(Number(v))) return Number(v);
  return fallback;
}
function strArr(v: unknown): string[] {
  return Array.isArray(v) ? v.map((x) => str(x)).filter((x) => x !== "") : [];
}
function arr<T>(v: unknown, f: (x: unknown) => T): T[] {
  return Array.isArray(v) ? v.map(f) : [];
}
function strMap(v: unknown): Record<string, string> | undefined {
  if (!isObj(v)) return undefined;
  const out: Record<string, string> = {};
  for (const [k, x] of Object.entries(v)) out[k] = str(x);
  return out;
}

// ---- parsers -------------------------------------------------------------

export function parseIncident(v: unknown): Incident {
  const o = isObj(v) ? v : {};
  const w = isObj(o.window) ? o.window : {};
  return {
    id: optStr(o.id),
    title: str(o.title, "Untitled incident"),
    service: optStr(o.service),
    started_at: str(o.started_at),
    window: { start: str(w.start), end: str(w.end) },
    labels: strMap(o.labels),
    source: optStr(o.source),
  };
}

export function parseEvidence(v: unknown): Evidence {
  const o = isObj(v) ? v : {};
  return {
    id: str(o.id),
    kind: str(o.kind),
    tool: str(o.tool),
    input: o.input ?? undefined,
    summary: str(o.summary),
    data: o.data ?? undefined,
    at: str(o.at),
  };
}

export function parseHypothesis(v: unknown): Hypothesis {
  const o = isObj(v) ? v : {};
  return {
    title: str(o.title, "Untitled hypothesis"),
    explanation: str(o.explanation),
    service: optStr(o.service),
    category: optStr(o.category),
    change_id: optStr(o.change_id),
    confidence: Math.min(1, Math.max(0, num(o.confidence))),
    evidence_ids: strArr(o.evidence_ids),
    next_steps: strArr(o.next_steps),
  };
}

export function parseUsage(v: unknown): Usage {
  const o = isObj(v) ? v : {};
  return {
    steps: num(o.steps),
    tool_calls: num(o.tool_calls),
    prompt_tokens: num(o.prompt_tokens),
    completion_tokens: num(o.completion_tokens),
  };
}

export function parseReport(v: unknown): Report {
  const o = isObj(v) ? v : {};
  return {
    incident_id: str(o.incident_id),
    status: str(o.status),
    summary: str(o.summary),
    timeline: arr(o.timeline, (x) => {
      const t = isObj(x) ? x : {};
      return { t: str(t.t), text: str(t.text), evidence_ids: strArr(t.evidence_ids) };
    }),
    hypotheses: arr(o.hypotheses, parseHypothesis),
    evidence: arr(o.evidence, parseEvidence).filter((e) => e.id !== ""),
    usage: parseUsage(o.usage),
    model: str(o.model),
    started_at: str(o.started_at),
    finished_at: str(o.finished_at),
  };
}

export function parseInvestigation(v: unknown): Investigation {
  const o = isObj(v) ? v : {};
  const report = isObj(o.report) ? parseReport(o.report) : undefined;
  return {
    id: str(o.id),
    incident: parseIncident(o.incident),
    status: str(o.status, report?.status || "queued"),
    report,
    error: optStr(o.error),
    created_at: str(o.created_at),
    updated_at: str(o.updated_at, str(o.created_at)),
  };
}

// The summary shape is not pinned by the plan, so accept the obvious variants:
// flat fields, a nested incident, a top hypothesis as text or as an object.
export function parseSummary(v: unknown): InvestigationSummary {
  const o = isObj(v) ? v : {};
  const inc = isObj(o.incident) ? o.incident : {};
  const rep = isObj(o.report) ? o.report : {};
  let top: unknown = o.top_hypothesis ?? o.top;
  if (top === undefined && Array.isArray(rep.hypotheses)) top = rep.hypotheses[0];
  let topTitle: string | undefined;
  let confidence: number | undefined = o.confidence === undefined || o.confidence === null ? undefined : num(o.confidence);
  if (isObj(top)) {
    topTitle = optStr(top.title);
    if (confidence === undefined && top.confidence !== undefined) confidence = num(top.confidence);
  } else {
    topTitle = optStr(top);
  }
  if (confidence === undefined && o.top_confidence !== undefined) confidence = num(o.top_confidence);
  return {
    id: str(o.id),
    title: str(o.title, str(inc.title, "Untitled incident")),
    service: optStr(o.service) ?? optStr(inc.service),
    status: str(o.status, "queued"),
    created_at: str(o.created_at),
    updated_at: str(o.updated_at, str(o.created_at)),
    top_hypothesis: topTitle,
    confidence: confidence === undefined ? undefined : Math.min(1, Math.max(0, confidence)),
  };
}

export function parseSummaries(v: unknown): InvestigationSummary[] {
  const list = Array.isArray(v) ? v : isObj(v) && Array.isArray(v.investigations) ? v.investigations : [];
  return list.map(parseSummary).filter((s) => s.id !== "");
}

export function parseEvent(v: unknown): CulpritEvent | null {
  if (!isObj(v)) return null;
  const type = str(v.type);
  if (!type) return null;
  return { seq: num(v.seq), type, at: str(v.at), data: v.data ?? null };
}

// ---- requests ------------------------------------------------------------

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function request(path: string, init?: RequestInit): Promise<unknown> {
  let res: Response;
  try {
    res = await fetch(`${API_BASE}${path}`, {
      ...init,
      headers: { Accept: "application/json", ...(init?.body ? { "Content-Type": "application/json" } : {}), ...init?.headers },
      cache: "no-store",
    });
  } catch {
    throw new ApiError(`Cannot reach the Culprit API at ${API_BASE}. Is \`culprit serve\` running?`, 0);
  }
  const text = await res.text();
  let body: unknown = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  if (!res.ok) {
    const msg = isObj(body) ? str(body.error, str(body.message)) : typeof body === "string" ? body.trim() : "";
    throw new ApiError(msg || `${res.status} ${res.statusText}`, res.status);
  }
  return body;
}

export interface NewInvestigation {
  title: string;
  service?: string;
  started_at: string;
  window: Window;
  labels?: Record<string, string>;
  source?: string;
}

export function buildIncident(input: { title: string; service?: string; start: Date; lookbackMinutes: number; afterMinutes: number }): NewInvestigation {
  const start = input.start.getTime();
  return {
    title: input.title.trim(),
    service: input.service?.trim() || undefined,
    started_at: new Date(start).toISOString(),
    window: {
      start: new Date(start - input.lookbackMinutes * 60_000).toISOString(),
      end: new Date(start + input.afterMinutes * 60_000).toISOString(),
    },
    source: "manual",
  };
}

async function mockModule() {
  return import("./mock");
}

export async function listInvestigations(): Promise<InvestigationSummary[]> {
  if (MOCK) return (await mockModule()).mockList();
  return parseSummaries(await request("/api/investigations"));
}

export async function getInvestigation(id: string): Promise<Investigation> {
  if (MOCK) {
    const inv = (await mockModule()).mockGet(id);
    if (!inv) throw new ApiError("Investigation not found", 404);
    return inv;
  }
  return parseInvestigation(await request(`/api/investigations/${encodeURIComponent(id)}`));
}

export async function createInvestigation(inc: NewInvestigation): Promise<string> {
  if (MOCK) return (await mockModule()).mockCreate(inc);
  const body = await request("/api/investigations", { method: "POST", body: JSON.stringify(inc) });
  const id = isObj(body) ? str(body.id) : "";
  if (!id) throw new ApiError("The API accepted the incident but returned no id", 500);
  return id;
}

export function eventsUrl(id: string): string {
  return `${API_BASE}/api/investigations/${encodeURIComponent(id)}/events`;
}

export const webhookUrl = `${API_BASE}/api/webhooks/alertmanager`;
