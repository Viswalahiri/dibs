// Package config loads and validates dibs.yaml, repos.yaml, and the required
// environment secrets. Everything outside this package may assume a *Config it
// receives is already valid.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Profile Profile `yaml:"profile"`
	Polling Polling `yaml:"polling"`
	Slack   Slack   `yaml:"slack"`
	Reaper  Reaper  `yaml:"reaper"`
}

type Profile struct {
	GitHubLogin string `yaml:"github_login"`
	Timezone    string `yaml:"timezone"`

	loc *time.Location
}

// Location returns the operator's timezone, which is what decides when the
// reaper's daily pass runs.
func (p Profile) Location() *time.Location { return p.loc }

type Polling struct {
	DefaultIntervalSec int `yaml:"default_interval_sec"`
	MinIntervalSec     int `yaml:"min_interval_sec"`
	MaxConcurrent      int `yaml:"max_concurrent"`
	FreshnessCutoffMin int `yaml:"freshness_cutoff_min"`
	GapWarnMin         int `yaml:"gap_warn_min"`
	RateLimitSlowAt    int `yaml:"rate_limit_slow_at"`
	RateLimitPauseAt   int `yaml:"rate_limit_pause_at"`
}

func (p Polling) DefaultInterval() time.Duration {
	return time.Duration(p.DefaultIntervalSec) * time.Second
}
func (p Polling) MinInterval() time.Duration {
	return time.Duration(p.MinIntervalSec) * time.Second
}

// FreshnessCutoff is the age limit at first sight. An issue older than this
// when dibs first sees it is recorded and dropped before any enrichment
// request.
func (p Polling) FreshnessCutoff() time.Duration {
	return time.Duration(p.FreshnessCutoffMin) * time.Minute
}
func (p Polling) GapWarn() time.Duration {
	return time.Duration(p.GapWarnMin) * time.Minute
}

type Slack struct {
	DeliverTo string `yaml:"deliver_to"`
}

type Reaper struct {
	ExpireAfterDays      int `yaml:"expire_after_days"`
	CadenceRecomputeHour int `yaml:"cadence_recompute_hour"`
	LeaseTTLSec          int `yaml:"lease_ttl_sec"`
}

func (r Reaper) LeaseTTL() time.Duration {
	return time.Duration(r.LeaseTTLSec) * time.Second
}
func (r Reaper) ExpireAfter() time.Duration {
	return time.Duration(r.ExpireAfterDays) * 24 * time.Hour
}

// Repo is one entry from repos.yaml.
type Repo struct {
	Slug  string `yaml:"slug"`
	Notes string `yaml:"notes"`
}

// Owner and Name split the slug. Validation guarantees exactly one slash.
func (r Repo) Owner() string { owner, _, _ := strings.Cut(r.Slug, "/"); return owner }
func (r Repo) Name() string  { _, name, _ := strings.Cut(r.Slug, "/"); return name }

type reposFile struct {
	Repos []Repo `yaml:"repos"`
}

// Secrets holds the tokens. Never log any of these, not even a prefix.
type Secrets struct {
	GitHubToken   string
	SlackBotToken string
}

// Paths resolves the config and database locations from the environment,
// falling back to the XDG-style defaults.
type Paths struct {
	Config string
	Repos  string
	DB     string
}

func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	cfg := os.Getenv("DIBS_CONFIG")
	if cfg == "" {
		cfg = filepath.Join(home, ".config", "dibs", "dibs.yaml")
	}
	db := os.Getenv("DIBS_DB")
	if db == "" {
		db = filepath.Join(home, ".local", "share", "dibs", "dibs.db")
	}
	return Paths{
		Config: cfg,
		Repos:  filepath.Join(filepath.Dir(cfg), "repos.yaml"),
		DB:     db,
	}, nil
}

// Environment variable names. Callers ask for the ones they need, so a
// dry run does not demand a Slack app that has not been created yet.
const (
	EnvGitHubToken   = "DIBS_GITHUB_TOKEN"
	EnvSlackBotToken = "DIBS_SLACK_BOT_TOKEN"
)

