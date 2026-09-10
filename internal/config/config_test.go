package config

import (
	"strings"
	"testing"
	"time"
)

const minimalConfig = `
profile:
  github_login: test-user
`

func TestSparseConfigTakesDefaults(t *testing.T) {
	cfg, err := parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Polling.FreshnessCutoff(); got != 60*time.Minute {
		t.Errorf("freshness cutoff = %v, want 60m", got)
	}
	if got := cfg.Polling.DefaultInterval(); got != 45*time.Second {
		t.Errorf("default interval = %v, want 45s", got)
	}
	if got := cfg.Reaper.LeaseTTL(); got != 5*time.Minute {
		t.Errorf("lease ttl = %v, want 5m", got)
	}
	if cfg.Slack.DeliverTo != "dm" {
		t.Errorf("deliver_to = %q, want dm", cfg.Slack.DeliverTo)
	}
	if cfg.Profile.Location() == nil {
		t.Error("timezone was not resolved to a *time.Location")
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "github_login is required",
			yaml:    "profile:\n  timezone: UTC\n",
			wantErr: "profile.github_login is required",
		},
		{
			name:    "unknown field is rejected",
			yaml:    minimalConfig + "\npolling:\n  push_tier: 70\n",
			wantErr: "push_tier",
		},
		{
			name:    "bad timezone",
			yaml:    "profile:\n  github_login: x\n  timezone: Mars/Olympus\n",
			wantErr: "is not a known timezone",
		},
		{
			name:    "pause threshold must sit below slow threshold",
			yaml:    minimalConfig + "\npolling:\n  rate_limit_slow_at: 100\n  rate_limit_pause_at: 500\n",
			wantErr: "must be below polling.rate_limit_slow_at",
		},
		{
			name:    "default interval below minimum",
			yaml:    minimalConfig + "\npolling:\n  default_interval_sec: 10\n",
			wantErr: "must be at least polling.min_interval_sec",
		},
		{
			name:    "freshness cutoff must be positive",
			yaml:    minimalConfig + "\npolling:\n  freshness_cutoff_min: 0\n",
			wantErr: "polling.freshness_cutoff_min must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Validation reports every problem in one pass so a fresh install does not
// need one restart per mistake.
func TestValidationReportsAllProblemsAtOnce(t *testing.T) {
	_, err := parse([]byte("polling:\n  max_concurrent: 0\n  freshness_cutoff_min: -1\n"))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{
		"profile.github_login is required",
		"polling.max_concurrent must be positive",
		"polling.freshness_cutoff_min must be positive",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

func TestParseRepos(t *testing.T) {
	repos, err := parseRepos([]byte(`
repos:
  - slug: golang/go
    notes: "responsive"
  - slug: sqlite/sqlite
`))
	if err != nil {
		t.Fatalf("parseRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}
	if repos[0].Owner() != "golang" || repos[0].Name() != "go" {
		t.Errorf("slug split = %q/%q, want golang/go", repos[0].Owner(), repos[0].Name())
	}
	if repos[0].Notes != "responsive" {
		t.Errorf("notes = %q, want them carried through", repos[0].Notes)
	}
}

func TestParseReposRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"empty list", "repos: []\n", "no repositories configured"},
		{"slug without owner", "repos:\n  - slug: go\n", "is not a valid owner/name slug"},
		{"slug with extra path", "repos:\n  - slug: a/b/c\n", "is not a valid owner/name slug"},
		{"unknown field", "repos:\n  - slug: a/b\n    receptivity: eager\n", "receptivity"},
		{"duplicate slug", "repos:\n  - slug: a/b\n  - slug: A/B\n", "listed twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRepos([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
