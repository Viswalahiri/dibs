package triage

import (
	"testing"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
)

// baseConfig is the shipped default: equal weights, no multiplier effect at
// normal receptivity with a stack match. Every test below starts here and
// changes one thing, so a number that moves can only have moved for one reason.
func baseConfig() *config.Config {
	return &config.Config{
		Profile: config.Profile{
			GitHubLogin: "test-user",
			Stacks:      []string{"go", "typescript"},
		},
		Scoring: config.Scoring{
			JunkFloor:      40,
			VetoConfidence: 0.60,
			Weights: config.Weights{
				ScopeClarity: 20, Concreteness: 20, BlastRadius: 20,
				MaintainerInvitation: 20, ContentionRisk: 20,
			},
			Multipliers: config.Multipliers{
				StackMatch: 1.00, StackMismatch: 0.70,
				ReceptivityHigh: 1.10, ReceptivityNormal: 1.00, ReceptivityCautious: 0.85,
			},
		},
	}
}

func repo(mut ...func(*store.Repo)) store.Repo {
	r := store.Repo{Owner: "acme", Name: "widget", Receptivity: config.ReceptivityNormal}
	for _, f := range mut {
		f(&r)
	}
	return r
}

// dims builds a response whose five dimensions all score n, with justifications
// long enough to survive the guard against a compliant model.
func dims(n int) Response {
	why := "cited the reproduction steps in the issue body"
	d := Dimension{Score: n, Why: why}
	return Response{
		Dimensions: Dimensions{
			ScopeClarity: d, Concreteness: d, BlastRadius: d,
			MaintainerInvitation: d, ContentionRisk: d,
		},
		Stack:     []string{"go"},
		Positives: []string{"repro included", "maintainer sketched the fix"},
		TopRisk:   "popular repo",
	}
}

func score(t *testing.T, r Response, iss store.Issue, rp store.Repo, cfg *config.Config) int {
	t.Helper()
	got, rejected, reason := Composite(r, iss, rp, cfg)
	if rejected {
		t.Fatalf("unexpected rejection: %s", reason)
	}
	return got
}

// The scale is the load-bearing claim: all ones is 20, all fives is 100.
// Everything else in this file is a deviation from these two numbers.
func TestCompositeScale(t *testing.T) {
	tests := []struct {
		dimension int
		want      int
	}{{1, 20}, {2, 40}, {3, 60}, {4, 80}, {5, 100}}
	for _, tt := range tests {
		got := score(t, dims(tt.dimension), store.Issue{}, repo(), baseConfig())
		if got != tt.want {
			t.Errorf("all dimensions at %d scored %d, want %d", tt.dimension, got, tt.want)
		}
	}
}

func TestHardVeto(t *testing.T) {
	tests := []struct {
		name   string
		set    func(*Vetoes)
		reason store.RejectReason
	}{
		{"self fixing", func(v *Vetoes) { v.SelfFixing.Confidence = 0.9 }, store.ReasonVetoSelfFixing},
		{"already taken", func(v *Vetoes) { v.AlreadyTaken.Confidence = 0.9 }, store.ReasonVetoAlreadyTaken},
		{"poorly scoped", func(v *Vetoes) { v.PoorlyScoped.Confidence = 0.9 }, store.ReasonVetoPoorlyScoped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := dims(5)
			tt.set(&r.Vetoes)
			got, rejected, reason := Composite(r, store.Issue{}, repo(), baseConfig())
			if !rejected || reason != tt.reason {
				t.Fatalf("got (%d, %v, %s), want a %s rejection", got, rejected, reason, tt.reason)
			}
			if got != 0 {
				t.Errorf("a vetoed issue scored %d, want 0", got)
			}
		})
	}
}

