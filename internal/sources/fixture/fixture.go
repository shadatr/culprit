// Package fixture loads offline scenario directories (PLAN.md §5) and serves
// them through the core source interfaces.
package fixture

import (
	"errors"

	"github.com/shadatr/culprit/internal/core"
)

// Truth is the known root cause of a scenario.
type Truth struct {
	Service  string   `yaml:"service" json:"service"`
	ChangeID string   `yaml:"change_id" json:"change_id,omitempty"`
	Category string   `yaml:"category" json:"category"`
	Keywords []string `yaml:"keywords" json:"keywords"`
}

// Scenario is a loaded scenario.
type Scenario struct {
	Name        string
	Description string
	Dir         string
	Incident    core.Incident // Window computed from lookback/after; Source = "eval"
	Topology    core.Topology
	Truth       Truth

	// TODO(WS-B): loaded data.
}

// Load reads dir. metrics.json, logs.jsonl and changes.json are optional.
// TODO(WS-B): implement.
func Load(dir string) (*Scenario, error) { return nil, errors.New("fixture: not implemented") }

// Metrics returns a MetricsSource over the scenario's series. Query supports
// selectors with =, !=, =~ and !~ matchers; points outside the window are
// dropped; step is ignored.
// TODO(WS-B): implement.
func (s *Scenario) Metrics() core.MetricsSource { return nil }

// Logs returns a LogsSource over the scenario's lines.
// TODO(WS-B): implement.
func (s *Scenario) Logs() core.LogsSource { return nil }

// Changes returns a ChangesSource over the scenario's changes.
// TODO(WS-B): implement.
func (s *Scenario) Changes() core.ChangesSource { return nil }
