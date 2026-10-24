package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/sources/fixture"
)

func ev(ids ...string) []core.Evidence {
	out := make([]core.Evidence, len(ids))
	for i, id := range ids {
		out[i] = core.Evidence{ID: id}
	}
	return out
}

func report(hs ...core.Hypothesis) *core.Report {
	return &core.Report{
		Status:     core.StatusCompleted,
		Hypotheses: hs,
		Evidence:   ev("E1", "E2", "E3"),
		Usage:      core.Usage{Steps: 4, PromptTokens: 1000, CompletionTokens: 200},
		Model:      "fake-model",
	}
}

func TestScoreReport(t *testing.T) {
	withChange := fixture.Truth{Service: "catalog", ChangeID: "deploy-catalog-1.8.2", Category: "bad-deploy", Keywords: []string{"N+1", "product_images"}}
	noChange := fixture.Truth{Service: "redis", Category: "dependency-failure", Keywords: []string{"connection refused"}}
	tr, fa := true, false

	tests := []struct {
		name  string
		truth fixture.Truth
		rep   *core.Report
		want  Score
	}{
		{
			name:  "nil report",
			truth: withChange,
			rep:   nil,
			want:  Score{ChangeMatch: &fa},
		},
		{
			name:  "all right",
			truth: withChange,
			rep: report(core.Hypothesis{Title: "Catalog N+1 query", Explanation: "loads product images per product",
				Service: "Catalog", Category: "bad-deploy", ChangeID: "deploy-catalog-1.8.2", Confidence: 0.8, EvidenceIDs: []string{"E1", "E2"}}),
			want: Score{HasReport: true, Hypotheses: 1, TopService: "Catalog", TopChangeID: "deploy-catalog-1.8.2", TopCategory: "bad-deploy",
				TopConfidence: 0.8, Top1Service: true, Top3Service: true, ChangeMatch: &tr, CategoryMatch: true, KeywordHit: true,
				CitationsValid: true, Correct: true, Tokens: 1200, Steps: 4},
		},
		{
			name:  "right service in top3, wrong change, bad citation",
			truth: withChange,
			rep: report(
				core.Hypothesis{Title: "Cart deploy", Explanation: "redis client", Service: "cart", Category: "bad-deploy", ChangeID: "deploy-cart-2.3.1", Confidence: 0.6, EvidenceIDs: []string{"E1"}},
				core.Hypothesis{Title: "Catalog slow", Service: "catalog", Confidence: 0.3, EvidenceIDs: []string{"E9"}},
			),
			want: Score{HasReport: true, Hypotheses: 2, TopService: "cart", TopChangeID: "deploy-cart-2.3.1", TopCategory: "bad-deploy",
				TopConfidence: 0.6, Top3Service: true, ChangeMatch: &fa, CategoryMatch: true, Tokens: 1200, Steps: 4},
		},
		{
			name:  "no change in truth, none blamed",
			truth: noChange,
			rep: report(core.Hypothesis{Title: "Redis down", Explanation: "dial tcp: Connection Refused", Service: "redis",
				Category: "dependency-failure", Confidence: 0.9, EvidenceIDs: []string{"E3"}}),
			want: Score{HasReport: true, Hypotheses: 1, TopService: "redis", TopCategory: "dependency-failure", TopConfidence: 0.9,
				Top1Service: true, Top3Service: true, SpuriousChange: &fa, CategoryMatch: true, KeywordHit: true, CitationsValid: true,
				Correct: true, Tokens: 1200, Steps: 4},
		},
		{
			name:  "no change in truth, change blamed is penalised",
			truth: noChange,
			rep: report(core.Hypothesis{Title: "Redis down", Service: "redis", ChangeID: "deploy-frontend-4.12.0",
				Confidence: 0.7, EvidenceIDs: []string{"E3"}}),
			want: Score{HasReport: true, Hypotheses: 1, TopService: "redis", TopChangeID: "deploy-frontend-4.12.0", TopConfidence: 0.7,
				Top1Service: true, Top3Service: true, SpuriousChange: &tr, CitationsValid: true, Tokens: 1200, Steps: 4},
		},
		{
			name:  "hypothesis without citations",
			truth: noChange,
			rep:   report(core.Hypothesis{Title: "x", Service: "cart", Confidence: 0.2}),
			want: Score{HasReport: true, Hypotheses: 1, TopService: "cart", TopConfidence: 0.2, SpuriousChange: &fa,
				Tokens: 1200, Steps: 4},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreReport(tc.truth, tc.rep)
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(tc.want)
			if string(gb) != string(wb) {
				t.Errorf("got  %s\nwant %s", gb, wb)
			}
		})
	}
}

func TestCitationsTimeline(t *testing.T) {
	rep := report(core.Hypothesis{Service: "a", EvidenceIDs: []string{"E1"}})
	rep.Timeline = []core.TimelineEntry{{Text: "x", EvidenceIDs: []string{"E7"}}}
	if ScoreReport(fixture.Truth{Service: "a"}, rep).CitationsValid {
		t.Error("timeline citing unknown evidence should be invalid")
	}
}

