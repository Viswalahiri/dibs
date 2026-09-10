package triage

import (
	"math"
	"strings"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
)

// softVetoFloor is where an uncertain veto starts costing points. Below it the
// model is not claiming anything, so nothing is deducted. Between it and the
// configured threshold the model is hedging, and hedging is worth a penalty
// rather than a kill.
const softVetoFloor = 0.35

// softVetoPenalty, labelPenalty, and assigneePenalty are flat deductions. All
// three are deliberately blunt: there are already two numbers to tune here, and
// PLAN's whole argument is that two is a fit that can be done against a hundred
// issues.
//
// assigneePenalty is what an assignee costs now that it is no longer a kill in
// the filter. An assignee is weak evidence of a claim. Kubernetes repositories
// hand them out through automation, and a bot's round-robin is not a person
// writing code. The strong signals, a linked pull request and an explicit claim
// in the thread, still reject outright.
const (
	softVetoPenalty = 10
	labelPenalty    = 10
	assigneePenalty = 10
)

// penaltyLabels cost points rather than a kill. Each describes an issue that
// may still be worth taking, just less often than a plain bug report.
var penaltyLabels = []string{"question", "discussion", "rfc"}

// vetoReasons maps a veto to the reject reason it records.
var vetoReasons = map[string]store.RejectReason{
	"self_fixing":   store.ReasonVetoSelfFixing,
	"already_taken": store.ReasonVetoAlreadyTaken,
	"poorly_scoped": store.ReasonVetoPoorlyScoped,
}

// Composite turns a model response into the final score.
//
// Go computes this, never the model. A model-produced composite drifts between
// calls, which would make the weights untunable and `dibs replay` pointless.
// Everything here is a pure function of its arguments, so replay can sweep the
// config over stored responses without spending anything.
func Composite(r Response, iss store.Issue, repo store.Repo, cfg *config.Config) (
	score int, rejected bool, reason store.RejectReason) {

	for _, v := range r.Vetoes.Each() {
		if v.Veto.Confidence >= cfg.Scoring.VetoConfidence {
			return 0, true, vetoReasons[v.Name]
		}
	}

	w := cfg.Scoring.Weights
	d := r.Dimensions
	base := float64(
		w.ScopeClarity*clampScore(d.ScopeClarity.Score) +
			w.Concreteness*clampScore(d.Concreteness.Score) +
			w.BlastRadius*clampScore(d.BlastRadius.Score) +
			w.MaintainerInvitation*clampScore(d.MaintainerInvitation.Score) +
			w.ContentionRisk*clampScore(d.ContentionRisk.Score))

	// Weights sum to 100 and scores run 1 to 5, so base lands in 100 to 500
	// and dividing by 5 puts the result on a 20 to 100 scale before any
	// multiplier.
	out := base / 5.0

	if stacksIntersect(r.Stack, effectiveStacks(repo, cfg)) {
		out *= cfg.Scoring.Multipliers.StackMatch
	} else {
		out *= cfg.Scoring.Multipliers.StackMismatch
	}

	out *= cfg.Scoring.Multipliers.For(repo.Receptivity)

	for _, v := range r.Vetoes.Each() {
		if v.Veto.Confidence >= softVetoFloor && v.Veto.Confidence < cfg.Scoring.VetoConfidence {
			out -= softVetoPenalty
		}
	}

	for _, label := range iss.Labels {
		lower := strings.ToLower(strings.TrimSpace(label))
		for _, p := range penaltyLabels {
			if lower == p {
				out -= labelPenalty
				break
			}
		}
	}

	if assignedToOther(iss.Assignees, cfg.Profile.GitHubLogin) {
		out -= assigneePenalty
	}

	return clampTo100(out), false, ""
}

// assignedToOther reports whether anyone but the operator holds the assignment.
// His own assignment is not a penalty: it is the outcome this system exists to
// produce.
func assignedToOther(assignees []string, self string) bool {
	for _, a := range assignees {
		if !strings.EqualFold(strings.TrimSpace(a), self) {
			return true
		}
	}
	return false
}

// effectiveStacks is the repository's own list when it has one, since a repo
// override is the operator saying "this project is Go regardless of what else
// I work in".
func effectiveStacks(repo store.Repo, cfg *config.Config) []string {
	if len(repo.Stacks) > 0 {
		return repo.Stacks
	}
	return cfg.Profile.Stacks
}

func stacksIntersect(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[strings.ToLower(strings.TrimSpace(s))] = true
	}
	for _, s := range a {
		if set[strings.ToLower(strings.TrimSpace(s))] {
			return true
		}
	}
	return false
}

// clampScore keeps a malformed dimension from dragging the composite out of
// range. Validate rejects these first; this is the belt to that pair of braces.
func clampScore(n int) int {
	switch {
	case n < 1:
		return 1
	case n > 5:
		return 5
	}
	return n
}

func clampTo100(f float64) int {
	n := int(math.Round(f))
	switch {
	case n < 0:
		return 0
	case n > 100:
		return 100
	}
	return n
}

// Route decides what happens to a scored issue. Below the floor it is stored
// and never surfaced, which is what makes raising the floor a one-line change
// with recorded data behind it.
func Route(score int, cfg *config.Config) (state store.State, reason store.RejectReason) {
	if score < cfg.Scoring.JunkFloor {
		return store.StateRejected, store.ReasonBelowFloor
	}
	return store.StateScored, ""
}
