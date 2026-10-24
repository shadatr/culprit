import { describe, expect, it } from "vitest";
import type { CulpritEvent } from "../api";
import { applyEvent, foldEvents, initialViewState } from "../events";
import { buildCatalogScenario } from "../mock";

let seq = 0;
const ev = (type: string, data: unknown): CulpritEvent => ({ seq: ++seq, type, at: `2026-10-20T14:06:${String(seq).padStart(2, "0")}Z`, data });

describe("event reducer", () => {
  it("folds a run into steps, evidence and report", () => {
    seq = 0;
    const s = foldEvents([
      ev("started", { title: "High p99", service: "frontend", started_at: "2026-10-20T14:05:00Z", window: { start: "a", end: "b" } }),
      ev("evidence", { id: "E1", kind: "anomaly", tool: "triage", summary: "3 anomalies" }),
      ev("triage", { summary: "catalog first", evidence_ids: ["E1"] }),
      ev("thinking", { text: "Look at catalog." }),
      ev("tool_call", { id: "c1", name: "get_metric", arguments: { selector: "x" } }),
      ev("evidence", { id: "E2", kind: "metric", tool: "get_metric", summary: "up" }),
      ev("tool_result", { id: "c1", name: "get_metric", evidence_id: "E2", summary: "up", error: "" }),
      ev("report", { status: "completed", summary: "done", hypotheses: [{ title: "h", confidence: 0.9, evidence_ids: ["E2"] }], evidence: [{ id: "E3", kind: "x", summary: "late" }] }),
      ev("finished", { status: "completed" }),
    ]);
    expect(s.incident?.title).toBe("High p99");
    expect(s.steps.map((x) => x.kind)).toEqual(["started", "triage", "thinking", "tool", "report", "finished"]);
    const tool = s.steps[3];
    expect(tool?.kind === "tool" && tool.result?.evidence_id).toBe("E2");
    expect(tool?.kind === "tool" && tool.args).toEqual({ selector: "x" });
    expect(s.evidenceOrder).toEqual(["E1", "E2", "E3"]);
    expect(s.report?.hypotheses).toHaveLength(1);
    expect(s.finished).toBe(true);
    expect(s.finalStatus).toBe("completed");
    expect(s.phase).toBe("done");
    expect(s.lastSeq).toBe(9);
  });

  it("tracks phases", () => {
    seq = 0;
    let s = applyEvent(initialViewState, ev("started", {}));
    expect(s.phase).toBe("triage");
    s = applyEvent(s, ev("triage", { summary: "x" }));
    expect(s.phase).toBe("investigating");
    s = applyEvent(s, ev("tool_call", { id: "r", name: "submit_report", arguments: {} }));
    expect(s.phase).toBe("reporting");
  });

  it("drops replayed events after a reconnect", () => {
    seq = 0;
    const a = ev("started", {});
    const b = ev("thinking", { text: "one" });
    let s = foldEvents([a, b]);
    s = applyEvent(s, a);
    s = applyEvent(s, b);
    expect(s.steps).toHaveLength(2);
    s = applyEvent(s, ev("thinking", { text: "two" }));
    expect(s.steps).toHaveLength(3);
  });

  it("keeps an unmatched tool_result and records errors", () => {
    seq = 0;
    const s = foldEvents([ev("tool_result", { id: "zz", name: "search_logs", error: "loki: 502" }), ev("error", { message: "budget" }), ev("finished", { status: "failed" })]);
    const t = s.steps[0];
    expect(t?.kind).toBe("tool");
    expect(t?.kind === "tool" && t.result?.error).toBe("loki: 502");
    expect(s.errors).toEqual(["budget"]);
    expect(s.finalStatus).toBe("failed");
  });

  it("ignores unknown types and empty thinking", () => {
    seq = 0;
    const s = foldEvents([ev("mystery", {}), ev("thinking", { text: "  " })]);
    expect(s.steps).toHaveLength(0);
    expect(s.lastSeq).toBe(2);
  });

  it("replays the bundled sample stream consistently", () => {
    const { stream } = buildCatalogScenario({ id: "x", start: Date.parse("2026-10-20T14:05:00Z"), now: Date.parse("2026-10-20T14:35:00Z") });
    const s = foldEvents(stream.map((i) => i.event));
    expect(s.finished).toBe(true);
    expect(s.evidenceOrder.length).toBeGreaterThanOrEqual(10);
    const tools = s.steps.filter((x) => x.kind === "tool");
    expect(tools.every((t) => t.kind === "tool" && t.result)).toBe(true);
    expect(s.report?.hypotheses[0]?.change_id).toBe("deploy-catalog-1.8.2");
  });
});
