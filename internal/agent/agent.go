// Package agent runs an investigation: deterministic triage, then an LLM tool
// loop, then a validated report.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/llm"
	"github.com/shadatr/culprit/internal/tools"
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

// Defaults.
const (
	DefaultMaxSteps    = 12
	DefaultMaxDuration = 3 * time.Minute
	DefaultMaxTokens   = 120000
	DefaultTemperature = 0.1

	// maxRepairs is how many rejected submit_report calls the model may fix.
	maxRepairs = 2
)

func (c Config) withDefaults() Config {
	if c.MaxSteps <= 0 {
		c.MaxSteps = DefaultMaxSteps
	}
	if c.MaxDuration <= 0 {
		c.MaxDuration = DefaultMaxDuration
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = DefaultMaxTokens
	}
	if c.Temperature == 0 {
		c.Temperature = DefaultTemperature
	}
	return c
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
//
// The error is non-nil only for status failed (LLM failure, cancelled ctx,
// missing LLM client); completed, invalid_report and budget_exceeded
// reports come with a nil error.
func (i *Investigator) Run(ctx context.Context, inc core.Incident, sink core.EventSink) (*core.Report, error) {
	r := &run{
		deps:    i.deps,
		cfg:     i.cfg.withDefaults(),
		inc:     normaliseIncident(inc),
		sink:    sink,
		ledger:  tools.NewLedger(),
		started: time.Now().UTC(),
		log:     slog.Default().With("incident", inc.ID),
	}
	return r.investigate(ctx)
}

func normaliseIncident(inc core.Incident) core.Incident {
	if inc.StartedAt.IsZero() {
		inc.StartedAt = time.Now().UTC()
	}
	if inc.Window.Start.IsZero() {
		inc.Window.Start = inc.StartedAt.Add(-time.Hour)
	}
	if inc.Window.End.IsZero() || !inc.Window.End.After(inc.Window.Start) {
		inc.Window.End = inc.StartedAt.Add(30 * time.Minute)
	}
	return inc
}

// run is the state of one investigation; it is used by one goroutine.
type run struct {
	deps    Deps
	cfg     Config
	inc     core.Incident
	sink    core.EventSink
	seq     int
	ledger  *tools.Ledger
	triage  *tools.TriageResult
	usage   core.Usage
	started time.Time
	log     *slog.Logger

	lastSubmission *submission // most recent rejected submission
}

func (r *run) emit(typ string, data any) {
	if r.sink == nil {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"message": "unencodable event payload: " + err.Error()})
	}
	r.seq++
	r.sink(core.Event{Seq: r.seq, Type: typ, At: time.Now().UTC(), Data: b})
}

func (r *run) tokens() int { return r.usage.PromptTokens + r.usage.CompletionTokens }