// The threshold is inclusive, and one hundredth below it must not kill the
// issue. This is one of the two numbers M4 tunes, so the boundary has to be
// exact or the calibration means nothing.
func TestVetoThresholdBoundary(t *testing.T) {
	cfg := baseConfig()
	tests := []struct {
		confidence float64
		wantKill   bool
	}{
		{0.59, false},
		{0.60, true},
		{0.61, true},
	}
	for _, tt := range tests {
		r := dims(5)
		r.Vetoes.AlreadyTaken.Confidence = tt.confidence
		_, rejected, _ := Composite(r, store.Issue{}, repo(), cfg)
		if rejected != tt.wantKill {
			t.Errorf("confidence %v: rejected = %v, want %v", tt.confidence, rejected, tt.wantKill)
		}
	}
}

func TestSoftVetoPenalty(t *testing.T) {
	tests := []struct {
		name       string
		confidence float64
		want       int
	}{
		{"below the soft floor costs nothing", 0.34, 100},
		{"at the soft floor costs ten", 0.35, 90},
		{"hedging costs ten", 0.50, 90},
		{"just below the kill line still costs ten", 0.59, 90},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := dims(5)
			r.Vetoes.PoorlyScoped.Confidence = tt.confidence
			if got := score(t, r, store.Issue{}, repo(), baseConfig()); got != tt.want {
				t.Errorf("confidence %v scored %d, want %d", tt.confidence, got, tt.want)
			}
		})
	}
}

func TestSoftVetoPenaltiesStack(t *testing.T) {
	r := dims(5)
	r.Vetoes.SelfFixing.Confidence = 0.4
	r.Vetoes.AlreadyTaken.Confidence = 0.4
	r.Vetoes.PoorlyScoped.Confidence = 0.4
	if got := score(t, r, store.Issue{}, repo(), baseConfig()); got != 70 {
		t.Fatalf("three hedged vetoes scored %d, want 70", got)
	}
}

func TestStackMultiplier(t *testing.T) {
	tests := []struct {
		name  string
		stack []string
		repo  store.Repo
		want  int
	}{
		{"matches the profile", []string{"go"}, repo(), 100},
		{"matches on a second entry", []string{"rust", "typescript"}, repo(), 100},
		{"matches case-insensitively", []string{"Go"}, repo(), 100},
		{"misses the profile entirely", []string{"rust"}, repo(), 70},
		{"an empty stack is a miss", nil, repo(), 70},
		{
			name:  "the repo override wins over the profile",
			stack: []string{"python"},
			repo:  repo(func(r *store.Repo) { r.Stacks = []string{"python"} }),
			want:  100,
		},
		{
			name:  "the repo override can also exclude",
			stack: []string{"go"},
			repo:  repo(func(r *store.Repo) { r.Stacks = []string{"python"} }),
			want:  70,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := dims(5)
			r.Stack = tt.stack
			if got := score(t, r, store.Issue{}, tt.repo, baseConfig()); got != tt.want {
				t.Errorf("stack %v scored %d, want %d", tt.stack, got, tt.want)
			}
		})
	}
}

func TestReceptivityMultiplier(t *testing.T) {
	// Dimensions at 4 give a base of 80, so a 1.10 multiplier has room to show
	// rather than being clipped at the ceiling.
	tests := []struct {
		receptivity config.Receptivity
		want        int
	}{
		{config.ReceptivityHigh, 88},
		{config.ReceptivityNormal, 80},
		{config.ReceptivityCautious, 68},
	}
	for _, tt := range tests {
		t.Run(string(tt.receptivity), func(t *testing.T) {
			rp := repo(func(r *store.Repo) { r.Receptivity = tt.receptivity })
			if got := score(t, dims(4), store.Issue{}, rp, baseConfig()); got != tt.want {
				t.Errorf("%s scored %d, want %d", tt.receptivity, got, tt.want)
			}
		})
	}
}

func TestLabelPenalty(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   int
	}{
		{"no labels", nil, 100},
		{"question", []string{"question"}, 90},
		{"discussion", []string{"discussion"}, 90},
		{"rfc", []string{"rfc"}, 90},
		{"two of them", []string{"question", "rfc"}, 80},
		{"case and spacing do not matter", []string{" Question "}, 90},
		{"an ordinary label costs nothing", []string{"bug", "help wanted"}, 100},
		// A substring is not a match here, unlike the killfile. "questionable"
		// is not the same label as "question".
		{"a label that merely contains one costs nothing", []string{"questionable"}, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iss := store.Issue{Labels: tt.labels}
			if got := score(t, dims(5), iss, repo(), baseConfig()); got != tt.want {
				t.Errorf("labels %v scored %d, want %d", tt.labels, got, tt.want)
			}
		})
	}
}

