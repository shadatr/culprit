import { describe, expect, it } from "vitest";
import { buildIncident, parseEvent, parseInvestigation, parseReport, parseSummaries, parseSummary } from "../api";

describe("parseReport", () => {
  it("normalises Go nil slices and clamps confidence", () => {
    const r = parseReport({
      incident_id: "abc",
      status: "completed",
      summary: "s",
      timeline: null,
      hypotheses: [{ title: "h", explanation: "e", confidence: 1.7, evidence_ids: null }],
      evidence: null,
      usage: { steps: 3, tool_calls: "4", prompt_tokens: 10, completion_tokens: 5 },
      model: "m",
      started_at: "2026-10-20T14:05:00Z",
      finished_at: "2026-10-20T14:06:00Z",
    });
    expect(r.timeline).toEqual([]);
    expect(r.evidence).toEqual([]);
    expect(r.hypotheses[0]?.confidence).toBe(1);
    expect(r.hypotheses[0]?.evidence_ids).toEqual([]);
    expect(r.hypotheses[0]?.next_steps).toEqual([]);
    expect(r.usage.tool_calls).toBe(4);
  });

  it("keeps evidence data opaque and drops evidence without id", () => {
    const r = parseReport({ evidence: [{ id: "E1", kind: "metric", data: { series: [] } }, { kind: "x" }] });
    expect(r.evidence).toHaveLength(1);
    expect(r.evidence[0]?.data).toEqual({ series: [] });
  });

  it("survives garbage", () => {
    const r = parseReport("nope");
    expect(r.hypotheses).toEqual([]);
    expect(r.usage.steps).toBe(0);
  });
});

describe("parseInvestigation", () => {
  it("parses the documented shape", () => {
    const inv = parseInvestigation({
      id: "i1",
      incident: { title: "High p99", service: "frontend", started_at: "2026-10-20T14:05:00Z", window: { start: "a", end: "b" } },
      status: "running",
      created_at: "2026-10-20T14:06:00Z",
      updated_at: "2026-10-20T14:07:00Z",
    });
    expect(inv.incident.title).toBe("High p99");
    expect(inv.incident.window).toEqual({ start: "a", end: "b" });
    expect(inv.report).toBeUndefined();
    expect(inv.error).toBeUndefined();
  });

  it("falls back to report status", () => {
    const inv = parseInvestigation({ id: "i", report: { status: "failed" } });
    expect(inv.status).toBe("failed");
  });
});

describe("parseSummary", () => {
  it("accepts flat fields", () => {
    const s = parseSummary({ id: "a", title: "t", service: "svc", status: "completed", created_at: "x", top_hypothesis: "N+1", confidence: 0.8 });
    expect(s).toMatchObject({ id: "a", title: "t", service: "svc", top_hypothesis: "N+1", confidence: 0.8 });
  });

  it("accepts nested incident and hypothesis object", () => {
    const s = parseSummary({ id: "a", incident: { title: "t", service: "svc" }, status: "completed", top_hypothesis: { title: "N+1", confidence: 0.7 } });
    expect(s).toMatchObject({ title: "t", service: "svc", top_hypothesis: "N+1", confidence: 0.7 });
  });

  it("reads the top hypothesis from an embedded report", () => {
    const s = parseSummary({ id: "a", title: "t", report: { hypotheses: [{ title: "first", confidence: 0.5 }] } });
    expect(s.top_hypothesis).toBe("first");
    expect(s.confidence).toBe(0.5);
  });

  it("list accepts arrays, wrapped arrays and null", () => {
    expect(parseSummaries(null)).toEqual([]);
    expect(parseSummaries({ investigations: [{ id: "x" }] })).toHaveLength(1);
    expect(parseSummaries([{ id: "x" }, { title: "no id" }])).toHaveLength(1);
  });
});

describe("parseEvent", () => {
  it("parses and rejects", () => {
    expect(parseEvent({ seq: 3, type: "thinking", at: "t", data: { text: "hi" } })).toEqual({ seq: 3, type: "thinking", at: "t", data: { text: "hi" } });
    expect(parseEvent({ seq: 1 })).toBeNull();
    expect(parseEvent(null)).toBeNull();
  });
});

describe("buildIncident", () => {
  it("computes the window from lookback and after", () => {
    const inc = buildIncident({ title: "  t ", service: "", start: new Date("2026-10-20T14:05:00Z"), lookbackMinutes: 120, afterMinutes: 30 });
    expect(inc.title).toBe("t");
    expect(inc.service).toBeUndefined();
    expect(inc.started_at).toBe("2026-10-20T14:05:00.000Z");
    expect(inc.window).toEqual({ start: "2026-10-20T12:05:00.000Z", end: "2026-10-20T14:35:00.000Z" });
  });
});