func (r *run) investigate(ctx context.Context) (*core.Report, error) {
	r.emit(core.EventStarted, r.inc)
	r.log.Info("investigation started", "title", r.inc.Title, "service", r.inc.Service)
	if r.deps.LLM == nil {
		return r.fail(errors.New("agent: no LLM client configured"))
	}

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.MaxDuration)
	defer cancel()

	td := tools.Deps{
		Metrics:  r.deps.Metrics,
		Logs:     r.deps.Logs,
		Changes:  r.deps.Changes,
		Topology: r.deps.Topology,
		Ledger:   r.ledger,
	}
	tr, err := tools.Triage(runCtx, td, r.inc, r.cfg.TriageMetrics)
	if err != nil {
		return r.stopped(ctx, runCtx, fmt.Errorf("agent: triage: %w", err), "time during triage")
	}
	r.triage = tr
	for _, e := range r.ledger.List() {
		r.emit(core.EventEvidence, e)
	}
	r.emit(core.EventTriage, map[string]any{"summary": tr.Summary, "evidence_ids": nonNil(tr.EvidenceIDs)})

	tb := tools.New(td, r.inc, tr)
	defs := tools.Definitions()
	msgs := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: incidentPrompt(r.inc, tr.Summary)},
	}
	temp := r.cfg.Temperature

	var (
		nudged, force, warned, pendingRepair bool
		rejected                             int
	)
	for {
		if ctx.Err() != nil || runCtx.Err() != nil {
			return r.stopped(ctx, runCtx, ctx.Err(), "time")
		}
		// Hard budgets. A pending repair may use up to maxRepairs extra steps.
		repairGrace := pendingRepair && r.usage.Steps < r.cfg.MaxSteps+maxRepairs
		if r.usage.Steps >= r.cfg.MaxSteps && !repairGrace {
			return r.budgetExceeded(fmt.Sprintf("%d steps", r.cfg.MaxSteps))
		}
		if r.tokens() >= r.cfg.MaxTokens && !pendingRepair {
			return r.budgetExceeded(fmt.Sprintf("%d tokens", r.cfg.MaxTokens))
		}

		// Soft budgets: ask for the report.
		elapsed := time.Since(r.started)
		var near string
		switch {
		case r.usage.Steps >= r.cfg.MaxSteps-1:
			near = fmt.Sprintf("step %d of %d", r.usage.Steps+1, r.cfg.MaxSteps)
		case r.tokens() >= r.cfg.MaxTokens*8/10:
			near = fmt.Sprintf("%d of %d tokens used", r.tokens(), r.cfg.MaxTokens)
		case elapsed >= r.cfg.MaxDuration*3/4:
			near = fmt.Sprintf("%s of %s elapsed", elapsed.Round(time.Second), r.cfg.MaxDuration)
		}
		if near != "" && !warned {
			warned = true
			msgs = append(msgs, llm.Message{Role: "user", Content: fmt.Sprintf(budgetText, near)})
		}
		choice := "auto"
		if near != "" || force {
			choice = tools.ToolSubmitReport
		}

		req := llm.Request{
			Model:       r.cfg.Model,
			Messages:    msgs,
			Tools:       defs,
			ToolChoice:  choice,
			Temperature: &temp,
		}
		resp, err := r.deps.LLM.Chat(runCtx, req)
		if err != nil {
			return r.stopped(ctx, runCtx, fmt.Errorf("agent: model call: %w", err), "time")
		}
		r.usage.Steps++
		r.addUsage(req, resp)

		msg := resp.Message
		msg.Role = "assistant"
		msg.ToolCallID, msg.Name = "", ""
		if strings.TrimSpace(msg.Content) != "" {
			r.emit(core.EventThinking, map[string]string{"text": msg.Content})
		}
		msgs = append(msgs, msg)

		if len(msg.ToolCalls) == 0 {
			// Plain-text answer: nudge once, then force submit_report.
			if !nudged {
				nudged = true
				msgs = append(msgs, llm.Message{Role: "user", Content: nudgeText})
			} else {
				force = true
				msgs = append(msgs, llm.Message{Role: "user", Content: "Call submit_report now."})
			}
			continue
		}

		pendingRepair = false
		var final *core.Report
		for _, tc := range msg.ToolCalls {
			r.usage.ToolCalls++
			r.emit(core.EventToolCall, map[string]any{"id": tc.ID, "name": tc.Name, "arguments": argsForEvent(tc.Arguments)})

			if tc.Name != tools.ToolSubmitReport {
				content := r.dispatch(runCtx, tb, tc)
				msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: content})
				continue
			}

			if final != nil {
				msgs = append(msgs, toolMsg(tc, "A report was already accepted."))
				continue
			}
			sub, perr := parseSubmission(tc.Arguments)
			var problems []string
			if perr != nil {
				problems = []string{perr.Error()}
			} else {
				problems = validate(sub, r.ledger.Has)
			}
			if len(problems) == 0 {
				final = r.report(core.StatusCompleted, sub)
				r.emit(core.EventToolResult, map[string]string{"id": tc.ID, "name": tc.Name, "evidence_id": "", "summary": "report accepted", "error": ""})
				msgs = append(msgs, toolMsg(tc, "Report accepted."))
				continue
			}
			rejected++
			if perr == nil {
				s := sub
				r.lastSubmission = &s
			}
			feedback := "Report rejected. Fix these problems and call submit_report again:\n- " + strings.Join(problems, "\n- ") +
				"\nValid evidence IDs: " + r.idList()
			r.emit(core.EventToolResult, map[string]string{"id": tc.ID, "name": tc.Name, "evidence_id": "", "summary": "report rejected", "error": strings.Join(problems, "; ")})
			msgs = append(msgs, toolMsg(tc, feedback))
			r.log.Info("report rejected", "attempt", rejected, "problems", len(problems))
			if rejected > maxRepairs {
				return r.invalidReport(problems)
			}
			force, pendingRepair = true, true
		}
		if final != nil {
			return r.finish(final, nil)
		}
	}
}

// dispatch runs one non-report tool and returns the content for the model.
func (r *run) dispatch(ctx context.Context, tb *tools.Toolbox, tc llm.ToolCall) string {
	id, text, err := tb.Dispatch(ctx, tc.Name, tc.Arguments)
	ev := map[string]string{"id": tc.ID, "name": tc.Name, "evidence_id": id, "summary": text, "error": ""}
	if err != nil {
		ev["error"] = err.Error()
		r.emit(core.EventToolResult, ev)
		return "error: " + err.Error()
	}
	r.emit(core.EventToolResult, ev)
	if id != "" {
		if e, ok := r.ledger.Get(id); ok {
			r.emit(core.EventEvidence, e)
		}
		return "evidence_id: " + id + "\n" + text
	}
	return text
}

func toolMsg(tc llm.ToolCall, content string) llm.Message {
	return llm.Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: content}
}

// argsForEvent returns arguments as a JSON object for the tool_call event.
func argsForEvent(a json.RawMessage) json.RawMessage {
	if len(a) == 0 || !json.Valid(a) {
		return json.RawMessage("{}")
	}
	return a
}

func (r *run) idList() string {
	ev := r.ledger.List()
	if len(ev) == 0 {
		return "(none)"
	}
	ids := make([]string, len(ev))
	for i, e := range ev {
		ids[i] = e.ID
	}
	return strings.Join(ids, ", ")
}

