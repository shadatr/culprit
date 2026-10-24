// Package eval runs Culprit against fixture scenarios with known root causes
// and scores the reports.
package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/sources/fixture"
)

// Investigator is what eval runs; *agent.Investigator satisfies it.
type Investigator interface {
	Run(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error)
}

// Options configures Run.
type Options struct {
	Scenarios   []string // scenario directories
	Runs        int      // runs per scenario, default 1
	Concurrency int      // parallel runs, default 2
	// Build loads a scenario and returns an investigator over it.
	Build func(dir string) (Investigator, *fixture.Scenario, error)
	// Progress, if set, is called after each run (never concurrently).
	Progress func(done, total int, r RunResult)
}

// Run statuses that do not come from a report.
const (
	StatusError      = "error"       // investigator returned an error and no report
	StatusBuildError = "build_error" // scenario could not be loaded
	StatusCanceled   = "canceled"
)

// RunResult is one investigation of one scenario.
type RunResult struct {
	Scenario        string  `json:"scenario"`
	Run             int     `json:"run"`
	Status          string  `json:"status"`
	Error           string  `json:"error,omitempty"`
	Model           string  `json:"model,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	Score
}

// Rate is hits out of of.
type Rate struct {
	Hits int     `json:"hits"`
	Of   int     `json:"of"`
	Rate float64 `json:"rate"`
}

func (r *Rate) add(hit bool) {
	r.Of++
	if hit {
		r.Hits++
	}
}

func (r *Rate) finish() {
	if r.Of > 0 {
		r.Rate = float64(r.Hits) / float64(r.Of)
	}
}

// Calibration compares the top hypothesis' confidence on correct vs wrong
// runs. A well calibrated investigator is more confident when right.
type Calibration struct {
	Correct               int     `json:"correct"`
	MeanConfidenceCorrect float64 `json:"mean_confidence_correct"`
	Wrong                 int     `json:"wrong"`
	MeanConfidenceWrong   float64 `json:"mean_confidence_wrong"`
}

// Summary aggregates runs.
type Summary struct {
	Runs           int  `json:"runs"`
	Completed      Rate `json:"completed"`       // status == completed
	Top1Service    Rate `json:"top1_service"`    // over all runs
	Top3Service    Rate `json:"top3_service"`    // over all runs
	ChangeMatch    Rate `json:"change_match"`    // over runs whose truth has a change
	SpuriousChange Rate `json:"spurious_change"` // over runs with hypotheses whose truth has no change; lower is better
	CategoryMatch  Rate `json:"category_match"`
	KeywordHit     Rate `json:"keyword_hit"`
	CitationsValid Rate `json:"citations_valid"` // over runs with hypotheses
	Correct        Rate `json:"correct"`

	MeanTokens          float64     `json:"mean_tokens"` // over runs with a report
	MeanSteps           float64     `json:"mean_steps"`
	MeanDurationSeconds float64     `json:"mean_duration_seconds"` // over all runs
	Calibration         Calibration `json:"calibration"`
}

// ScenarioResult is every run of one scenario.
type ScenarioResult struct {
	Name    string        `json:"name"`
	Dir     string        `json:"dir"`
	Truth   fixture.Truth `json:"truth"`
	Runs    []RunResult   `json:"runs"`
	Summary Summary       `json:"summary"`
}

// Results is the outcome of Run.
type Results struct {
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at"`
	Model         string           `json:"model,omitempty"`
	RunsPerSample int              `json:"runs_per_scenario"`
	Scenarios     []ScenarioResult `json:"scenarios"`
	Overall       Summary          `json:"overall"`
}

// Summarize aggregates runs.
func Summarize(runs []RunResult) Summary {
	var s Summary
	var tokens, steps, withReport int
	var dur, confOK, confBad float64
	for _, r := range runs {
		s.Runs++
		dur += r.DurationSeconds
		s.Completed.add(r.Status == core.StatusCompleted)
		s.Top1Service.add(r.Top1Service)
		s.Top3Service.add(r.Top3Service)
		if r.ChangeMatch != nil {
			s.ChangeMatch.add(*r.ChangeMatch)
		}
		if r.SpuriousChange != nil && r.Hypotheses > 0 {
			s.SpuriousChange.add(*r.SpuriousChange)
		}
		s.CategoryMatch.add(r.CategoryMatch)
		s.KeywordHit.add(r.KeywordHit)
		s.Correct.add(r.Correct)
		if r.Hypotheses > 0 {
			s.CitationsValid.add(r.CitationsValid)
			if r.Correct {
				s.Calibration.Correct++
				confOK += r.TopConfidence
			} else {
				s.Calibration.Wrong++
				confBad += r.TopConfidence
			}
		}
		if r.HasReport {
			withReport++
			tokens += r.Tokens
			steps += r.Steps
		}
	}
	for _, r := range []*Rate{&s.Completed, &s.Top1Service, &s.Top3Service, &s.ChangeMatch, &s.SpuriousChange,
		&s.CategoryMatch, &s.KeywordHit, &s.CitationsValid, &s.Correct} {
		r.finish()
	}
	if withReport > 0 {
		s.MeanTokens = float64(tokens) / float64(withReport)
		s.MeanSteps = float64(steps) / float64(withReport)
	}
	if s.Runs > 0 {
		s.MeanDurationSeconds = dur / float64(s.Runs)
	}
	if s.Calibration.Correct > 0 {
		s.Calibration.MeanConfidenceCorrect = confOK / float64(s.Calibration.Correct)
	}
	if s.Calibration.Wrong > 0 {
		s.Calibration.MeanConfidenceWrong = confBad / float64(s.Calibration.Wrong)
	}
	return s
}

