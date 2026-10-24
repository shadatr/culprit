package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shadatr/culprit/internal/agent"
	"github.com/shadatr/culprit/internal/app"
	"github.com/shadatr/culprit/internal/config"
	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/report"
	"github.com/shadatr/culprit/internal/server"
	"github.com/shadatr/culprit/internal/tools"
)

func runInvestigate(args []string) error {
	fs := flag.NewFlagSet("investigate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file (default: built-in defaults)")
	scenario := fs.String("scenario", "", "fixture scenario directory; the incident comes from scenario.yaml")
	title := fs.String("title", "", "incident title (live mode, required)")
	service := fs.String("service", "", "symptomatic service (live mode)")
	since := fs.Duration("since", time.Hour, "lookback before the incident start (live mode)")
	startStr := fs.String("start", "", "incident start, RFC3339 (live mode, default now)")
	after := fs.Duration("after", 30*time.Minute, "window past the incident start, capped at now (live mode)")
	model := fs.String("model", "", "override the configured LLM model")
	asJSON := fs.Bool("json", false, "print the report as JSON instead of Markdown")
	quiet := fs.Bool("quiet", false, "do not print progress to stderr")
	triageOnly := fs.Bool("triage-only", false, "run deterministic triage only (no LLM) and print its summary")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: culprit investigate [-config file] (-scenario dir | -title text [-service name] [-start time] [-since 1h] [-after 30m]) [-json] [-quiet]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, err := loadConfigFlag(*cfgPath)
	if err != nil {
		return err
	}
	if *model != "" {
		cfg.LLM.Model = *model
	}

	var (
		inv  server.Investigator
		inc  core.Incident
		deps agent.Deps
	)
	client := app.LLM(cfg)
	if *scenario != "" {
		i, sc, err := app.Scenario(cfg, client, *scenario)
		if err != nil {
			return err
		}
		inv, inc = i, sc.Incident
		deps = agent.Deps{Metrics: sc.Metrics(), Logs: sc.Logs(), Changes: sc.Changes(), Topology: sc.Topology}
	} else {
		if strings.TrimSpace(*title) == "" {
			fs.Usage()
			return errors.New("investigate: -title or -scenario is required")
		}
		now := time.Now().UTC()
		start := now
		if *startStr != "" {
			start, err = time.Parse(time.RFC3339, *startStr)
			if err != nil {
				return fmt.Errorf("investigate: -start: %w", err)
			}
		}
		inc = core.NewIncident(strings.TrimSpace(*title), *service, start, *since, *after)
		if inc.Window.End.After(now) {
			inc.Window.End = now
		}
		if !inc.Window.End.After(inc.Window.Start) {
			return errors.New("investigate: empty window; check -start and -since")
		}
		inc.Source = "cli"
		deps, err = app.LiveDeps(cfg, client)
		if err != nil {
			return err
		}
		inv = agent.New(deps, app.AgentConfig(cfg))
	}
	if inc.ID == "" {
		inc.ID = fmt.Sprintf("cli-%d", time.Now().Unix())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *triageOnly {
		tr, err := tools.Triage(ctx, tools.Deps{
			Metrics: deps.Metrics, Logs: deps.Logs, Changes: deps.Changes,
			Topology: deps.Topology, Ledger: tools.NewLedger(),
		}, inc, cfg.Sources.Prometheus.TriageMetrics)
		if err != nil {
			return fmt.Errorf("investigate: triage: %w", err)
		}
		fmt.Println(tr.Summary)
		return nil
	}

	var sink core.EventSink
	if !*quiet {
		sink = newProgressPrinter(os.Stderr, isTerminal(os.Stderr)).event
	}
	rep, runErr := inv.Run(ctx, inc, sink)
	if rep == nil {
		if runErr == nil {
			runErr = errors.New("no report produced")
		}
		return fmt.Errorf("investigate: %w", runErr)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return fmt.Errorf("investigate: %w", err)
		}
	} else {
		fmt.Print(report.Markdown(rep))
	}
	if runErr != nil {
		return fmt.Errorf("investigate: %w", runErr)
	}
	if rep.Status != core.StatusCompleted {
		return fmt.Errorf("investigate: finished with status %s", rep.Status)
	}
	return nil
}

