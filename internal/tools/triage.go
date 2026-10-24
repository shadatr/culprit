package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/analysis/anomaly"
	"github.com/shadatr/culprit/internal/analysis/correlate"
	"github.com/shadatr/culprit/internal/analysis/logpattern"
	"github.com/shadatr/culprit/internal/core"
)

// MaxTriageChars caps the triage summary given to the model.
const MaxTriageChars = 6000

// Triage limits.
const (
	maxAutoMetrics      = 60 // scan all metrics only when the source lists at most this many
	maxTriageServices   = 30
	maxTriageAnomalies  = 8
	maxShiftsPerService = 3
	maxTriageChanges    = 6
	maxSuspects         = 6
)

// TriageResult is the outcome of deterministic triage.
type TriageResult struct {
	Summary     string                   // compact text for the model, lines tagged with evidence IDs
	EvidenceIDs []string                 // evidence recorded during triage, in order
	Anomalies   []anomaly.Anomaly        // all detected anomalies, highest score first
	Shifts      []logpattern.Shift       // all log shifts across services
	Changes     []correlate.ScoredChange // ranked changes in the window
	Suspects    []correlate.ScoredService
	Notes       []string // problems such as unreachable sources
}

// Triage runs deterministic analysis over inc's window (PLAN.md §3 step 2)
// and records findings in d.Ledger. selectors are the metrics to scan; when
// empty, every metric is scanned if the source lists at most 60. Source
// failures are noted, not fatal; only a done ctx or a nil ledger returns an
// error.
func Triage(ctx context.Context, d Deps, inc core.Incident, selectors []string) (*TriageResult, error) {
	if d.Ledger == nil {
		return nil, errors.New("tools: triage needs a ledger")
	}
	tr := &TriageResult{}
	tb := &Toolbox{d: d, inc: inc, tr: tr}
	start := inc.StartedAt

	note := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		slog.Warn("triage", "note", msg)
		tr.Notes = append(tr.Notes, msg)
	}
	add := func(kind, summary string, data any) string {
		id := tb.record(kind, "triage", nil, summary, data)
		tr.EvidenceIDs = append(tr.EvidenceIDs, id)
		return id
	}

	// Metrics.
	var metricLines []string
	if d.Metrics == nil {
		metricLines = append(metricLines, "Metrics: no metrics source configured.")
	} else {
		sels := selectors
		if len(sels) == 0 {
			names, err := d.Metrics.ListMetrics(ctx, "")
			switch {
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case err != nil:
				note("metrics: list metrics: %v", err)
			case len(names) > maxAutoMetrics:
				note("metrics: %d metrics available; configure triage metrics to scan them in triage", len(names))
			default:
				sels = names
			}
		}
		var all []core.Series
		for _, sel := range sels {
			ss, err := d.Metrics.Query(ctx, sel, inc.Window, queryStep(inc.Window))
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				note("metrics: query %s: %v", sel, err)
				continue
			}
			all = append(all, ss...)
		}
		tr.Anomalies = anomaly.Detect(all, anomaly.Options{IncidentStart: start})
		for i := range tr.Anomalies {
			if tr.Anomalies[i].Service == "" {
				tr.Anomalies[i].Service = tr.Anomalies[i].Labels["service"]
			}
		}
		if len(tr.Anomalies) == 0 {
			metricLines = append(metricLines, fmt.Sprintf("Metrics: scanned %d series from %d selectors; no anomalies detected.", len(all), len(sels)))
		} else {
			metricLines = append(metricLines, fmt.Sprintf("Metric anomalies (%d of %d series; top %d, earliest onset is a strong lead):", len(tr.Anomalies), len(all), min(len(tr.Anomalies), maxTriageAnomalies)))
		}
		for _, a := range tr.Anomalies[:min(len(tr.Anomalies), maxTriageAnomalies)] {
			line := fmt.Sprintf("%s%s %s: baseline %s -> peak %s, score %s, onset %s",
				a.Metric, fmtLabels(a.Labels), a.Direction, num(a.Baseline), num(a.Peak), num(a.Score), ts(a.Onset, start))
			id := add("anomaly", line, a)
			metricLines = append(metricLines, fmt.Sprintf("[%s] %s", id, line))
		}
	}

	// Logs.
	var logLines []string
	if d.Logs == nil {
		logLines = append(logLines, "Logs: no logs source configured.")
	} else {
		services, err := d.Logs.Services(ctx, inc.Window)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			note("logs: list services: %v", err)
			// Fall back to services we know about from the topology.
			for k, vs := range d.Topology {
				services = append(services, k)
				services = append(services, vs...)
			}
			if inc.Service != "" {
				services = append(services, inc.Service)
			}
		}
		services = prioritise(services, inc.Service, d.Topology)
		if len(services) > maxTriageServices {
			note("logs: %d services; triage scanned the first %d", len(services), maxTriageServices)
			services = services[:maxTriageServices]
		}
		type svcShifts struct {
			svc    string
			shifts []logpattern.Shift
		}
		var found []svcShifts
		for _, svc := range services {
			lines, err := d.Logs.Query(ctx, core.LogQuery{Service: svc, Window: inc.Window, Limit: maxLogLines})
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				note("logs: query %s: %v", svc, err)
				continue
			}
			sh := logpattern.Diff(lines, start)
			for i := range sh {
				if sh[i].Template.Service == "" {
					sh[i].Template.Service = svc
				}
			}
			tr.Shifts = append(tr.Shifts, sh...)
			if len(sh) > 0 {
				found = append(found, svcShifts{svc, sh})
			}
		}
		slices.SortStableFunc(found, func(a, b svcShifts) int {
			switch {
			case a.shifts[0].Score > b.shifts[0].Score:
				return -1
			case a.shifts[0].Score < b.shifts[0].Score:
				return 1
			}
			return 0
		})
		if len(found) == 0 {
			logLines = append(logLines, fmt.Sprintf("Logs: scanned %d services; no new or spiking log templates.", len(services)))
		} else {
			logLines = append(logLines, fmt.Sprintf("Log shifts (%d of %d services changed):", len(found), len(services)))
		}
		for _, f := range found {
			top := f.shifts[:min(len(f.shifts), maxShiftsPerService)]
			parts := make([]string, len(top))
			for i, s := range top {
				parts[i] = shiftLine(s, start)
			}
			summary := f.svc + ": " + strings.Join(parts, " | ")
			id := add("log_patterns", summary, map[string]any{"service": f.svc, "shifts": top})
			logLines = append(logLines, fmt.Sprintf("[%s] %s", id, summary))
		}
	}

	// Changes.
	var changeLines []string
	if d.Changes == nil {
		changeLines = append(changeLines, "Changes: no changes source configured.")
	} else {
		cs, err := d.Changes.Changes(ctx, inc.Window)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			note("changes: %v", err)
		}
		tr.Changes = rankChanges(cs, tr.Anomalies, tr.Shifts, d.Topology, start)
		if len(tr.Changes) == 0 {
			changeLines = append(changeLines, "Changes: none in the window.")
		} else {
			changeLines = append(changeLines, fmt.Sprintf("Changes (%d in window; top %d by correlation):", len(tr.Changes), min(len(tr.Changes), maxTriageChanges)))
		}
		for _, sc := range tr.Changes[:min(len(tr.Changes), maxTriageChanges)] {
			line := changeLine(sc, start)
			sc.Change = truncateDiff(sc.Change, 2000)
			id := add("change", line, sc)
			changeLines = append(changeLines, fmt.Sprintf("[%s] %s", id, line))
		}
	}

	// Suspect services.
	var suspectLines []string
	tr.Suspects = correlate.SuspectServices(tr.Anomalies, tr.Shifts, d.Topology, inc.Service)
	if len(tr.Suspects) > 0 {
		top := tr.Suspects[:min(len(tr.Suspects), maxSuspects)]
		parts := make([]string, len(top))
		for i, s := range top {
			p := fmt.Sprintf("%d. %s score %.2f", i+1, s.Service, s.Score)
			if !s.EarliestOnset.IsZero() {
				p += " onset " + ts(s.EarliestOnset, start)
			}
			if len(s.Reasons) > 0 {
				p += " (" + clip(strings.Join(s.Reasons, "; "), 160) + ")"
			}
			parts[i] = p
		}
		id := add("suspects", "Suspect services: "+strings.Join(parts, "; "), top)
		suspectLines = append(suspectLines, "Suspect services (localised from onsets, signal strength and topology):")
		for _, p := range parts {
			suspectLines = append(suspectLines, fmt.Sprintf("[%s] %s", id, p))
		}
	}

	// Assemble: most decision-relevant sections first, so truncation drops
	// the least useful lines.
	var b strings.Builder
	fmt.Fprintf(&b, "Triage for %q (symptom service %s), incident start %s, window %s..%s.\n",
		inc.Title, orDash(inc.Service), start.UTC().Format(time.RFC3339), inc.Window.Start.UTC().Format(time.RFC3339), inc.Window.End.UTC().Format(time.RFC3339))
	for _, sec := range [][]string{suspectLines, metricLines, logLines, changeLines} {
		if len(sec) == 0 {
			continue
		}
		b.WriteByte('\n')
		for _, l := range sec {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	if len(d.Topology) > 0 {
		b.WriteString("\nTopology (caller -> callees): ")
		b.WriteString(topologyOneLine(d.Topology))
		b.WriteByte('\n')
	}
	if len(tr.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range tr.Notes[:min(len(tr.Notes), 8)] {
			b.WriteString("- " + clip(n, 200) + "\n")
		}
	}
	tr.Summary = capText(b.String(), MaxTriageChars)
	return tr, nil
}

func orDash(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// prioritise orders services: symptom first, then its dependencies (transitively, BFS),
// then the rest alphabetically.
func prioritise(services []string, symptom string, topo core.Topology) []string {
	set := map[string]bool{}
	for _, s := range services {
		set[s] = true
	}
	var out []string
	seen := map[string]bool{}
	queue := []string{symptom}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		if set[s] {
			out = append(out, s)
		}
		queue = append(queue, topo[s]...)
	}
	rest := make([]string, 0, len(services))
	for _, s := range services {
		if !seen[s] {
			rest = append(rest, s)
			seen[s] = true
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func topologyOneLine(topo core.Topology) string {
	keys := make([]string, 0, len(topo))
	for k := range topo {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" -> "+strings.Join(topo[k], ","))
	}
	return strings.Join(parts, "; ")
}
