import { describe, expect, it } from "vitest";
import { parseReport } from "../api";
import { cell, renderReportMarkdown } from "../markdown";
import { buildCatalogScenario } from "../mock";

const report = parseReport({
  incident_id: "inv1",
  status: "completed",
  summary: "Catalog 1.8.2 added an N+1 query.",
  timeline: [{ t: "2026-10-20T13:57:00Z", text: "catalog | 1.8.2 deployed", evidence_ids: ["E4"] }],
  hypotheses: [
    {
      title: "N+1 query in catalog",
      explanation: "One query per product.",
      service: "catalog",
      category: "bad-deploy",
      change_id: "deploy-catalog-1.8.2",
      confidence: 0.86,
      evidence_ids: ["E4", "E7"],
      next_steps: ["Roll back catalog"],
    },
    { title: "Postgres", explanation: "", confidence: 0.1, evidence_ids: [] },
  ],
  evidence: [
    { id: "E4", kind: "changes", tool: "triage", summary: "4 changes" },
    { id: "E7", kind: "metric", tool: "get_metric", summary: "db p99\nup" },
  ],
  usage: { steps: 8, tool_calls: 8, prompt_tokens: 38420, completion_tokens: 2915 },
  model: "gpt-4.1",
  started_at: "2026-10-20T14:06:00Z",
  finished_at: "2026-10-20T14:06:42Z",
});

describe("renderReportMarkdown", () => {
  const md = renderReportMarkdown(report, {
    id: "inv1",
    title: "High p99 latency on frontend",
    service: "frontend",
    started_at: "2026-10-20T14:05:00Z",
    window: { start: "", end: "" },
  });

  it("has title, meta and summary", () => {
    expect(md.startsWith("# High p99 latency on frontend\n")).toBe(true);
    expect(md).toContain("**Service:** frontend");
    expect(md).toContain("**Started:** 2026-10-20 14:05:00 UTC");
    expect(md).toContain("## Summary\n\nCatalog 1.8.2 added an N+1 query.");
  });

  it("ranks hypotheses with confidence and cites evidence", () => {
    expect(md).toContain("### 1. N+1 query in catalog (86%)");
    expect(md).toContain("- **Suspected change:** `deploy-catalog-1.8.2`");
    expect(md).toContain("- **Evidence:** `E4`, `E7`");
    expect(md).toContain("- [ ] Roll back catalog");
    expect(md).toContain("### 2. Postgres (10%)");
    expect(md).toContain("- **Evidence:** none");
    expect(md.indexOf("### 1.")).toBeLessThan(md.indexOf("### 2."));
  });

  it("renders a timeline table with escaped cells", () => {
    expect(md).toContain("| Time (UTC) | Event | Evidence |");
    expect(md).toContain("| 2026-10-20 13:57:00 UTC | catalog \\| 1.8.2 deployed | E4 |");
  });

  it("lists evidence on one line each and a usage footer", () => {
    expect(md).toContain("- **E7** `metric` via `get_metric`: db p99 up");
    expect(md).toContain("8 steps, 8 tool calls, 41k tokens (38k in / 2.9k out), took 42s");
    expect(md).toContain("model gpt-4.1");
  });

  it("works without an incident", () => {
    const out = renderReportMarkdown(parseReport({ status: "failed", summary: "" }));
    expect(out).toContain("# Incident");
    expect(out).toContain("_No hypotheses._");
    expect(out).not.toContain("## Timeline");
  });

  it("renders the bundled sample report", () => {
    const { report: r, incident } = buildCatalogScenario({ id: "x", start: Date.parse("2026-10-20T14:05:00Z"), now: Date.parse("2026-10-20T14:35:00Z") });
    const out = renderReportMarkdown(parseReport(r), incident);
    expect(out).toContain("N+1");
    for (const h of r.hypotheses) for (const id of h.evidence_ids) expect(r.evidence.some((e) => e.id === id)).toBe(true);
  });
});

describe("cell", () => {
  it("escapes pipes and newlines", () => {
    expect(cell("a|b\nc")).toBe("a\\|b c");
  });
});
