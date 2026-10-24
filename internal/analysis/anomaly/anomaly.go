// Package anomaly finds metric series that changed around an incident and
// estimates when the change began.
package anomaly

import (
	"time"

	"github.com/shadatr/culprit/internal/core"
)

// Options controls detection.
type Options struct {
	IncidentStart time.Time // baseline is everything before this
	MinScore      float64   // drop anomalies scoring below this; 0 means default (3)
	MaxResults    int       // 0 means no limit
}

// Anomaly is a series whose incident behaviour differs from its baseline.
type Anomaly struct {
	Metric    string            `json:"metric"`
	Labels    map[string]string `json:"labels"`
	Service   string            `json:"service,omitempty"` // Labels["service"]
	Direction string            `json:"direction"`         // up | down
	Onset     time.Time         `json:"onset"`             // estimated start of the change
	Baseline  float64           `json:"baseline"`          // typical value before
	Peak      float64           `json:"peak"`              // most extreme value after onset
	Score     float64           `json:"score"`             // robust z-score style magnitude
}

// Stats describes one series before and after the incident start.
type Stats struct {
	Metric        string            `json:"metric"`
	Labels        map[string]string `json:"labels"`
	BeforeMean    float64           `json:"before_mean"`
	BeforeP95     float64           `json:"before_p95"`
	AfterMean     float64           `json:"after_mean"`
	AfterMax      float64           `json:"after_max"`
	ChangePercent float64           `json:"change_percent"`
	Anomaly       *Anomaly          `json:"anomaly,omitempty"`
}

// Detect returns anomalies sorted by Score, highest first.
// TODO(WS-C): implement.
func Detect(series []core.Series, opts Options) []Anomaly { return nil }

// Describe summarises one series around incidentStart.
// TODO(WS-C): implement.
func Describe(s core.Series, incidentStart time.Time) Stats {
	return Stats{Metric: s.Name, Labels: s.Labels}
}