// LoadSecrets reads the named tokens and reports every missing one at once, so
// a fresh install does not need a restart per token to find them all.
func LoadSecrets(required ...string) (Secrets, error) {
	values := map[string]string{
		EnvGitHubToken:   os.Getenv(EnvGitHubToken),
		EnvSlackBotToken: os.Getenv(EnvSlackBotToken),
	}
	var missing []string
	for _, name := range required {
		if values[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return Secrets{}, fmt.Errorf("missing required environment variables: %s\n"+
			"put them in ~/.config/dibs/env, then run: set -a; . ~/.config/dibs/env; set +a",
			strings.Join(missing, ", "))
	}
	return Secrets{
		GitHubToken:   values[EnvGitHubToken],
		SlackBotToken: values[EnvSlackBotToken],
	}, nil
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parse(raw)
}

func parse(raw []byte) (*Config, error) {
	cfg := defaults()
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func LoadRepos(path string) ([]Repo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseRepos(raw)
}

func parseRepos(raw []byte) ([]Repo, error) {
	var f reposFile
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse repos: %w", err)
	}
	if len(f.Repos) == 0 {
		return nil, errors.New("repos: no repositories configured")
	}
	seen := make(map[string]bool, len(f.Repos))
	for _, r := range f.Repos {
		owner, name, ok := strings.Cut(r.Slug, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("repos: %q is not a valid owner/name slug", r.Slug)
		}
		key := strings.ToLower(r.Slug)
		if seen[key] {
			return nil, fmt.Errorf("repos: %s listed twice", r.Slug)
		}
		seen[key] = true
	}
	return f.Repos, nil
}

// defaults returns a Config pre-populated with the documented defaults. YAML
// decoding overwrites only the fields the operator actually set, so a sparse
// config file is valid.
func defaults() *Config {
	return &Config{
		Profile: Profile{
			Timezone: "UTC",
		},
		Polling: Polling{
			DefaultIntervalSec: 45,
			MinIntervalSec:     30,
			MaxConcurrent:      4,
			FreshnessCutoffMin: 60,
			GapWarnMin:         15,
			RateLimitSlowAt:    500,
			RateLimitPauseAt:   100,
		},
		Slack: Slack{DeliverTo: "dm"},
		Reaper: Reaper{
			ExpireAfterDays:      14,
			CadenceRecomputeHour: 3,
			LeaseTTLSec:          300,
		},
	}
}

func (c *Config) validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if c.Profile.GitHubLogin == "" {
		add("profile.github_login is required; the filter needs it to tell your " +
			"comments and assignments from everyone else's")
	}
	loc, err := time.LoadLocation(c.Profile.Timezone)
	if err != nil {
		add("profile.timezone %q is not a known timezone", c.Profile.Timezone)
	} else {
		c.Profile.loc = loc
	}
	if c.Polling.MinIntervalSec <= 0 {
		add("polling.min_interval_sec must be positive")
	}
	if c.Polling.DefaultIntervalSec < c.Polling.MinIntervalSec {
		add("polling.default_interval_sec (%d) must be at least polling.min_interval_sec (%d)",
			c.Polling.DefaultIntervalSec, c.Polling.MinIntervalSec)
	}
	if c.Polling.MaxConcurrent <= 0 {
		add("polling.max_concurrent must be positive")
	}
	if c.Polling.FreshnessCutoffMin <= 0 {
		add("polling.freshness_cutoff_min must be positive")
	}
	if c.Polling.GapWarnMin <= 0 {
		add("polling.gap_warn_min must be positive")
	}
	if c.Polling.RateLimitPauseAt >= c.Polling.RateLimitSlowAt {
		add("polling.rate_limit_pause_at (%d) must be below polling.rate_limit_slow_at (%d)",
			c.Polling.RateLimitPauseAt, c.Polling.RateLimitSlowAt)
	}

	if c.Slack.DeliverTo == "" {
		add("slack.deliver_to is required; use \"dm\" or a channel ID")
	}

	if c.Reaper.ExpireAfterDays <= 0 {
		add("reaper.expire_after_days must be positive")
	}
	if c.Reaper.CadenceRecomputeHour < 0 || c.Reaper.CadenceRecomputeHour > 23 {
		add("reaper.cadence_recompute_hour must be between 0 and 23")
	}
	if c.Reaper.LeaseTTLSec <= 0 {
		add("reaper.lease_ttl_sec must be positive")
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}
