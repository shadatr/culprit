package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
)

const systemPrompt = `You are Culprit, an expert site reliability engineer investigating a production incident. Your job is to find the root cause, not just describe symptoms.

Rules:
- You are strictly read-only. Your tools only query metrics, logs, changes and topology. Never suggest you changed anything.
- Evidence or it didn't happen: every claim must cite evidence IDs (E1, E2, ...) that appear in the triage summary or in tool results. Never invent IDs.
- Prefer the root cause over symptoms. A fault in a dependency usually shows up as latency or errors in its callers; follow the topology downstream from the symptomatic service and look for the earliest onset.
- Consider red herrings: a change or anomaly that is close in time is not necessarily causal. Check that the service, timing and failure mode line up, and say why you rejected plausible alternatives.
- Metric tools take Prometheus selectors such as name{service="x"} — not PromQL functions or operators. Times are RFC3339.
- Be efficient: each tool call costs budget. Confirm or reject the strongest leads from triage first.
- When you are confident, or when told the budget is nearly exhausted, call submit_report. Rank hypotheses by confidence (0..1, highest first), give each a causal explanation, the root-cause service, category, change_id if a change is responsible, cited evidence_ids and concrete next steps. Sort the timeline by time.`

const nudgeText = `Please continue by calling a tool. If you have enough evidence, call submit_report now with your hypotheses and cited evidence IDs. Do not answer in plain text.`

const budgetText = `Budget nearly exhausted (%s). Call submit_report now with your best-supported hypotheses, citing only evidence IDs that exist.`

func incidentPrompt(inc core.Incident, triage string) string {
	var b strings.Builder
	b.WriteString("Incident to investigate:\n")
	fmt.Fprintf(&b, "- Title: %s\n", inc.Title)
	if inc.ID != "" {
		fmt.Fprintf(&b, "- ID: %s\n", inc.ID)
	}
	if inc.Service != "" {
		fmt.Fprintf(&b, "- Symptomatic service: %s\n", inc.Service)
	}
	fmt.Fprintf(&b, "- Started at: %s\n", inc.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Analysis window: %s .. %s (baseline is before the start)\n",
		inc.Window.Start.UTC().Format(time.RFC3339), inc.Window.End.UTC().Format(time.RFC3339))
	if len(inc.Labels) > 0 {
		keys := make([]string, 0, len(inc.Labels))
		for k := range inc.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + inc.Labels[k]
		}
		fmt.Fprintf(&b, "- Labels: %s\n", strings.Join(parts, ", "))
	}
	b.WriteString("\nDeterministic triage (already recorded as evidence):\n")
	b.WriteString(triage)
	b.WriteString("\n\nInvestigate with the tools, then call submit_report.")
	return b.String()
}
