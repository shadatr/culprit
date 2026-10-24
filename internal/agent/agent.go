// Package agent runs an investigation: deterministic triage, then an LLM tool
// loop, then a validated report.
package agent

import (
	"context"
	"errors"
	"time"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/llm"
)

// Deps are the investigator's collaborators. Any source may be nil.
type Deps struct {
	LLM      llm.Client
	Metrics  core.MetricsSource
	Logs     core.LogsSource
	Changes  core.ChangesSource
	Topology core.Topology
}

// Config sets the model and budgets. Zero values take defaults.
type Config struct {
	Model         string
	MaxSteps      int           // default 12
	MaxDuration   time.Duration // default 3m
	MaxTokens     int           // default 120000
	TriageMetrics []string      // selectors to scan in triage; empty = all if source lists <= 60
	Temperature   float64       // default 0.1
}

// Investigator runs investigations. It is safe for concurrent use.
type Investigator struct {
	deps Deps
	cfg  Config
}

// New returns an Investigator.
func New(deps Deps, cfg Config) *Investigator { return &Investigator{deps: deps, cfg: cfg} }

// Run investigates inc, emitting events to sink (may be nil). It returns a
// report whenever one could be built, even on failure statuses.
// TODO(WS-A): implement.
func (i *Investigator) Run(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
	return nil, errors.New("agent: not implemented")
}