// loadConfigFlag reads path, or returns defaults when path is empty.
func loadConfigFlag(path string) (*config.Config, error) {
	if path == "" {
		return config.Default(), nil
	}
	return config.Load(path)
}

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// progress prints compact live progress from events.
type progressPrinter struct {
	w     io.Writer
	color bool
	start time.Time
}

func newProgressPrinter(w io.Writer, color bool) *progressPrinter {
	return &progressPrinter{w: w, color: color, start: time.Now()}
}

const (
	ansiDim   = "\x1b[2m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
	ansiBold  = "\x1b[1m"
	ansiReset = "\x1b[0m"
)

func (p *progressPrinter) paint(code, s string) string {
	if !p.color {
		return s
	}
	return code + s + ansiReset
}

func (p *progressPrinter) line(format string, a ...any) {
	elapsed := time.Since(p.start).Truncate(100 * time.Millisecond)
	fmt.Fprintf(p.w, "%s %s\n", p.paint(ansiDim, fmt.Sprintf("%6s", fmtSeconds(elapsed))), fmt.Sprintf(format, a...))
}

func fmtSeconds(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (p *progressPrinter) event(ev core.Event) {
	switch ev.Type {
	case core.EventStarted:
		var inc core.Incident
		_ = json.Unmarshal(ev.Data, &inc)
		svc := ""
		if inc.Service != "" {
			svc = " on " + inc.Service
		}
		p.line("%s %q%s, window %s .. %s", p.paint(ansiBold, "investigating"), inc.Title, svc,
			inc.Window.Start.UTC().Format("15:04"), inc.Window.End.UTC().Format("15:04 MST"))
	case core.EventTriage:
		var d struct {
			Summary     string   `json:"summary"`
			EvidenceIDs []string `json:"evidence_ids"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		p.line("%s %d findings %s", p.paint(ansiCyan, "triage done:"), len(d.EvidenceIDs), p.paint(ansiDim, evidenceRange(d.EvidenceIDs)))
	case core.EventThinking:
		var d struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if t := clipLine(d.Text, 100); t != "" {
			p.line("%s", p.paint(ansiDim, "thinking: "+t))
		}
	case core.EventToolCall:
		var d struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		p.line("%s %s %s", p.paint(ansiCyan, "->"), d.Name, shortToolArgs(d.Arguments))
	case core.EventToolResult:
		var d struct {
			Name       string `json:"name"`
			EvidenceID string `json:"evidence_id"`
			Summary    string `json:"summary"`
			Error      string `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if d.Error != "" {
			p.line("   %s %s", p.paint(ansiRed, "<- error:"), clipLine(d.Error, 120))
			return
		}
		id := d.EvidenceID
		if id == "" {
			id = "-"
		}
		p.line("   <- %s %s", p.paint(ansiGreen, id), p.paint(ansiDim, clipLine(d.Summary, 100)))
	case core.EventError:
		var d struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		p.line("%s %s", p.paint(ansiRed, "error:"), d.Message)
	case core.EventReport:
		var r core.Report
		_ = json.Unmarshal(ev.Data, &r)
		top := ""
		if len(r.Hypotheses) > 0 {
			top = fmt.Sprintf(": %s (%.0f%%)", r.Hypotheses[0].Title, r.Hypotheses[0].Confidence*100)
		}
		p.line("%s %d hypotheses%s", p.paint(ansiBold, "report:"), len(r.Hypotheses), top)
	case core.EventFinished:
		var d struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		code := ansiGreen
		if d.Status != core.StatusCompleted {
			code = ansiRed
		}
		p.line("%s %s", p.paint(ansiBold, "finished:"), p.paint(code, d.Status))
	}
}

// shortToolArgs renders a JSON object as sorted k=v pairs with long values cut.
func shortToolArgs(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, ok := m[k].(string)
		if !ok {
			b, _ := json.Marshal(m[k])
			v = string(b)
		}
		parts = append(parts, k+"="+clipLine(v, 48))
	}
	return strings.Join(parts, " ")
}

// clipLine collapses whitespace and truncates s to n runes.
func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// evidenceRange shows evidence IDs compactly: "E1..E7" or "E1, E3".
func evidenceRange(ids []string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1, 2, 3:
		return "(" + strings.Join(ids, ", ") + ")"
	default:
		return "(" + ids[0] + ".." + ids[len(ids)-1] + ")"
	}
}
