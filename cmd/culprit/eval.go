package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/shadatr/culprit/internal/app"
	"github.com/shadatr/culprit/internal/config"
	"github.com/shadatr/culprit/internal/eval"
	"github.com/shadatr/culprit/internal/sources/fixture"
)

// runEval scores Culprit against fixture scenarios (see EVAL.md).
func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	scenarios := fs.String("scenarios", "eval/scenarios", "directory containing scenario directories")
	only := fs.String("only", "", "comma-separated scenario names to run (default all)")
	runs := fs.Int("runs", 1, "runs per scenario")
	conc := fs.Int("concurrency", 2, "parallel investigations")
	cfgPath := fs.String("config", "", "config file (optional; LLM and budgets are read from it)")
	model := fs.String("model", "", "override the model from config")
	jsonOut := fs.String("json", "", "write JSON results to this path")
	mdOut := fs.String("md", "", "write the Markdown table to this path (default stdout)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: culprit eval [flags]\n\nRuns the investigator over fixture scenarios with known root causes and scores the reports.\n\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("eval: unexpected arguments %v", fs.Args())
	}
	if *runs < 1 || *conc < 1 {
		return errors.New("eval: -runs and -concurrency must be >= 1")
	}

	cfg := config.Default()
	if *cfgPath != "" {
		c, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		cfg = c
	}
	if *model != "" {
		cfg.LLM.Model = *model
	}
	if cfg.LLM.APIKey() == "" {
		fmt.Fprintf(os.Stderr, "warning: %s is empty; requests to %s may be rejected\n", cfg.LLM.APIKeyEnv, cfg.LLM.BaseURL)
	}

	dirs, err := eval.Discover(*scenarios, strings.Split(*only, ","))
	if err != nil {
		return err
	}
	client := app.LLM(cfg)
	build := func(dir string) (eval.Investigator, *fixture.Scenario, error) {
		inv, sc, err := app.Scenario(cfg, client, dir)
		if err != nil {
			return nil, nil, err
		}
		return inv, sc, nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Fprintf(os.Stderr, "eval: %d scenarios x %d runs, model %s, concurrency %d\n", len(dirs), *runs, cfg.LLM.Model, *conc)
	res, runErr := eval.Run(ctx, eval.Options{
		Scenarios:   dirs,
		Runs:        *runs,
		Concurrency: *conc,
		Build:       build,
		Progress: func(done, total int, r eval.RunResult) {
			verdict := "wrong"
			if r.Correct {
				verdict = "correct"
			}
			line := fmt.Sprintf("[%d/%d] %s run %d: %s, %s, top=%s", done, total, r.Scenario, r.Run, r.Status, verdict, orDash(r.TopService))
			if r.TopChangeID != "" {
				line += " change=" + r.TopChangeID
			}
			line += fmt.Sprintf(" conf=%.2f tokens=%d %.1fs", r.TopConfidence, r.Tokens, r.DurationSeconds)
			if r.Error != "" {
				line += " err=" + r.Error
			}
			fmt.Fprintln(os.Stderr, line)
		},
	})
	if res == nil {
		return runErr
	}

	md := res.Markdown()
	if *mdOut != "" {
		if err := os.WriteFile(*mdOut, []byte(md), 0o644); err != nil {
			return fmt.Errorf("eval: %w", err)
		}
		fmt.Fprintln(os.Stderr, "eval: wrote", *mdOut)
	} else {
		fmt.Print(md)
	}
	if *jsonOut != "" {
		b, err := res.JSON()
		if err != nil {
			return err
		}
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			return fmt.Errorf("eval: %w", err)
		}
		fmt.Fprintln(os.Stderr, "eval: wrote", *jsonOut)
	}
	return runErr
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
