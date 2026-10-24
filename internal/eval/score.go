package eval

import (
	"strings"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/sources/fixture"
)

// Score is how one report compares to a scenario's ground truth.
type Score struct {
	HasReport     bool    `json:"has_report"`
	Hypotheses    int     `json:"hypotheses"`
	TopService    string  `json:"top_service,omitempty"`
	TopChangeID   string  `json:"top_change_id,omitempty"`
	TopCategory   string  `json:"top_category,omitempty"`
	TopConfidence float64 `json:"top_confidence"`

	Top1Service bool `json:"top1_service"`
	Top3Service bool `json:"top3_service"`
	// ChangeMatch is nil when the truth names no change.
	ChangeMatch *bool `json:"change_match"`
	// SpuriousChange is nil when the truth names a change; otherwise true
	// when the top hypothesis blames a change anyway.
	SpuriousChange *bool `json:"spurious_change"`
	CategoryMatch  bool  `json:"category_match"`
	KeywordHit     bool  `json:"keyword_hit"`
	// CitationsValid: every hypothesis cites at least one evidence ID and all
	// cited IDs exist in the report's evidence.
	CitationsValid bool `json:"citations_valid"`
	// Correct: top-1 service right and the change verdict right (matching
	// change, or no change blamed when there is none).
	Correct bool `json:"correct"`

	Tokens int `json:"tokens"`
	Steps  int `json:"steps"`
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ScoreReport scores rep against truth. rep may be nil.
func ScoreReport(truth fixture.Truth, rep *core.Report) Score {
	var s Score
	hasChange := strings.TrimSpace(truth.ChangeID) != ""
	f := false
	if hasChange {
		s.ChangeMatch = &f
	} else {
		s.SpuriousChange = &f
	}
	if rep == nil {
		return s
	}
	s.HasReport = true
	s.Tokens = rep.Usage.PromptTokens + rep.Usage.CompletionTokens
	s.Steps = rep.Usage.Steps
	s.Hypotheses = len(rep.Hypotheses)
	if len(rep.Hypotheses) == 0 {
		return s
	}
	want := norm(truth.Service)
	top := rep.Hypotheses[0]
	s.TopService, s.TopChangeID, s.TopCategory = top.Service, top.ChangeID, top.Category
	s.TopConfidence = top.Confidence
	s.Top1Service = want != "" && norm(top.Service) == want
	for i, h := range rep.Hypotheses {
		if i >= 3 {
			break
		}
		if want != "" && norm(h.Service) == want {
			s.Top3Service = true
		}
	}
	changeOK := false
	if hasChange {
		m := norm(top.ChangeID) == norm(truth.ChangeID)
		s.ChangeMatch = &m
		changeOK = m
	} else {
		sp := norm(top.ChangeID) != ""
		s.SpuriousChange = &sp
		changeOK = !sp
	}
	s.CategoryMatch = truth.Category != "" && norm(top.Category) == norm(truth.Category)
	text := strings.ToLower(top.Title + "\n" + top.Explanation)
	for _, k := range truth.Keywords {
		if k = norm(k); k != "" && strings.Contains(text, k) {
			s.KeywordHit = true
			break
		}
	}
	s.CitationsValid = citationsValid(rep)
	s.Correct = s.Top1Service && changeOK
	return s
}

func citationsValid(rep *core.Report) bool {
	ids := make(map[string]bool, len(rep.Evidence))
	for _, e := range rep.Evidence {
		ids[e.ID] = true
	}
	for _, h := range rep.Hypotheses {
		if len(h.EvidenceIDs) == 0 {
			return false
		}
		for _, id := range h.EvidenceIDs {
			if !ids[id] {
				return false
			}
		}
	}
	for _, t := range rep.Timeline {
		for _, id := range t.EvidenceIDs {
			if !ids[id] {
				return false
			}
		}
	}
	return true
}
