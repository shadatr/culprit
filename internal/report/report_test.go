package report

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

var update = flag.Bool("update", false, "rewrite golden files")

func sample() *core.Report {
	t0 := time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC)
	return &core.Report{
		IncidentID: "inc-42",
		Status:     core.StatusCompleted,
		Summary:    "Catalog 1.8.2 added an N+1 query that saturated the DB pool; frontend p99 rose.",
		Timeline: []core.TimelineEntry{
			{T: t0.Add(-3 * time.Minute), Text: "catalog 1.8.2 deployed", EvidenceIDs: []string{"E3"}},
			{T: t0, Text: "frontend p99\nrises", EvidenceIDs: []string{"E1"}},
		},
		Hypotheses: []core.Hypothesis{
			{
				Title: "N+1 query in catalog 1.8.2", Explanation: "Pool saturation began two minutes after the deploy.",
				Service: "catalog", Category: "bad-deploy", ChangeID: "deploy-catalog-1.8.2", Confidence: 0.82,
				EvidenceIDs: []string{"E1", "E2", "E3"}, NextSteps: []string{"Roll back catalog to 1.8.1", "Check slow query log"},
			},
			{Title: "Cart TTL change", Explanation: "Unlikely: cart metrics are flat.", Service: "cart", Confidence: 0.05, EvidenceIDs: []string{"E4"}},
		},
		Evidence: []core.Evidence{
			{ID: "E1", Kind: "anomaly", Summary: "http_request_duration_seconds:p99{service=\"frontend\"} up 0.2 -> 1.9"},
			{ID: "E2", Kind: "log_patterns", Summary: "catalog: NEW [error] pool | exhausted\nwaiting <*>ms"},
			{ID: "E3", Kind: "change", Summary: "deploy-catalog-1.8.2 deploy catalog " + strings.Repeat("x", 300)},
			{ID: "E4", Kind: "metric", Summary: "cache_hit:ratio flat"},
		},
		Usage:      core.Usage{Steps: 5, ToolCalls: 6, PromptTokens: 23456, CompletionTokens: 1234},
		Model:      "test-model",
		StartedAt:  t0.Add(10 * time.Minute),
		FinishedAt: t0.Add(10*time.Minute + 42*time.Second),
	}
}

func TestMarkdownGolden(t *testing.T) {
	tests := []struct {
		name string
		r    *core.Report
	}{
		{"completed", sample()},
		{"budget_exceeded_empty", &core.Report{IncidentID: "inc-7", Status: core.StatusBudgetExceeded, Summary: "Investigation stopped: budget exhausted."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Markdown(tt.r)
			path := filepath.Join("testdata", tt.name+".golden.md")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -update to create)", err)
			}
			if got != string(want) {
				t.Errorf("Markdown mismatch; got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestMarkdownContent(t *testing.T) {
	md := Markdown(sample())
	for _, want := range []string{
		"# Culprit report: inc-42",
		"**Status:** completed",
		"### 1. N+1 query in catalog 1.8.2 (82%)",
		"**Change:** `deploy-catalog-1.8.2`",
		"**Evidence:** E1, E2, E3",
		"- Roll back catalog to 1.8.1",
		"- `2026-10-20T14:02:00Z` catalog 1.8.2 deployed (E3)",
		`| E2 | log_patterns | catalog: NEW [error] pool \| exhausted waiting <*>ms |`,
		"23,456 prompt + 1,234 completion tokens",
		"42s",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if strings.Index(md, "### 1.") > strings.Index(md, "### 2.") {
		t.Error("hypotheses out of order")
	}
	if Markdown(nil) == "" {
		t.Error("nil report renders empty")
	}
}

func TestThousands(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4200: "-4,200"} {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d) = %s want %s", in, got, want)
		}
	}
}