// addUsage adds reported token usage, estimating it (about 4 bytes per
// token) when the server reports none so token budgets still apply.
func (r *run) addUsage(req llm.Request, resp *llm.Response) {
	p, c := resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	if p == 0 && c == 0 {
		n := 0
		for _, m := range req.Messages {
			n += len(m.Content)
			for _, tc := range m.ToolCalls {
				n += len(tc.Arguments)
			}
		}
		for _, d := range req.Tools {
			n += len(d.Description) + len(d.Parameters)
		}
		p = n / 4
		c = len(resp.Message.Content) / 4
		for _, tc := range resp.Message.ToolCalls {
			c += len(tc.Arguments) / 4
		}
	}
	r.usage.PromptTokens += p
	r.usage.CompletionTokens += c
}

func (r *run) report(status string, s submission) *core.Report {
	rep := &core.Report{
		IncidentID: r.inc.ID,
		Title:      r.inc.Title,
		Status:     status,
		Summary:    s.Summary,
		Timeline:   s.Timeline,
		Hypotheses: s.Hypotheses,
		Evidence:   r.ledger.List(),
		Usage:      r.usage,
		Model:      r.cfg.Model,
		StartedAt:  r.started,
		FinishedAt: time.Now().UTC(),
	}
	if rep.Timeline == nil {
		rep.Timeline = []core.TimelineEntry{}
	}
	if rep.Hypotheses == nil {
		rep.Hypotheses = []core.Hypothesis{}
	}
	if rep.Evidence == nil {
		rep.Evidence = []core.Evidence{}
	}
	return rep
}

// stopped handles a model or triage failure: a cancelled parent is a
// failure; our own deadline is a time budget; anything else fails.
func (r *run) stopped(parent, runCtx context.Context, err error, budget string) (*core.Report, error) {
	switch {
	case parent.Err() != nil:
		return r.fail(fmt.Errorf("agent: investigation cancelled: %w", parent.Err()))
	case runCtx.Err() != nil:
		return r.budgetExceeded(fmt.Sprintf("%s (%s)", budget, r.cfg.MaxDuration))
	case err != nil:
		return r.fail(err)
	}
	return r.fail(errors.New("agent: stopped"))
}

func (r *run) fail(err error) (*core.Report, error) {
	r.log.Warn("investigation failed", "err", err)
	rep := r.report(core.StatusFailed, submission{Summary: "Investigation failed: " + err.Error()})
	r.emit(core.EventError, map[string]string{"message": err.Error()})
	return r.finish(rep, err)
}

func (r *run) budgetExceeded(what string) (*core.Report, error) {
	msg := "budget exhausted (" + what + ") before a valid report was submitted"
	r.log.Info("budget exceeded", "budget", what)
	sub := r.fallback("Investigation stopped: " + msg + ".")
	r.emit(core.EventError, map[string]string{"message": msg})
	return r.finish(r.report(core.StatusBudgetExceeded, sub), nil)
}

func (r *run) invalidReport(problems []string) (*core.Report, error) {
	msg := fmt.Sprintf("model failed to submit a valid report after %d repair attempts: %s", maxRepairs, strings.Join(problems, "; "))
	sub := r.fallback("Investigation ended with an invalid report: " + msg + ".")
	r.emit(core.EventError, map[string]string{"message": msg})
	return r.finish(r.report(core.StatusInvalidReport, sub), nil)
}

// fallback builds the best report content available without a valid
// submission: the last rejected submission with unknown citations removed,
// or else unconfirmed triage suspects.
func (r *run) fallback(note string) submission {
	if s := r.lastSubmission; s != nil {
		out := sanitise(*s, r.ledger.Has)
		out.Summary = strings.TrimSpace(note + " Last submitted (unvalidated) summary: " + s.Summary)
		return out
	}
	out := submission{Summary: note}
	if r.triage == nil || len(r.triage.Suspects) == 0 {
		return out
	}
	var suspectsID string
	for _, e := range r.ledger.List() {
		if e.Kind == "suspects" {
			suspectsID = e.ID
			break
		}
	}
	if suspectsID == "" {
		return out
	}
	out.Summary += " Hypotheses below are unconfirmed triage leads."
	for _, s := range r.triage.Suspects[:min(3, len(r.triage.Suspects))] {
		out.Hypotheses = append(out.Hypotheses, core.Hypothesis{
			Title:       "Unconfirmed triage suspect: " + s.Service,
			Explanation: strings.Join(s.Reasons, "; "),
			Service:     s.Service,
			Confidence:  max(0, min(1, s.Score)) * 0.5,
			EvidenceIDs: []string{suspectsID},
		})
	}
	return out
}

func (r *run) finish(rep *core.Report, err error) (*core.Report, error) {
	rep.Usage = r.usage
	rep.Evidence = r.ledger.List()
	if rep.Evidence == nil {
		rep.Evidence = []core.Evidence{}
	}
	rep.FinishedAt = time.Now().UTC()
	r.emit(core.EventReport, rep)
	r.emit(core.EventFinished, map[string]string{"status": rep.Status})
	r.log.Info("investigation finished", "status", rep.Status, "steps", r.usage.Steps, "tool_calls", r.usage.ToolCalls, "tokens", r.tokens())
	return rep, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
