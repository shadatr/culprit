package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/shadatr/culprit/internal/core"
)

// submission is the submit_report argument payload.
type submission struct {
	Summary    string               `json:"summary"`
	Timeline   []core.TimelineEntry `json:"timeline"`
	Hypotheses []core.Hypothesis    `json:"hypotheses"`
}

// parseSubmission decodes submit_report arguments.
func parseSubmission(args json.RawMessage) (submission, error) {
	var s submission
	if err := json.Unmarshal(args, &s); err != nil {
		return s, fmt.Errorf("arguments are not a valid report object: %w", err)
	}
	return s, nil
}

// validate checks a submission against PLAN.md §3 step 4 and returns every
// problem found. has reports whether an evidence ID exists.
func validate(s submission, has func(string) bool) []string {
	var errs []string
	if strings.TrimSpace(s.Summary) == "" {
		errs = append(errs, "summary is empty")
	}
	if len(s.Hypotheses) == 0 {
		errs = append(errs, "at least one hypothesis is required")
	}
	for i, h := range s.Hypotheses {
		n := i + 1
		if strings.TrimSpace(h.Title) == "" {
			errs = append(errs, fmt.Sprintf("hypothesis %d: title is empty", n))
		}
		if math.IsNaN(h.Confidence) || h.Confidence < 0 || h.Confidence > 1 {
			errs = append(errs, fmt.Sprintf("hypothesis %d: confidence %v is outside [0,1]", n, h.Confidence))
		}
		if i > 0 && h.Confidence > s.Hypotheses[i-1].Confidence {
			errs = append(errs, fmt.Sprintf("hypothesis %d: confidence %v is higher than hypothesis %d (%v); rank hypotheses by confidence, highest first", n, h.Confidence, i, s.Hypotheses[i-1].Confidence))
		}
		if len(h.EvidenceIDs) == 0 {
			errs = append(errs, fmt.Sprintf("hypothesis %d: cites no evidence_ids", n))
		}
		for _, id := range h.EvidenceIDs {
			if !has(id) {
				errs = append(errs, fmt.Sprintf("hypothesis %d: evidence %q does not exist", n, id))
			}
		}
	}
	for i, e := range s.Timeline {
		n := i + 1
		if e.T.IsZero() {
			errs = append(errs, fmt.Sprintf("timeline entry %d: missing time", n))
		}
		if i > 0 && e.T.Before(s.Timeline[i-1].T) {
			errs = append(errs, fmt.Sprintf("timeline entry %d: %s is before entry %d; sort the timeline by time", n, e.T.Format("15:04:05Z07:00"), i))
		}
		for _, id := range e.EvidenceIDs {
			if !has(id) {
				errs = append(errs, fmt.Sprintf("timeline entry %d: evidence %q does not exist", n, id))
			}
		}
	}
	return errs
}

// sanitise drops citations of unknown evidence, for reports kept despite
// failing validation.
func sanitise(s submission, has func(string) bool) submission {
	keep := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if has(id) {
				out = append(out, id)
			}
		}
		return out
	}
	hs := make([]core.Hypothesis, len(s.Hypotheses))
	for i, h := range s.Hypotheses {
		h.EvidenceIDs = keep(h.EvidenceIDs)
		h.Confidence = math.Max(0, math.Min(1, h.Confidence))
		if math.IsNaN(h.Confidence) {
			h.Confidence = 0
		}
		hs[i] = h
	}
	tl := make([]core.TimelineEntry, len(s.Timeline))
	for i, e := range s.Timeline {
		e.EvidenceIDs = keep(e.EvidenceIDs)
		tl[i] = e
	}
	s.Hypotheses, s.Timeline = hs, tl
	return s
}
