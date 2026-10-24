"use client";

// Live investigation events (PLAN.md §4.7): a pure reducer that folds
// core.Event values into a view state, and a hook that feeds it from the SSE
// endpoint (or from the bundled replay in mock mode).

import { useEffect, useReducer, useRef, useState } from "react";
import {
  type CulpritEvent,
  type Evidence,
  type Incident,
  type Report,
  MOCK,
  eventsUrl,
  isObj,
  parseEvent,
  parseEvidence,
  parseIncident,
  parseReport,
  str,
} from "./api";

export const EVENT_TYPES = ["started", "triage", "thinking", "tool_call", "tool_result", "evidence", "report", "error", "finished"] as const;

export interface ToolResult {
  evidence_id?: string;
  summary: string;
  error?: string;
  at: string;
}

export type Step =
  | { kind: "started"; seq: number; at: string; incident: Incident }
  | { kind: "triage"; seq: number; at: string; summary: string; evidence_ids: string[] }
  | { kind: "thinking"; seq: number; at: string; text: string }
  | { kind: "tool"; seq: number; at: string; callId: string; name: string; args: unknown; result?: ToolResult }
  | { kind: "error"; seq: number; at: string; message: string }
  | { kind: "report"; seq: number; at: string; hypotheses: number }
  | { kind: "finished"; seq: number; at: string; status: string };

export type Phase = "waiting" | "triage" | "investigating" | "reporting" | "done";

export interface ViewState {
  lastSeq: number;
  incident?: Incident;
  steps: Step[];
  evidence: Record<string, Evidence>;
  evidenceOrder: string[];
  report?: Report;
  errors: string[];
  finished: boolean;
  finalStatus?: string;
  phase: Phase;
}

export const initialViewState: ViewState = {
  lastSeq: 0,
  steps: [],
  evidence: {},
  evidenceOrder: [],
  errors: [],
  finished: false,
  phase: "waiting",
};

export type Action = { type: "event"; event: CulpritEvent } | { type: "reset" };

function addEvidence(state: ViewState, ev: Evidence): Pick<ViewState, "evidence" | "evidenceOrder"> {
  if (!ev.id) return { evidence: state.evidence, evidenceOrder: state.evidenceOrder };
  const known = ev.id in state.evidence;
  return {
    evidence: { ...state.evidence, [ev.id]: ev },
    evidenceOrder: known ? state.evidenceOrder : [...state.evidenceOrder, ev.id],
  };
}

export function applyEvent(state: ViewState, e: CulpritEvent): ViewState {
  // Replays after a reconnect may repeat events we already folded in.
  if (e.seq > 0 && e.seq <= state.lastSeq) return state;
  const base: ViewState = { ...state, lastSeq: e.seq > 0 ? e.seq : state.lastSeq };
  const d = isObj(e.data) ? e.data : {};
  const meta = { seq: e.seq, at: e.at };

  switch (e.type) {
    case "started": {
      const incident = parseIncident(e.data);
      return { ...base, incident, phase: "triage", steps: [...base.steps, { kind: "started", ...meta, incident }] };
    }
    case "triage":
      return {
        ...base,
        phase: "investigating",
        steps: [
          ...base.steps,
          {
            kind: "triage",
            ...meta,
            summary: str(d.summary),
            evidence_ids: Array.isArray(d.evidence_ids) ? d.evidence_ids.map((x) => str(x)).filter(Boolean) : [],
          },
        ],
      };
    case "thinking": {
      const text = str(d.text).trim();
      if (!text) return base;
      return { ...base, phase: base.phase === "waiting" || base.phase === "triage" ? "investigating" : base.phase, steps: [...base.steps, { kind: "thinking", ...meta, text }] };
    }
    case "tool_call": {
      const name = str(d.name, "tool");
      return {
        ...base,
        phase: name === "submit_report" ? "reporting" : "investigating",
        steps: [...base.steps, { kind: "tool", ...meta, callId: str(d.id), name, args: d.arguments ?? {} }],
      };
    }
    case "tool_result": {
      const callId = str(d.id);
      const result: ToolResult = {
        evidence_id: str(d.evidence_id) || undefined,
        summary: str(d.summary),
        error: str(d.error) || undefined,
        at: e.at,
      };
      let idx = -1;
      if (callId) {
        for (let i = base.steps.length - 1; i >= 0; i--) {
          const s = base.steps[i];
          if (s && s.kind === "tool" && s.callId === callId) {
            idx = i;
            break;
          }
        }
      }
      if (idx === -1) {
        return { ...base, steps: [...base.steps, { kind: "tool", ...meta, callId, name: str(d.name, "tool"), args: {}, result }] };
      }
      const steps = base.steps.slice();
      const prev = steps[idx];
      if (prev && prev.kind === "tool") steps[idx] = { ...prev, result };
      return { ...base, steps };
    }
    case "evidence": {
      const ev = parseEvidence(e.data);
      return { ...base, ...addEvidence(base, ev) };
    }
    case "report": {
      const report = parseReport(e.data);
      let next: ViewState = { ...base, report, phase: "reporting", steps: [...base.steps, { kind: "report", ...meta, hypotheses: report.hypotheses.length }] };
      for (const ev of report.evidence) next = { ...next, ...addEvidence(next, ev) };
      return next;
    }
    case "error": {
      const message = str(d.message, typeof e.data === "string" ? e.data : "Unknown error");
      return { ...base, errors: [...base.errors, message], steps: [...base.steps, { kind: "error", ...meta, message }] };
    }
    case "finished": {
      const status = str(d.status, base.report?.status || "completed");
      return { ...base, finished: true, finalStatus: status, phase: "done", steps: [...base.steps, { kind: "finished", ...meta, status }] };
    }
    default:
      return base;
  }
}

