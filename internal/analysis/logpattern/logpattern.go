// Package logpattern groups log lines into templates and compares template
// frequencies before and during an incident.
package logpattern

import (
	"time"

	"github.com/shadatr/culprit/internal/core"
)

// Template is a group of similar log lines, e.g.
// "timeout after <*> ms calling <*>".
type Template struct {
	ID      string    `json:"id"` // stable hash of Pattern
	Pattern string    `json:"pattern"`
	Count   int       `json:"count"`
	Level   string    `json:"level,omitempty"` // most common level
	Service string    `json:"service,omitempty"`
	Sample  string    `json:"sample"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
}

// Shift is a template whose rate changed between baseline and incident.
type Shift struct {
	Template        Template  `json:"template"`
	BaselineRate    float64   `json:"baseline_rate"` // lines per minute before incident start
	IncidentRate    float64   `json:"incident_rate"` // lines per minute from incident start
	New             bool      `json:"new"`           // never seen in baseline
	Score           float64   `json:"score"`
	FirstInIncident time.Time `json:"first_in_incident"`
}

// Mine groups lines into templates sorted by Count, highest first.
// TODO(WS-C): implement.
func Mine(lines []core.LogLine) []Template { return nil }

// Diff returns templates whose rate rose (or that are new) after
// incidentStart, sorted by Score, highest first.
// TODO(WS-C): implement.
func Diff(lines []core.LogLine, incidentStart time.Time) []Shift { return nil }
