// Package core holds the types shared by every part of Culprit and the
// interfaces that data sources implement.
package core

import (
	"context"
	"encoding/json"
	"time"
)

// Window is a closed time range.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Contains reports whether t falls inside w.
func (w Window) Contains(t time.Time) bool {
	return !t.Before(w.Start) && !t.After(w.End)
}

// Incident is what we are asked to investigate.
type Incident struct {
	ID        string            `json:"id,omitempty"`
	Title     string            `json:"title"`
	Service   string            `json:"service,omitempty"` // symptomatic service, if known
	StartedAt time.Time         `json:"started_at"`
	Window    Window            `json:"window"` // analysis window; baseline is Window.Start..StartedAt
	Labels    map[string]string `json:"labels,omitempty"`
	Source    string            `json:"source,omitempty"` // manual | alertmanager | cli | eval
}

// NewIncident builds an incident with a window of lookback before start and
// after past start.
func NewIncident(title, service string, start time.Time, lookback, after time.Duration) Incident {
	return Incident{
		Title:     title,
		Service:   service,
		StartedAt: start,
		Window:    Window{Start: start.Add(-lookback), End: start.Add(after)},
	}
}

// Point is one sample.
type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Series is one labelled time series.
type Series struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// MetricsSource answers selector queries (name{label="v",other=~"re"}).
type MetricsSource interface {
	Name() string
	ListMetrics(ctx context.Context, match string) ([]string, error)
	Query(ctx context.Context, selector string, w Window, step time.Duration) ([]Series, error)
}

// LogQuery filters log lines. Empty fields match everything.
type LogQuery struct {
	Service  string `json:"service,omitempty"`
	Contains string `json:"contains,omitempty"`
	Level    string `json:"level,omitempty"`
	Window   Window `json:"window"`
	Limit    int    `json:"limit,omitempty"` // 0 means source default (1000)
}

// LogLine is one log record.
type LogLine struct {
	T       time.Time         `json:"t"`
	Service string            `json:"service"`
	Level   string            `json:"level,omitempty"`
	Message string            `json:"message"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// LogsSource queries logs.
type LogsSource interface {
	Name() string
	Services(ctx context.Context, w Window) ([]string, error)
	Query(ctx context.Context, q LogQuery) ([]LogLine, error)
}

// Change is a deploy, commit, config change or feature-flag flip.
type Change struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // deploy | commit | config | flag | infra
	Service string    `json:"service,omitempty"`
	T       time.Time `json:"t"`
	Author  string    `json:"author,omitempty"`
	Summary string    `json:"summary"`
	Diff    string    `json:"diff,omitempty"` // may be truncated
	URL     string    `json:"url,omitempty"`
}

// ChangesSource lists changes in a window.
type ChangesSource interface {
	Name() string
	Changes(ctx context.Context, w Window) ([]Change, error)
}

// Topology maps a service to the services it calls.
type Topology map[string][]string

// Evidence is one recorded observation. IDs are E1, E2, ... per investigation.
type Evidence struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"` // metric | logs | log_patterns | changes | change | anomaly | suspects | topology
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input,omitempty"`
	Summary string          `json:"summary"`        // short text shown to the model and the UI
	Data    json.RawMessage `json:"data,omitempty"` // full payload for the UI
	At      time.Time       `json:"at"`
}

// Hypothesis is one candidate root cause.
type Hypothesis struct {
	Title       string   `json:"title"`
	Explanation string   `json:"explanation"`
	Service     string   `json:"service,omitempty"`
	Category    string   `json:"category,omitempty"` // see PLAN.md §5 truth.category
	ChangeID    string   `json:"change_id,omitempty"`
	Confidence  float64  `json:"confidence"` // 0..1
	EvidenceIDs []string `json:"evidence_ids"`
	NextSteps   []string `json:"next_steps,omitempty"`
}

// TimelineEntry is one point in the reconstructed incident timeline.
type TimelineEntry struct {
	T           time.Time `json:"t"`
	Text        string    `json:"text"`
	EvidenceIDs []string  `json:"evidence_ids,omitempty"`
}

// Usage counts what an investigation consumed.
type Usage struct {
	Steps            int `json:"steps"`
	ToolCalls        int `json:"tool_calls"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Report statuses.
const (
	StatusQueued         = "queued"
	StatusRunning        = "running"
	StatusCompleted      = "completed"
	StatusFailed         = "failed"
	StatusInvalidReport  = "invalid_report"
	StatusBudgetExceeded = "budget_exceeded"
)

// Report is the result of an investigation.
type Report struct {
	IncidentID string          `json:"incident_id"`
	Title      string          `json:"title,omitempty"` // incident title
	Status     string          `json:"status"`
	Summary    string          `json:"summary"`
	Timeline   []TimelineEntry `json:"timeline"`
	Hypotheses []Hypothesis    `json:"hypotheses"` // ranked, most likely first
	Evidence   []Evidence      `json:"evidence"`
	Usage      Usage           `json:"usage"`
	Model      string          `json:"model"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
}

// Event types.
const (
	EventStarted    = "started"
	EventTriage     = "triage"
	EventThinking   = "thinking"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventEvidence   = "evidence"
	EventReport     = "report"
	EventError      = "error"
	EventFinished   = "finished"
)

// Event is a progress notification. Data payloads:
//
//	started:     Incident
//	triage:      {"summary": string, "evidence_ids": []string}
//	thinking:    {"text": string}
//	tool_call:   {"id": string, "name": string, "arguments": object}
//	tool_result: {"id": string, "name": string, "evidence_id": string, "summary": string, "error": string}
//	evidence:    Evidence
//	report:      Report
//	error:       {"message": string}
//	finished:    {"status": string}
type Event struct {
	Seq  int             `json:"seq"`
	Type string          `json:"type"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// EventSink receives events. It must be safe to call from one goroutine at a
// time and must not block for long.
type EventSink func(Event)
