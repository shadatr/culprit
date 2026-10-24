package eval

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// JSON returns the results as indented JSON.
func (r *Results) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	return append(b, '\n'), nil
}

func frac(r Rate) string {
	if r.Of == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d", r.Hits, r.Of)
}

func pct(r Rate) string {
	if r.Of == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", r.Hits, r.Of, 100*r.Rate)
}

func secs(s float64) string {
	return (time.Duration(s * float64(time.Second))).Round(100 * time.Millisecond).String()
}

func meanConf(s Summary) string {
	n := s.Calibration.Correct + s.Calibration.Wrong
	if n == 0 {
		return "n/a"
	}
	c := (s.Calibration.MeanConfidenceCorrect*float64(s.Calibration.Correct) +
		s.Calibration.MeanConfidenceWrong*float64(s.Calibration.Wrong)) / float64(n)
	return fmt.Sprintf("%.2f", c)
}

func cell(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// Markdown renders a results table.
func (r *Results) Markdown() string {
	var b strings.Builder
	b.WriteString("## Culprit eval results\n\n")
	model := r.Model
	if model == "" {
		model = "unknown"
	}
	fmt.Fprintf(&b, "Model: `%s` · runs per scenario: %d · scenarios: %d · %s\n\n",
		model, r.RunsPerSample, len(r.Scenarios), r.StartedAt.Format(time.RFC3339))
	b.WriteString("| Scenario | Truth | Completed | Top-1 | Top-3 | Change | Spurious change | Category | Keyword | Citations | Mean conf | Mean tokens | Mean time |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	row := func(name, truth string, s Summary, f func(Rate) string) {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %.0f | %s |\n",
			cell(name), cell(truth), f(s.Completed), f(s.Top1Service), f(s.Top3Service), f(s.ChangeMatch),
			f(s.SpuriousChange), f(s.CategoryMatch), f(s.KeywordHit), f(s.CitationsValid),
			meanConf(s), s.MeanTokens, secs(s.MeanDurationSeconds))
	}
	for _, sc := range r.Scenarios {
		truth := sc.Truth.Service
		if sc.Truth.ChangeID != "" {
			truth += " / " + sc.Truth.ChangeID
		}
		if truth == "" {
			truth = "?"
		}
		row(sc.Name, truth, sc.Summary, frac)
	}
	row("**Overall**", "", r.Overall, pct)

	o := r.Overall
	b.WriteString("\n")
	fmt.Fprintf(&b, "Fully correct (top-1 service and right change verdict): %s.\n\n", pct(o.Correct))
	if o.Calibration.Correct+o.Calibration.Wrong > 0 {
		fmt.Fprintf(&b, "Calibration: mean top confidence %.2f when correct (n=%d), %.2f when wrong (n=%d).\n\n",
			o.Calibration.MeanConfidenceCorrect, o.Calibration.Correct, o.Calibration.MeanConfidenceWrong, o.Calibration.Wrong)
	}
	var failures []string
	for _, sc := range r.Scenarios {
		for _, run := range sc.Runs {
			if run.Status != "completed" {
				msg := strings.Join(strings.Fields(run.Error), " ")
				if len(msg) > 160 {
					msg = msg[:160] + "..."
				}
				failures = append(failures, fmt.Sprintf("- %s run %d: %s %s", sc.Name, run.Run, run.Status, msg))
			}
		}
	}
	if len(failures) > 0 {
		b.WriteString("Runs that did not complete:\n\n")
		b.WriteString(strings.Join(failures, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString("Change: top hypothesis names the true change (scenarios with a causal change). " +
		"Spurious change: top hypothesis blames a change although none caused the incident (lower is better). " +
		"Citations: every hypothesis cites evidence that exists in the report.\n")
	return b.String()
}
