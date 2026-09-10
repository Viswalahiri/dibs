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

// Receptivity records how open a repository's maintainers are to outside
// contributions. It is operator judgment, not something dibs computes.
type Receptivity string

const (
	ReceptivityHigh     Receptivity = "high"
	ReceptivityNormal   Receptivity = "normal"
	ReceptivityCautious Receptivity = "cautious"
)

func (r Receptivity) valid() bool {
	switch r {
	case ReceptivityHigh, ReceptivityNormal, ReceptivityCautious:
		return true
	}
	return false
}

type Config struct {
	Profile Profile `yaml:"profile"`
	Scoring Scoring `yaml:"scoring"`
	Polling Polling `yaml:"polling"`
	Triage  Triage  `yaml:"triage"`
	Slack   Slack   `yaml:"slack"`
	Reaper  Reaper  `yaml:"reaper"`
}

type Profile struct {
	GitHubLogin        string   `yaml:"github_login"`
	Stacks             []string `yaml:"stacks"`
	EffortCeilingHours float64  `yaml:"effort_ceiling_hours"`
	Timezone           string   `yaml:"timezone"`

	loc *time.Location
}

// Location returns the operator's timezone, used for the daily call-cap reset.
func (p Profile) Location() *time.Location { return p.loc }

// Scoring is the tunable half of the system. Both numbers ship wide open: the
// floor pushes everything and the veto threshold kills nothing, so a week of
// running produces a corpus with a real Track and Skip on each row. `dibs
// replay` sweeps candidate values over that corpus, and the numbers that come
// out of it are the ones worth committing.
type Scoring struct {
	// JunkFloor is the score at or above which an issue reaches Slack. Zero
	// pushes everything.
	JunkFloor int `yaml:"junk_floor"`

	// VetoConfidence is the confidence a veto must exceed to kill an issue
	// outright. The comparison is exclusive, so 1.00 means no veto ever kills
	// and every one of them lands as a flat penalty instead.
	VetoConfidence float64 `yaml:"veto_confidence"`

	Weights     Weights     `yaml:"weights"`
	Multipliers Multipliers `yaml:"multipliers"`
}

type Weights struct {
	ScopeClarity         int `yaml:"scope_clarity"`
	Concreteness         int `yaml:"concreteness"`
	BlastRadius          int `yaml:"blast_radius"`
	MaintainerInvitation int `yaml:"maintainer_invitation"`
	ContentionRisk       int `yaml:"contention_risk"`
}

func (w Weights) Sum() int {
	return w.ScopeClarity + w.Concreteness + w.BlastRadius +
		w.MaintainerInvitation + w.ContentionRisk
}

type Multipliers struct {
	StackMatch          float64 `yaml:"stack_match"`
	StackMismatch       float64 `yaml:"stack_mismatch"`
	ReceptivityHigh     float64 `yaml:"receptivity_high"`
	ReceptivityNormal   float64 `yaml:"receptivity_normal"`
	ReceptivityCautious float64 `yaml:"receptivity_cautious"`
}

// For returns the multiplier for a repository's receptivity. Validation
// guarantees r is one of the three known values.
func (m Multipliers) For(r Receptivity) float64 {
	switch r {
	case ReceptivityHigh:
		return m.ReceptivityHigh
	case ReceptivityCautious:
		return m.ReceptivityCautious
	default:
		return m.ReceptivityNormal
	}
}

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
// request or model call.
func (p Polling) FreshnessCutoff() time.Duration {
	return time.Duration(p.FreshnessCutoffMin) * time.Minute
}
func (p Polling) GapWarn() time.Duration {
	return time.Duration(p.GapWarnMin) * time.Minute
}

type Triage struct {
	Model string `yaml:"model"`

	// Thinking is "disabled" or "adaptive". Scoring against a fixed rubric with
	// short justifications does not need reasoning tokens, and dibs is a system
	// whose whole argument is latency, so the default is off. Flip it during
	// calibration and compare with `dibs replay`.
	Thinking string `yaml:"thinking"`

	MaxBodyChars   int  `yaml:"max_body_chars"`
	MaxThreadChars int  `yaml:"max_thread_chars"`
	MaxDocChars    int  `yaml:"max_doc_chars"`
	MaxRetries     int  `yaml:"max_retries"`
	TimeoutSec     int  `yaml:"timeout_sec"`
	DailyCallCap   int  `yaml:"daily_call_cap"`
	Cost           Cost `yaml:"cost"`
}

func (t Triage) Timeout() time.Duration {
	return time.Duration(t.TimeoutSec) * time.Second
}

type Cost struct {
	InputPerMTokUSD  float64 `yaml:"input_per_mtok_usd"`
	OutputPerMTokUSD float64 `yaml:"output_per_mtok_usd"`
	MonthlyBudgetUSD float64 `yaml:"monthly_budget_usd"`
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
	Slug        string      `yaml:"slug"`
	Receptivity Receptivity `yaml:"receptivity"`
	Stacks      []string    `yaml:"stacks"`
	Notes       string      `yaml:"notes"`
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
	AnthropicKey  string
	SlackBotToken string
	SlackAppToken string
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
	EnvAnthropicKey  = "DIBS_ANTHROPIC_KEY"
	EnvSlackBotToken = "DIBS_SLACK_BOT_TOKEN"
	EnvSlackAppToken = "DIBS_SLACK_APP_TOKEN"
)

