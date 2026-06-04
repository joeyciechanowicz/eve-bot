// Package bot is the embeddable entry point for an eve-bot instance. A binary
// (this repo's cmd/eve-bot or an external private binary) composes a Config
// and calls Run. Custom sources and actions are wired in by blank-importing
// their packages so their init() functions register with
// github.com/joeyciechanowicz/eve-bot/source and .../action.
package bot

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/joeyciechanowicz/eve-bot/internal/rules"
)

// Config is the full YAML document the bot loads at startup.
type Config struct {
	Store struct {
		Path             string        `yaml:"path"`
		JanitorInterval  time.Duration `yaml:"janitor_interval"`
		ActionHistoryTTL time.Duration `yaml:"action_history_ttl"`
	} `yaml:"store"`

	Retry struct {
		MaxRetries  int           `yaml:"max_retries"`
		BaseBackoff time.Duration `yaml:"base_backoff"`
		MaxBackoff  time.Duration `yaml:"max_backoff"`
	} `yaml:"retry"`

	Debug bool `yaml:"debug"`

	BufferSize int `yaml:"buffer_size"`

	Sources []SourceConfig `yaml:"sources"`
	Rules   RuleSetConfig  `yaml:"rules"`

	Enrich EnrichConfig `yaml:"enrich"`
}

// EnrichConfig configures cross-cutting enrichers that run after source-local
// normalization. All entries are optional; zero-value disables.
type EnrichConfig struct {
	Names NamesEnrichConfig `yaml:"names"`
}

// NamesEnrichConfig configures the ESI name resolver. The enricher is always
// wired in; these fields only override its defaults.
type NamesEnrichConfig struct {
	CacheTTL time.Duration `yaml:"cache_ttl"`
	BaseURL  string        `yaml:"base_url"`
}

// SourceConfig names a source instance and selects its driver. Additional
// driver-specific parameters are decoded from the inline remainder.
type SourceConfig struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	Params map[string]any `yaml:",inline"`
}

// RuleSetConfig mirrors rules.Set but keeps raw YAML rules pre-compile.
type RuleSetConfig struct {
	Mode  rules.Mode   `yaml:"mode"`
	Rules []rules.Rule `yaml:"rules"`
}

// LoadConfig reads and validates the config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) validate() error {
	if c.Store.Path == "" {
		return fmt.Errorf("store.path is required")
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("at least one source is required")
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if s.Name == "" {
			return fmt.Errorf("sources[%d]: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("sources[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		if s.Type == "" {
			return fmt.Errorf("sources[%q]: type is required", s.Name)
		}
	}
	for i, r := range c.Rules.Rules {
		for _, ref := range r.Sources {
			if !seen[ref] {
				return fmt.Errorf("rules[%d] %q: unknown source %q", i, r.Name, ref)
			}
		}
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Store.JanitorInterval <= 0 {
		c.Store.JanitorInterval = 5 * time.Minute
	}
	if c.Store.ActionHistoryTTL <= 0 {
		c.Store.ActionHistoryTTL = 7 * 24 * time.Hour
	}
	if c.Retry.MaxRetries <= 0 {
		c.Retry.MaxRetries = 3
	}
	if c.Retry.BaseBackoff <= 0 {
		c.Retry.BaseBackoff = 250 * time.Millisecond
	}
	if c.Retry.MaxBackoff <= 0 {
		c.Retry.MaxBackoff = 10 * time.Second
	}
	if c.Enrich.Names.CacheTTL <= 0 {
		c.Enrich.Names.CacheTTL = 7 * 24 * time.Hour
	}
}
