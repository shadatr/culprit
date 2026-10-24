// Package app wires configuration into a ready Investigator.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/shadatr/culprit/internal/agent"
	"github.com/shadatr/culprit/internal/config"
	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/llm"
	"github.com/shadatr/culprit/internal/sources/changesfile"
	"github.com/shadatr/culprit/internal/sources/fixture"
	"github.com/shadatr/culprit/internal/sources/gitlog"
	"github.com/shadatr/culprit/internal/sources/loki"
	"github.com/shadatr/culprit/internal/sources/prometheus"
)

// LLM builds the model client from config.
func LLM(c *config.Config) llm.Client {
	return llm.NewOpenAI(c.LLM.BaseURL, c.LLM.APIKey(), &http.Client{Timeout: 120 * time.Second})
}

// AgentConfig maps config to agent.Config.
func AgentConfig(c *config.Config) agent.Config {
	return agent.Config{
		Model:         c.LLM.Model,
		MaxSteps:      c.Budgets.MaxSteps,
		MaxDuration:   c.Budgets.MaxDuration,
		MaxTokens:     c.Budgets.MaxTokens,
		TriageMetrics: c.Sources.Prometheus.TriageMetrics,
		Temperature:   c.LLM.Temperature,
	}
}

// Live builds an investigator over the live sources in c.
func Live(c *config.Config, client llm.Client) (*agent.Investigator, error) {
	deps, err := LiveDeps(c, client)
	if err != nil {
		return nil, err
	}
	return agent.New(deps, AgentConfig(c)), nil
}

// LiveDeps builds the live sources in c.
func LiveDeps(c *config.Config, client llm.Client) (agent.Deps, error) {
	deps := agent.Deps{LLM: client, Topology: c.Topology}
	s := c.Sources
	if s.Prometheus.URL != "" {
		deps.Metrics = prometheus.New(s.Prometheus.URL, nil)
	}
	if s.Loki.URL != "" {
		deps.Logs = loki.New(s.Loki.URL, s.Loki.ServiceLabel, nil)
	}
	var cs []core.ChangesSource
	if len(s.Git.Repos) > 0 {
		cs = append(cs, gitlog.New(s.Git.Repos))
	}
	if s.ChangesFile != "" {
		cs = append(cs, changesfile.New(s.ChangesFile))
	}
	if len(cs) > 0 {
		deps.Changes = MergeChanges(cs...)
	}
	if deps.Metrics == nil && deps.Logs == nil && deps.Changes == nil {
		return deps, errors.New("app: no data sources configured")
	}
	return deps, nil
}

// Scenario builds an investigator over a fixture scenario directory.
func Scenario(c *config.Config, client llm.Client, dir string) (*agent.Investigator, *fixture.Scenario, error) {
	sc, err := fixture.Load(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("app: %w", err)
	}
	deps := agent.Deps{
		LLM:      client,
		Metrics:  sc.Metrics(),
		Logs:     sc.Logs(),
		Changes:  sc.Changes(),
		Topology: sc.Topology,
	}
	return agent.New(deps, AgentConfig(c)), sc, nil
}

// MergeChanges combines change sources; a failing source is skipped if at
// least one other succeeds.
func MergeChanges(srcs ...core.ChangesSource) core.ChangesSource {
	if len(srcs) == 1 {
		return srcs[0]
	}
	return multiChanges(srcs)
}

type multiChanges []core.ChangesSource

func (m multiChanges) Name() string { return "changes" }

func (m multiChanges) Changes(ctx context.Context, w core.Window) ([]core.Change, error) {
	var all []core.Change
	var errs []error
	for _, s := range m {
		cs, err := s.Changes(ctx, w)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
			continue
		}
		all = append(all, cs...)
	}
	if len(errs) == len(m) {
		return nil, errors.Join(errs...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].T.After(all[j].T) })
	return all, nil
}