type fakeInv struct {
	fn    func(inc core.Incident) (*core.Report, error)
	calls atomic.Int32
}

func (f *fakeInv) Run(ctx context.Context, inc core.Incident, _ core.EventSink) (*core.Report, error) {
	f.calls.Add(1)
	return f.fn(inc)
}

func fakeScenario(name string, truth fixture.Truth) *fixture.Scenario {
	start := time.Date(2026, 10, 20, 14, 5, 0, 0, time.UTC)
	return &fixture.Scenario{Name: name, Truth: truth, Incident: core.NewIncident("t", "frontend", start, 2*time.Hour, 30*time.Minute)}
}

func TestRunAggregates(t *testing.T) {
	good := &fakeInv{fn: func(inc core.Incident) (*core.Report, error) {
		if inc.Source != "eval" || !strings.HasPrefix(inc.ID, "eval-good-") {
			return nil, errors.New("bad incident " + inc.ID)
		}
		return report(core.Hypothesis{Title: "N+1 query", Service: "catalog", Category: "bad-deploy",
			ChangeID: "deploy-1", Confidence: 0.9, EvidenceIDs: []string{"E1"}}), nil
	}}
	bad := &fakeInv{fn: func(core.Incident) (*core.Report, error) {
		r := report(core.Hypothesis{Title: "deploy", Service: "frontend", ChangeID: "deploy-x", Confidence: 0.5, EvidenceIDs: []string{"E1"}})
		return r, nil
	}}
	broken := &fakeInv{fn: func(core.Incident) (*core.Report, error) { return nil, errors.New("llm down") }}

	build := func(dir string) (Investigator, *fixture.Scenario, error) {
		switch filepath.Base(dir) {
		case "good":
			return good, fakeScenario("good", fixture.Truth{Service: "catalog", ChangeID: "deploy-1", Category: "bad-deploy", Keywords: []string{"n+1"}}), nil
		case "bad":
			return bad, fakeScenario("bad", fixture.Truth{Service: "redis", Category: "dependency-failure"}), nil
		case "broken":
			return broken, fakeScenario("broken", fixture.Truth{Service: "cart"}), nil
		default:
			return nil, nil, errors.New("no such scenario")
		}
	}
	var progress atomic.Int32
	res, err := Run(context.Background(), Options{
		Scenarios:   []string{"x/good", "x/bad", "x/broken", "x/missing"},
		Runs:        3,
		Concurrency: 4,
		Build:       build,
		Progress:    func(done, total int, r RunResult) { progress.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if progress.Load() != 12 || good.calls.Load() != 3 || bad.calls.Load() != 3 || broken.calls.Load() != 3 {
		t.Fatalf("progress=%d calls good=%d bad=%d broken=%d", progress.Load(), good.calls.Load(), bad.calls.Load(), broken.calls.Load())
	}
	if res.Model != "fake-model" || len(res.Scenarios) != 4 {
		t.Fatalf("model=%q scenarios=%d", res.Model, len(res.Scenarios))
	}
	for i, sc := range res.Scenarios {
		for j, r := range sc.Runs {
			if r.Run != j+1 || r.Scenario != sc.Name {
				t.Errorf("scenario %d run %d mislabelled: %+v", i, j, r)
			}
		}
	}
	if s := res.Scenarios[0].Summary; s.Top1Service.Hits != 3 || s.ChangeMatch.Hits != 3 || s.KeywordHit.Hits != 3 || s.Correct.Hits != 3 {
		t.Errorf("good summary: %+v", s)
	}
	if s := res.Scenarios[3]; s.Runs[0].Status != StatusBuildError || s.Name != "missing" {
		t.Errorf("missing scenario: %+v", s)
	}
	if s := res.Scenarios[2].Runs[0]; s.Status != StatusError || s.Error != "llm down" {
		t.Errorf("broken run: %+v", s)
	}

	o := res.Overall
	check := func(name string, r Rate, hits, of int) {
		t.Helper()
		if r.Hits != hits || r.Of != of {
			t.Errorf("%s = %d/%d, want %d/%d", name, r.Hits, r.Of, hits, of)
		}
	}
	check("completed", o.Completed, 6, 12)
	check("top1", o.Top1Service, 3, 12)
	check("change", o.ChangeMatch, 3, 3)
	check("spurious", o.SpuriousChange, 3, 3)
	check("citations", o.CitationsValid, 6, 6)
	check("correct", o.Correct, 3, 12)
	if o.Calibration.Correct != 3 || o.Calibration.Wrong != 3 ||
		o.Calibration.MeanConfidenceCorrect != 0.9 || o.Calibration.MeanConfidenceWrong != 0.5 {
		t.Errorf("calibration: %+v", o.Calibration)
	}
	if o.MeanTokens != 1200 || o.MeanSteps != 4 {
		t.Errorf("means: tokens=%v steps=%v", o.MeanTokens, o.MeanSteps)
	}

	md := res.Markdown()
	for _, want := range []string{"| good | catalog / deploy-1 | 3/3 | 3/3 |", "**Overall**", "3/12 (25%)", "Calibration: mean top confidence 0.90 when correct (n=3), 0.50 when wrong (n=3)", "broken run 1: error llm down"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	js, err := res.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Results
	if err := json.Unmarshal(js, &back); err != nil || back.Overall.Top1Service.Hits != 3 {
		t.Errorf("json roundtrip: %v %+v", err, back.Overall.Top1Service)
	}
}

func TestRunCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	inv := &fakeInv{fn: func(core.Incident) (*core.Report, error) {
		cancel()
		return report(core.Hypothesis{Service: "a", Confidence: 0.5, EvidenceIDs: []string{"E1"}}), nil
	}}
	res, err := Run(ctx, Options{
		Scenarios:   []string{"a"},
		Runs:        5,
		Concurrency: 1,
		Build: func(string) (Investigator, *fixture.Scenario, error) {
			return inv, fakeScenario("a", fixture.Truth{Service: "a"}), nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if inv.calls.Load() != 1 || res.Scenarios[0].Runs[4].Status != StatusCanceled {
		t.Errorf("calls=%d runs=%+v", inv.calls.Load(), res.Scenarios[0].Runs)
	}
}

func TestRunValidation(t *testing.T) {
	if _, err := Run(context.Background(), Options{Scenarios: []string{"a"}}); err == nil {
		t.Error("want error without Build")
	}
	if _, err := Run(context.Background(), Options{Build: func(string) (Investigator, *fixture.Scenario, error) { return nil, nil, nil }}); err == nil {
		t.Error("want error without scenarios")
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"b", "a", "c"} {
		os.MkdirAll(filepath.Join(root, n), 0o755)
		if n != "c" {
			os.WriteFile(filepath.Join(root, n, "scenario.yaml"), []byte("name: "+n+"\n"), 0o644)
		}
	}
	os.WriteFile(filepath.Join(root, "README"), nil, 0o644)

	got, err := Discover(root, nil)
	if err != nil || len(got) != 2 || filepath.Base(got[0]) != "a" || filepath.Base(got[1]) != "b" {
		t.Fatalf("Discover = %v, %v", got, err)
	}
	got, err = Discover(root, []string{"b", " "})
	if err != nil || len(got) != 1 || filepath.Base(got[0]) != "b" {
		t.Fatalf("Discover only = %v, %v", got, err)
	}
	if _, err := Discover(root, []string{"zzz"}); err == nil || !strings.Contains(err.Error(), "zzz") {
		t.Fatalf("want unknown error, got %v", err)
	}
	if _, err := Discover(t.TempDir(), nil); err == nil {
		t.Fatal("want error for empty root")
	}
}

// TestRunOverFixtures drives the real fixture loader over the committed
// scenarios with a fake investigator that always blames the truth. It skips
// while the loader is a stub.
func TestRunOverFixtures(t *testing.T) {
	dirs, err := Discover(filepath.Join("..", "..", "eval", "scenarios"), nil)
	if err != nil {
		t.Skipf("no scenarios: %v", err)
	}
	if _, err := fixture.Load(dirs[0]); err != nil {
		if strings.Contains(err.Error(), "not implemented") {
			t.Skip("fixture.Load not implemented yet")
		}
		t.Fatalf("fixture.Load(%s): %v", dirs[0], err)
	}
	build := func(dir string) (Investigator, *fixture.Scenario, error) {
		sc, err := fixture.Load(dir)
		if err != nil {
			return nil, nil, err
		}
		inv := &fakeInv{fn: func(inc core.Incident) (*core.Report, error) {
			kw := ""
			if len(sc.Truth.Keywords) > 0 {
				kw = sc.Truth.Keywords[0]
			}
			return report(core.Hypothesis{Title: kw, Service: sc.Truth.Service, Category: sc.Truth.Category,
				ChangeID: sc.Truth.ChangeID, Confidence: 0.8, EvidenceIDs: []string{"E1"}}), nil
		}}
		return inv, sc, nil
	}
	res, err := Run(context.Background(), Options{Scenarios: dirs, Build: build, Concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	o := res.Overall
	if o.Runs != len(dirs) || o.Correct.Hits != len(dirs) || o.KeywordHit.Hits != len(dirs) || o.CategoryMatch.Hits != len(dirs) {
		t.Errorf("oracle investigator not perfect: %+v", o)
	}
	for _, sc := range res.Scenarios {
		if sc.Truth.Service == "" {
			t.Errorf("%s: truth not loaded", sc.Name)
		}
	}
}