// LoadSecrets reads the named tokens and reports every missing one at once, so
// a fresh install does not need four restarts to find them all.
func LoadSecrets(required ...string) (Secrets, error) {
	values := map[string]string{
		EnvGitHubToken:   os.Getenv(EnvGitHubToken),
		EnvAnthropicKey:  os.Getenv(EnvAnthropicKey),
		EnvSlackBotToken: os.Getenv(EnvSlackBotToken),
		EnvSlackAppToken: os.Getenv(EnvSlackAppToken),
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
		AnthropicKey:  values[EnvAnthropicKey],
		SlackBotToken: values[EnvSlackBotToken],
		SlackAppToken: values[EnvSlackAppToken],
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
	for i := range f.Repos {
		r := &f.Repos[i]
		if r.Receptivity == "" {
			r.Receptivity = ReceptivityNormal
		}
		owner, name, ok := strings.Cut(r.Slug, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("repos: %q is not a valid owner/name slug", r.Slug)
		}
		if !r.Receptivity.valid() {
			return nil, fmt.Errorf("repos: %s has unknown receptivity %q, want high, normal, or cautious",
				r.Slug, r.Receptivity)
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
			EffortCeilingHours: 16,
			Timezone:           "UTC",
		},
		Scoring: Scoring{
			JunkFloor:      0,
			VetoConfidence: 1.00,
			Weights: Weights{
				ScopeClarity: 20, Concreteness: 20, BlastRadius: 20,
				MaintainerInvitation: 20, ContentionRisk: 20,
			},
			Multipliers: Multipliers{
				StackMatch: 1.00, StackMismatch: 0.70,
				ReceptivityHigh: 1.10, ReceptivityNormal: 1.00,
				ReceptivityCautious: 0.85,
			},
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
		Triage: Triage{
			Model:          "claude-sonnet-5",
			Thinking:       "disabled",
			MaxBodyChars:   4000,
			MaxThreadChars: 2500,
			MaxDocChars:    1500,
			MaxRetries:     2,
			TimeoutSec:     45,
			DailyCallCap:   400,
			Cost: Cost{
				InputPerMTokUSD:  2.00,
				OutputPerMTokUSD: 10.00,
				MonthlyBudgetUSD: 25.00,
			},
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
		add("profile.github_login is required; the filter and reaper both need it " +
			"to tell your comments and assignments from everyone else's")
	}
	loc, err := time.LoadLocation(c.Profile.Timezone)
	if err != nil {
		add("profile.timezone %q is not a known timezone", c.Profile.Timezone)
	} else {
		c.Profile.loc = loc
	}
	if c.Profile.EffortCeilingHours <= 0 {
		add("profile.effort_ceiling_hours must be positive")
	}

	if sum := c.Scoring.Weights.Sum(); sum != 100 {
		add("scoring.weights must sum to 100, got %d", sum)
	}
	if c.Scoring.JunkFloor < 0 || c.Scoring.JunkFloor > 100 {
		add("scoring.junk_floor must be between 0 and 100, got %d", c.Scoring.JunkFloor)
	}
	if c.Scoring.VetoConfidence <= 0 || c.Scoring.VetoConfidence > 1 {
		add("scoring.veto_confidence must be between 0 and 1, got %v", c.Scoring.VetoConfidence)
	}
	for name, m := range map[string]float64{
		"stack_match":          c.Scoring.Multipliers.StackMatch,
		"stack_mismatch":       c.Scoring.Multipliers.StackMismatch,
		"receptivity_high":     c.Scoring.Multipliers.ReceptivityHigh,
		"receptivity_normal":   c.Scoring.Multipliers.ReceptivityNormal,
		"receptivity_cautious": c.Scoring.Multipliers.ReceptivityCautious,
	} {
		if m <= 0 {
			add("scoring.multipliers.%s must be positive, got %v", name, m)
		}
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

	if c.Triage.Model == "" {
		add("triage.model is required")
	}
	if c.Triage.Thinking != "disabled" && c.Triage.Thinking != "adaptive" {
		add("triage.thinking must be \"disabled\" or \"adaptive\", got %q", c.Triage.Thinking)
	}
	for name, v := range map[string]int{
		"max_body_chars":   c.Triage.MaxBodyChars,
		"max_thread_chars": c.Triage.MaxThreadChars,
		"max_doc_chars":    c.Triage.MaxDocChars,
		"timeout_sec":      c.Triage.TimeoutSec,
		"daily_call_cap":   c.Triage.DailyCallCap,
	} {
		if v <= 0 {
			add("triage.%s must be positive, got %d", name, v)
		}
	}
	if c.Triage.MaxRetries < 0 {
		add("triage.max_retries cannot be negative")
	}
	if c.Triage.Cost.MonthlyBudgetUSD <= 0 {
		add("triage.cost.monthly_budget_usd must be positive")
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
