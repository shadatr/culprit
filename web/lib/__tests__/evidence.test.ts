import { describe, expect, it } from "vitest";
import { classifyEvidence, patternSegments, toMs } from "../evidence";
import { buildCatalogScenario } from "../mock";

describe("classifyEvidence", () => {
  it("recognises metric series with stats, tuple points included", () => {
    const v = classifyEvidence({
      kind: "metric",
      data: { series: [{ name: "m", labels: { service: "a" }, points: [[1760968800, 1], { t: "2026-10-20T14:05:00Z", v: 2 }] }], stats: [{ metric: "m", labels: { service: "a" }, before_mean: 1, anomaly: { metric: "m", labels: {}, onset: "2026-10-20T14:00:00Z", score: 4, direction: "up" } }] },
    });
    expect(v.kind).toBe("metric");
    if (v.kind !== "metric") return;
    expect(v.series[0]?.points).toHaveLength(2);
    expect(v.stats).toHaveLength(1);
    expect(v.anomalies[0]?.onset).toBe(Date.parse("2026-10-20T14:00:00Z"));
  });

  it("recognises shifts, scored changes, plain changes, suspects and falls back to json", () => {
    expect(classifyEvidence({ kind: "log_patterns", data: [{ template: { pattern: "a <*>" }, baseline_rate: 0, incident_rate: 3, new: true }] }).kind).toBe("shifts");
    expect(classifyEvidence({ kind: "changes", data: { changes: [{ change: { id: "c", summary: "s" }, score: 0.5, reasons: null }] } }).kind).toBe("changes");
    expect(classifyEvidence({ kind: "change", data: { id: "c", kind: "deploy", summary: "s", diff: "+x" } }).kind).toBe("changes");
    expect(classifyEvidence({ kind: "suspects", data: [{ service: "a", score: 0.2, reasons: [] }] }).kind).toBe("suspects");
    expect(classifyEvidence({ kind: "topology", data: { a: ["b"], b: null } }).kind).toBe("topology");
    expect(classifyEvidence({ kind: "x", data: { hello: "world" } }).kind).toBe("json");
    expect(classifyEvidence({ kind: "x", data: undefined }).kind).toBe("json");
  });

  it("classifies every sample evidence item into a specialised view", () => {
    const { report } = buildCatalogScenario({ id: "x", start: Date.parse("2026-10-20T14:05:00Z"), now: Date.parse("2026-10-20T14:35:00Z") });
    for (const e of report.evidence) expect(classifyEvidence(e).kind, e.id).not.toBe("json");
  });
});

describe("helpers", () => {
  it("toMs handles seconds, ms, strings and Go zero time", () => {
    expect(toMs(1_700_000_000)).toBe(1_700_000_000_000);
    expect(toMs(1_700_000_000_000)).toBe(1_700_000_000_000);
    expect(toMs("0001-01-01T00:00:00Z")).toBeUndefined();
    expect(toMs("nope")).toBeUndefined();
  });
  it("splits patterns on wildcards", () => {
    expect(patternSegments("took <*> ms")).toEqual([
      { text: "took ", wild: false },
      { text: "<*>", wild: true },
      { text: " ms", wild: false },
    ]);
  });
});
