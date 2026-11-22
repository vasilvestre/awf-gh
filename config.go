package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole declarative surface of awf-gh. Everything else is
// decided by the workflow you point at.
type Config struct {
	AwfURL    string       `yaml:"awf_url"`
	StatePath string       `yaml:"state_path"`
	MaxPerRun int          `yaml:"max_per_run"`
	Repos     []RepoConfig `yaml:"repos"`
}

type RepoConfig struct {
	Repo          string   `yaml:"repo"`
	Workflow      string   `yaml:"workflow"`
	Scope         string   `yaml:"scope"`
	On            []string `yaml:"on"`
	IgnoreAuthors []string `yaml:"ignore_authors"`
	OnlyAuthors   []string `yaml:"only_authors"`
	// Labels turn issue-labeled on for the issues that carry one of them.
	Labels []string `yaml:"labels"`
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "awf-gh", "config.yaml")
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.AwfURL == "" {
		c.AwfURL = "http://127.0.0.1:8080"
	}
	c.StatePath = expandHome(c.StatePath)
	if c.StatePath == "" {
		c.StatePath = filepath.Join(os.Getenv("HOME"), ".local", "state", "awf-gh", "seen.jsonl")
	}
	if c.MaxPerRun == 0 {
		c.MaxPerRun = 3
	}
	for i := range c.Repos {
		if c.Repos[i].Scope == "" {
			c.Repos[i].Scope = "local"
		}
		if len(c.Repos[i].On) == 0 {
			c.Repos[i].On = []string{EventOpened, EventUpdated}
		}
	}
}

func (c *Config) validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("no repos configured")
	}
	if c.MaxPerRun < 1 {
		return fmt.Errorf("max_per_run must be positive")
	}
	for i, rc := range c.Repos {
		if rc.Repo == "" || rc.Workflow == "" {
			return fmt.Errorf("repos[%d]: repo and workflow are required", i)
		}
		for _, ev := range rc.On {
			if !slices.Contains(allEvents, ev) {
				return fmt.Errorf("repos[%d]: unknown event %q (want one of %v)", i, ev, allEvents)
			}
		}
		if slices.Contains(rc.On, EventIssueLabeled) && len(rc.Labels) == 0 {
			// Without a label the event would fire on every open issue.
			return fmt.Errorf("repos[%d]: issue-labeled needs at least one label", i)
		}
	}
	return nil
}

func (c RepoConfig) ignores(login string) bool {
	return slices.Contains(c.IgnoreAuthors, login)
}

// allows keeps the watch to the listed PR authors. An empty list allows all.
func (c RepoConfig) allows(login string) bool {
	return len(c.OnlyAuthors) == 0 || slices.Contains(c.OnlyAuthors, login)
}

// expandHome resolves a leading ~ so the example config can be literal.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(os.Getenv("HOME"), p[1:])
	}
	return p
}