export function reduce(state: ViewState, action: Action): ViewState {
  if (action.type === "reset") return initialViewState;
  return applyEvent(state, action.event);
}

export function foldEvents(events: CulpritEvent[]): ViewState {
  return events.reduce(applyEvent, initialViewState);
}

export type Connection = "idle" | "connecting" | "open" | "reconnecting" | "closed" | "unavailable";

const MAX_RETRIES = 8;

/**
 * Subscribes to an investigation's event stream. The server replays stored
 * events then streams live ones and ends after `finished`; the browser resends
 * Last-Event-ID on its own reconnects, and duplicate seqs are dropped by the
 * reducer when we have to reconnect manually.
 */
export function useInvestigationEvents(id: string, opts: { enabled: boolean; onFinished?: () => void }) {
  const [state, dispatch] = useReducer(reduce, initialViewState);
  const [connection, setConnection] = useState<Connection>("idle");
  const onFinished = useRef(opts.onFinished);
  useEffect(() => {
    onFinished.current = opts.onFinished;
  });

  const { enabled } = opts;
  useEffect(() => {
    dispatch({ type: "reset" });
    if (!enabled || !id) {
      setConnection("idle");
      return;
    }
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let es: EventSource | null = null;
    let retries = 0;

    const handle = (raw: string) => {
      let parsed: unknown;
      try {
        parsed = JSON.parse(raw);
      } catch {
        return;
      }
      const ev = parseEvent(parsed);
      if (!ev) return;
      dispatch({ type: "event", event: ev });
      if (ev.type === "finished") {
        stopped = true;
        es?.close();
        setConnection("closed");
        onFinished.current?.();
      }
    };

    if (MOCK) {
      setConnection("open");
      void import("./mock").then(({ mockStream, mockFinish }) => {
        if (stopped) return;
        const stream = mockStream(id);
        if (!stream) {
          setConnection("unavailable");
          return;
        }
        let i = 0;
        const next = () => {
          if (stopped || i >= stream.length) return;
          const item = stream[i++];
          if (!item) return;
          timer = setTimeout(() => {
            if (item.event.type === "finished") mockFinish(id);
            handle(JSON.stringify(item.event));
            next();
          }, item.delay);
        };
        next();
      });
      return () => {
        stopped = true;
        if (timer) clearTimeout(timer);
      };
    }

    if (typeof EventSource === "undefined") {
      setConnection("unavailable");
      return;
    }

    const connect = () => {
      setConnection(retries === 0 ? "connecting" : "reconnecting");
      es = new EventSource(eventsUrl(id));
      const listener = (m: MessageEvent) => handle(String(m.data));
      for (const t of EVENT_TYPES) es.addEventListener(t, listener as EventListener);
      es.onmessage = listener;
      es.onopen = () => {
        retries = 0;
        setConnection("open");
      };
      es.onerror = () => {
        if (stopped || !es) return;
        if (es.readyState === EventSource.CLOSED) {
          es.close();
          retries++;
          if (retries > MAX_RETRIES) {
            setConnection("unavailable");
            return;
          }
          setConnection("reconnecting");
          timer = setTimeout(connect, Math.min(15_000, 500 * 2 ** retries));
        } else {
          setConnection("reconnecting");
        }
      };
    };
    connect();

    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
      es?.close();
    };
  }, [id, enabled]);

  return { state, connection };
}

