// Package correlate ranks recent changes and services as likely causes.
package correlate

import (
	"time"

	"github.com/shadatr/culprit/internal/analysis/anomaly"
	"github.com/shadatr/culprit/internal/analysis/logpattern"
	"github.com/shadatr/culprit/internal/core"
)

// ScoredChange is a change with a likelihood score and the reasons for it.
type ScoredChange struct {
	Change  core.Change `json:"change"`
	Score   float64     `json:"score"` // 0..1
	Reasons []string    `json:"reasons"`
}

// ScoredService is a service with a likelihood of being the root cause.
type ScoredService struct {
	Service       string    `json:"service"`
	Score         float64   `json:"score"` // 0..1
	EarliestOnset time.Time `json:"earliest_onset,omitempty"`
	Reasons       []string  `json:"reasons"`
}

// RankChanges scores changes by timing relative to anomaly onsets, service
// overlap with anomalies and log shifts, and topology. Sorted by Score desc.
// TODO(WS-C): implement.
func RankChanges(changes []core.Change, an []anomaly.Anomaly, sh []logpattern.Shift, topo core.Topology, incidentStart time.Time) []ScoredChange {
	return nil
}

// SuspectServices localises the likely root-cause service: earliest onset,
// strongest signals, and position downstream of the symptomatic service in
// the topology. Sorted by Score desc.
// TODO(WS-C): implement.
func SuspectServices(an []anomaly.Anomaly, sh []logpattern.Shift, topo core.Topology, symptom string) []ScoredService {
	return nil
}
