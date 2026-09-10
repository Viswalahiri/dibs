//go:build eval

// Package triage_test's eval run scores the golden fixtures against a live
// model. It is deliberately outside `go test ./...`: a model version change
// would otherwise redden the build for a reason that has nothing to do with
// the code. Run it by hand with `make eval`.
package triage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
	"github.com/Viswalahiri/dibs/internal/triage"
)

const fixtureDir = "../../testdata/triage"

// minFixtures and the veto coverage below are what make the run mean anything.
// Twelve issues is small enough to hand-label in an afternoon and large enough
// that a rubric drift shows up as more than one disagreement.
const minFixtures = 12

// Band is the verdict a human assigned to a fixture. Bands, never exact
// scores: the numbers move whenever the weights are retuned, and a test that
// pins them would have to be rewritten every time calibration improves.
type Band string

const (
	BandVeto Band = "veto" // must be killed outright
	BandLow  Band = "low"  // scored, below the junk floor
	BandMid  Band = "mid"  // pushed, unremarkable
	BandHigh Band = "high" // pushed, clearly worth claiming
)

// highBand is where a fixture has to land to count as high. It matches the
// green badge in the alert rather than any config value, because that is what
// the operator reads.
const highBand = 70

// Fixture is one hand-labelled issue. Everything but the label comes straight
// out of the database: `input` is the `triage_input` column, verbatim, so the
// model sees exactly the bytes it saw in production.
type Fixture struct {
	Name  string `json:"name"`
	Band  Band   `json:"band"`
	Veto  string `json:"veto,omitempty"` // required when band is veto
	Note  string `json:"note"`
	Input string `json:"input"`

	Repo struct {
		Slug        string   `json:"slug"`
		Receptivity string   `json:"receptivity"`
		Stacks      []string `json:"stacks"`
	} `json:"repo"`
	Labels []string `json:"labels"`
}

func TestEval(t *testing.T) {
	key := os.Getenv(config.EnvAnthropicKey)
	if key == "" {
		t.Fatalf("%s is not set; the eval run calls a live model", config.EnvAnthropicKey)
	}
	cfg, err := config.Load("../../configs/dibs.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fixtures := loadFixtures(t)
	client := triage.NewClient(key, cfg)

	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			result, _, err := client.Score(context.Background(), triage.SystemPrompt(cfg), f.Input)
			if err != nil {
				t.Fatalf("scoring failed: %v", err)
			}
			iss := store.Issue{Labels: f.Labels}
			repo := store.Repo{
				Receptivity: config.Receptivity(f.Repo.Receptivity),
				Stacks:      f.Repo.Stacks,
			}
			score, rejected, reason := triage.Composite(result.Response, iss, repo, cfg)

			if got := band(score, rejected, cfg); got != f.Band {
				t.Errorf("landed in %q, hand-labelled %q (score %d, reason %q)\n  %s",
					got, f.Band, score, reason, f.Note)
			}
			if f.Band == BandVeto && f.Veto != "" {
				if want := store.RejectReason("veto_" + f.Veto); reason != want {
					t.Errorf("vetoed for %q, expected %q", reason, want)
				}
			}
		})
	}
}

func band(score int, rejected bool, cfg *config.Config) Band {
	switch {
	case rejected:
		return BandVeto
	case score < cfg.Scoring.JunkFloor:
		return BandLow
	case score < highBand:
		return BandMid
	}
	return BandHigh
}

// loadFixtures reads the golden set and refuses to run against a thin one. An
// eval that passes because there is nothing to disagree with is worse than no
// eval at all, so the shortfall is a failure rather than a skip.
func loadFixtures(t *testing.T) []Fixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}

	var fixtures []Fixture
	vetoes := map[string]bool{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f Fixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if f.Name == "" {
			f.Name = strings.TrimSuffix(filepath.Base(path), ".json")
		}
		switch f.Band {
		case BandVeto, BandLow, BandMid, BandHigh:
		default:
			t.Fatalf("%s: band is %q, want veto, low, mid, or high", path, f.Band)
		}
		if f.Input == "" {
			t.Fatalf("%s: input is empty; copy the triage_input column verbatim", path)
		}
		if f.Band == BandVeto && f.Veto != "" {
			vetoes[f.Veto] = true
		}
		fixtures = append(fixtures, f)
	}

	if len(fixtures) < minFixtures {
		t.Fatalf("%s holds %d fixtures, want at least %d.\n%s",
			fixtureDir, len(fixtures), minFixtures, howToAddFixtures)
	}
	for _, want := range []string{"self_fixing", "already_taken", "poorly_scoped"} {
		if !vetoes[want] {
			t.Errorf("no fixture covers the %s veto", want)
		}
	}
	return fixtures
}

var howToAddFixtures = fmt.Sprintf(
	"Fixtures come from issues dibs has already scored, one JSON file each in %s.\n"+
		"`dibs status --today` lists candidates; the `input` field is that issue's\n"+
		"triage_input column copied verbatim. See the README in that directory.",
	fixtureDir)
