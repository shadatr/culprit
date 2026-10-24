// Package config loads Culprit's YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/shadatr/culprit/internal/core"
	"github.com/shadatr/culprit/internal/sources/gitlog"
)

// Config is the whole configuration file.
type Config struct {
	LLM      LLM           `yaml:"llm"`
	Sources  Sources       `yaml:"sources"`
	Topology core.Topology `yaml:"topology"`
	Budgets  Budgets       `yaml:"budgets"`
	Server   Server        `yaml:"server"`
}

// LLM selects the model endpoint.
type LLM struct {
	BaseURL     string  `yaml:"base_url"`    // OpenAI-compatible, e.g. https://api.openai.com/v1
	APIKeyEnv   string  `yaml:"api_key_env"` // env var holding the key; never put keys in the file
	Model       string  `yaml:"model"`
	Temperature float64 `yaml:"temperature"`
}

// APIKey reads the key from the configured env var.
func (l LLM) APIKey() string {
	if l.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(l.APIKeyEnv)
}

// Sources configures data sources. Any may be empty.
type Sources struct {
	Prometheus struct {
		URL           string   `yaml:"url"`
		TriageMetrics []string `yaml:"triage_metrics"`
	} `yaml:"prometheus"`
	Loki struct {
		URL          string `yaml:"url"`
		ServiceLabel string `yaml:"service_label"`
	} `yaml:"loki"`
	Git struct {
		Repos []gitlog.Repo `yaml:"repos"`
	} `yaml:"git"`
	ChangesFile string `yaml:"changes_file"`
}

// Budgets cap one investigation.
type Budgets struct {
	MaxSteps    int           `yaml:"max_steps"`
	MaxDuration time.Duration `yaml:"max_duration"`
	MaxTokens   int           `yaml:"max_tokens"`
}

// Server configures `culprit serve`.
type Server struct {
	Addr        string        `yaml:"addr"`        // default :8090
	DBPath      string        `yaml:"db_path"`     // default culprit.db
	CORSOrigin  string        `yaml:"cors_origin"` // default http://localhost:3200
	Concurrency int           `yaml:"concurrency"` // parallel investigations, default 2
	Lookback    time.Duration `yaml:"lookback"`    // window for webhook incidents, default 1h
}

// Load reads and validates path, applying defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	c.applyDefaults()
	return &c, nil
}

// Default returns a config with only defaults set.
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.LLM.BaseURL == "" {
		c.LLM.BaseURL = "https://api.openai.com/v1"
	}
	if c.LLM.APIKeyEnv == "" {
		c.LLM.APIKeyEnv = "CULPRIT_LLM_API_KEY"
	}
	if c.LLM.Model == "" {
		c.LLM.Model = "gpt-4.1-mini"
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8090"
	}
	if c.Server.DBPath == "" {
		c.Server.DBPath = "culprit.db"
	}
	if c.Server.CORSOrigin == "" {
		c.Server.CORSOrigin = "http://localhost:3200"
	}
	if c.Server.Concurrency == 0 {
		c.Server.Concurrency = 2
	}
	if c.Server.Lookback == 0 {
		c.Server.Lookback = time.Hour
	}
}