// Run investigates every scenario opts.Runs times and scores the reports.
// On cancellation it returns the partial results along with ctx.Err().
func Run(ctx context.Context, opts Options) (*Results, error) {
	if opts.Build == nil {
		return nil, errors.New("eval: Options.Build is required")
	}
	if len(opts.Scenarios) == 0 {
		return nil, errors.New("eval: no scenarios")
	}
	runs := max(opts.Runs, 1)
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 2
	}
	res := &Results{StartedAt: time.Now().UTC(), RunsPerSample: runs}

	type built struct {
		inv Investigator
		sc  *fixture.Scenario
		err error
	}
	scs := make([]built, len(opts.Scenarios))
	res.Scenarios = make([]ScenarioResult, len(opts.Scenarios))
	for i, dir := range opts.Scenarios {
		inv, sc, err := opts.Build(dir)
		if err == nil && (inv == nil || sc == nil) {
			err = errors.New("build returned no investigator or scenario")
		}
		scs[i] = built{inv, sc, err}
		name := filepath.Base(filepath.Clean(dir))
		sr := ScenarioResult{Name: name, Dir: dir, Runs: make([]RunResult, runs)}
		if err == nil {
			if sc.Name != "" {
				sr.Name = sc.Name
			}
			sr.Truth = sc.Truth
		}
		res.Scenarios[i] = sr
	}

	type job struct{ si, run int }
	jobs := make(chan job)
	var mu sync.Mutex
	done, total := 0, len(opts.Scenarios)*runs
	var wg sync.WaitGroup
	for range min(conc, total) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				b := scs[j.si]
				name := res.Scenarios[j.si].Name
				var r RunResult
				switch {
				case b.err != nil:
					r = RunResult{Status: StatusBuildError, Error: b.err.Error(), Score: ScoreReport(res.Scenarios[j.si].Truth, nil)}
				case ctx.Err() != nil:
					r = RunResult{Status: StatusCanceled, Error: ctx.Err().Error(), Score: ScoreReport(b.sc.Truth, nil)}
				default:
					r = runOne(ctx, b.inv, b.sc, name, j.run)
				}
				r.Scenario, r.Run = name, j.run
				mu.Lock()
				res.Scenarios[j.si].Runs[j.run-1] = r
				done++
				if opts.Progress != nil {
					opts.Progress(done, total, r)
				}
				mu.Unlock()
			}
		}()
	}
	for run := 1; run <= runs; run++ {
		for si := range opts.Scenarios {
			jobs <- job{si, run}
		}
	}
	close(jobs)
	wg.Wait()

	var all []RunResult
	for i := range res.Scenarios {
		sr := &res.Scenarios[i]
		sr.Summary = Summarize(sr.Runs)
		all = append(all, sr.Runs...)
		for _, r := range sr.Runs {
			if res.Model == "" && r.Model != "" {
				res.Model = r.Model
			}
		}
	}
	res.Overall = Summarize(all)
	res.FinishedAt = time.Now().UTC()
	return res, ctx.Err()
}

func runOne(ctx context.Context, inv Investigator, sc *fixture.Scenario, name string, run int) (r RunResult) {
	inc := sc.Incident
	inc.ID = fmt.Sprintf("eval-%s-%d", name, run)
	inc.Source = "eval"
	start := time.Now()
	defer func() {
		r.DurationSeconds = time.Since(start).Seconds()
		if p := recover(); p != nil {
			r.Status, r.Error = StatusError, fmt.Sprintf("panic: %v", p)
			r.Score = ScoreReport(sc.Truth, nil)
		}
	}()
	rep, err := inv.Run(ctx, inc, nil)
	r.Score = ScoreReport(sc.Truth, rep)
	if err != nil {
		r.Error = err.Error()
	}
	switch {
	case rep != nil:
		r.Status = rep.Status
		r.Model = rep.Model
		if r.Status == "" {
			r.Status = core.StatusCompleted
			if err != nil {
				r.Status = core.StatusFailed
			}
		}
	default:
		r.Status = StatusError
		if err == nil {
			r.Error = "investigator returned no report"
		}
	}
	return r
}

// Discover lists scenario directories (those containing scenario.yaml) under
// root, sorted by name. If only is non-empty, just those names are returned,
// and unknown names are an error.
func Discover(root string, only []string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	found := map[string]string{}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "scenario.yaml")); err != nil {
			continue
		}
		found[e.Name()] = dir
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var want []string
	for _, o := range only {
		if o = strings.TrimSpace(o); o != "" {
			want = append(want, o)
		}
	}
	if len(want) == 0 {
		dirs := make([]string, len(names))
		for i, n := range names {
			dirs[i] = found[n]
		}
		if len(dirs) == 0 {
			return nil, fmt.Errorf("eval: no scenarios under %s", root)
		}
		return dirs, nil
	}
	var dirs, missing []string
	for _, n := range want {
		if d, ok := found[n]; ok {
			dirs = append(dirs, d)
		} else {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("eval: unknown scenarios %s (have %s)", strings.Join(missing, ", "), strings.Join(names, ", "))
	}
	return dirs, nil
}
