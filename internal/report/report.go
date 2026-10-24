// Package report renders investigation reports.
package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

// maxEvidenceSummary caps each evidence summary in the appendix.
const maxEvidenceSummary = 200

var statusLabel = map[string]string{
	core.StatusCompleted:      "completed",
	core.StatusFailed:         "FAILED",
	core.StatusInvalidReport:  "INVALID REPORT",
	core.StatusBudgetExceeded: "BUDGET EXCEEDED",
	core.StatusRunning:        "running",
	core.StatusQueued:         "queued",
}

// Markdown renders r for terminals, PRs and chat.
func Markdown(r *core.Report) string {
	if r == nil {
		return "# Culprit report\n\n_No report._\n"
	}
	var b strings.Builder
	title := "Culprit report"
	switch {
	case r.Title != "":
		title = r.Title
	case r.IncidentID != "":
		title += ": " + r.IncidentID
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	status := statusLabel[r.Status]
	if status == "" {
		status = r.Status
	}
	fmt.Fprintf(&b, "**Status:** %s\n\n", status)

	b.WriteString("## Summary\n\n")
	if s := strings.TrimSpace(r.Summary); s != "" {
		b.WriteString(s + "\n\n")
	} else {
		b.WriteString("_No summary._\n\n")
	}

	b.WriteString("## Hypotheses\n\n")
	if len(r.Hypotheses) == 0 {
		b.WriteString("_No hypotheses._\n\n")
	}
	for i, h := range r.Hypotheses {
		fmt.Fprintf(&b, "### %d. %s (%.0f%%)\n\n", i+1, oneLine(h.Title), h.Confidence*100)
		var facts []string
		if h.Service != "" {
			facts = append(facts, "**Service:** "+h.Service)
		}
		if h.Category != "" {
			facts = append(facts, "**Category:** "+h.Category)
		}
		if h.ChangeID != "" {
			facts = append(facts, "**Change:** `"+h.ChangeID+"`")
		}
		if len(h.EvidenceIDs) > 0 {
			facts = append(facts, "**Evidence:** "+strings.Join(h.EvidenceIDs, ", "))
		}
		if len(facts) > 0 {
			b.WriteString(strings.Join(facts, " · ") + "\n\n")
		}
		if e := strings.TrimSpace(h.Explanation); e != "" {
			b.WriteString(e + "\n\n")
		}
		if len(h.NextSteps) > 0 {
			b.WriteString("Next steps:\n\n")
			for _, s := range h.NextSteps {
				b.WriteString("- " + oneLine(s) + "\n")
			}
			b.WriteString("\n")
		}
	}

	if len(r.Timeline) > 0 {
		b.WriteString("## Timeline\n\n")
		for _, e := range r.Timeline {
			fmt.Fprintf(&b, "- `%s` %s", e.T.UTC().Format(time.RFC3339), oneLine(e.Text))
			if len(e.EvidenceIDs) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(e.EvidenceIDs, ", "))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(r.Evidence) > 0 {
		b.WriteString("## Evidence\n\n| ID | Kind | Summary |\n|---|---|---|\n")
		for _, e := range r.Evidence {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", e.ID, cell(e.Kind), cell(clip(oneLine(e.Summary), maxEvidenceSummary)))
		}
		b.WriteString("\n")
	}

	b.WriteString(usageLine(r) + "\n")
	return b.String()
}

func usageLine(r *core.Report) string {
	parts := []string{}
	if r.Model != "" {
		parts = append(parts, "model "+r.Model)
	}
	u := r.Usage
	parts = append(parts,
		fmt.Sprintf("%d steps", u.Steps),
		fmt.Sprintf("%d tool calls", u.ToolCalls),
		fmt.Sprintf("%s prompt + %s completion tokens", thousands(u.PromptTokens), thousands(u.CompletionTokens)),
	)
	if !r.StartedAt.IsZero() && !r.FinishedAt.IsZero() {
		parts = append(parts, r.FinishedAt.Sub(r.StartedAt).Round(time.Second).String())
	}
	return "_" + strings.Join(parts, " · ") + "_"
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