// Multipliers compound before the flat penalties are taken off, so the order in
// the specification is observable in the result.
func TestMultipliersApplyBeforePenalties(t *testing.T) {
	r := dims(4)
	r.Stack = []string{"rust"}
	r.Vetoes.SelfFixing.Confidence = 0.4
	rp := repo(func(x *store.Repo) { x.Receptivity = config.ReceptivityCautious })
	iss := store.Issue{Labels: []string{"question"}}

	// 80 base, times 0.70 stack miss, times 0.85 cautious, is 47.6.
	// Then one soft veto and one label penalty take 20 off, leaving 27.6.
	if got := score(t, r, iss, rp, baseConfig()); got != 28 {
		t.Fatalf("scored %d, want 28", got)
	}
}

func TestClamping(t *testing.T) {
	cfg := baseConfig()

	// Penalties cannot push a score below zero.
	r := dims(1)
	r.Vetoes.SelfFixing.Confidence = 0.4
	r.Vetoes.AlreadyTaken.Confidence = 0.4
	r.Stack = []string{"rust"}
	iss := store.Issue{Labels: []string{"question", "discussion", "rfc"}}
	if got := score(t, r, iss, repo(), cfg); got != 0 {
		t.Errorf("a floor-scraping issue scored %d, want 0", got)
	}

	// A high-receptivity repository cannot push a perfect issue past 100.
	rp := repo(func(x *store.Repo) { x.Receptivity = config.ReceptivityHigh })
	if got := score(t, dims(5), store.Issue{}, rp, cfg); got != 100 {
		t.Errorf("a perfect issue at high receptivity scored %d, want 100", got)
	}
}

// A dimension outside 1 to 5 is a malformed response, not a reason to emit a
// nonsense composite. Validate rejects these first; this proves the scorer is
// safe even if one slips through.
func TestOutOfRangeDimensionsAreClamped(t *testing.T) {
	r := dims(3)
	r.Dimensions.ScopeClarity.Score = 99
	r.Dimensions.Concreteness.Score = -4
	got := score(t, r, store.Issue{}, repo(), baseConfig())
	// 5, 1, 3, 3, 3 at equal weights is 300, which is 60.
	if got != 60 {
		t.Fatalf("scored %d, want 60", got)
	}
}

func TestRoute(t *testing.T) {
	cfg := baseConfig()
	tests := []struct {
		score  int
		state  store.State
		reason store.RejectReason
	}{
		{39, store.StateRejected, store.ReasonBelowFloor},
		{40, store.StateScored, ""},
		{100, store.StateScored, ""},
		{0, store.StateRejected, store.ReasonBelowFloor},
	}
	for _, tt := range tests {
		state, reason := Route(tt.score, cfg)
		if state != tt.state || reason != tt.reason {
			t.Errorf("score %d routed to (%s, %s), want (%s, %s)",
				tt.score, state, reason, tt.state, tt.reason)
		}
	}
}

// Weights are the lever M4 has not pulled yet, so an unequal set has to work
// the first time it is tried.
func TestUnequalWeights(t *testing.T) {
	cfg := baseConfig()
	cfg.Scoring.Weights = config.Weights{
		ScopeClarity: 40, Concreteness: 30, BlastRadius: 10,
		MaintainerInvitation: 10, ContentionRisk: 10,
	}
	r := dims(3)
	r.Dimensions.ScopeClarity.Score = 5
	// 40*5 + 30*3 + 10*3 + 10*3 + 10*3 = 380, which is 76.
	if got := score(t, r, store.Issue{}, repo(), cfg); got != 76 {
		t.Fatalf("scored %d, want 76", got)
	}
}
