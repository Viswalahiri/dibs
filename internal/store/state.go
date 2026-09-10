package store

import "fmt"

// State is where an issue sits in the pipeline. The table below is the whole
// lifecycle; there are no other flags. A worker claims rows in one state,
// works, and advances them to the next in the same transaction that commits
// its result.
type State string

const (
	// StateBaseline was already open when the repository was adopted. It is
	// recorded so the waterline has something to compare against, and is never
	// enriched, scored, or surfaced.
	StateBaseline State = "baseline"

	StateNew      State = "new"      // inserted, awaiting enrichment
	StateEnriched State = "enriched" // context fetched, awaiting filter and triage
	StateScored   State = "scored"   // above the junk floor, awaiting the push worker
	StatePushed   State = "pushed"   // individual Slack alert sent
	StateTracked  State = "tracked"  // operator pressed Track
	StateSkipped  State = "skipped"  // operator pressed Skip
	StateSnoozed  State = "snoozed"  // operator pressed Snooze, will resurface

	StateRejected State = "rejected" // filter, veto, or junk floor; see RejectReason
	StateAgedOut  State = "aged_out" // older than the freshness cutoff at first sight

	// StateClaimedBeforePush means the re-check immediately before the Slack
	// send found the issue taken. No notification was sent.
	StateClaimedBeforePush State = "claimed_before_push"

	StateExpired State = "expired" // aged out of scored or snoozed

	// StateBackfilled came from `dibs backfill` rather than the poller. It is
	// scored for calibration and never surfaced, which is why it is terminal
	// and why no worker claims it. A backfilled row still carries the
	// RejectReason the filter or the floor would have given it, so the corpus
	// records what would have happened as well as the score.
	StateBackfilled State = "backfilled"
)

// RejectReason explains a StateRejected row. Every rejection carries one.
type RejectReason string

const (
	ReasonAlreadyAssigned  RejectReason = "already_assigned"
	ReasonLinkedPRExists   RejectReason = "linked_pr_exists"
	ReasonKillfileLabel    RejectReason = "killfile_label"
	ReasonClaimedInThread  RejectReason = "claimed_in_thread"
	ReasonTooThin          RejectReason = "too_thin"
	ReasonVetoSelfFixing   RejectReason = "veto_self_fixing"
	ReasonVetoAlreadyTaken RejectReason = "veto_already_taken"
	ReasonVetoPoorlyScoped RejectReason = "veto_poorly_scoped"
	ReasonBelowFloor       RejectReason = "below_floor"
)

// transitions is the complete set of legal moves. A state whose entry is empty
// is terminal. Snoozed returns to scored rather than straight to pushed so the
// resurfaced issue goes back through the push worker's freshness re-check
// instead of needing a second copy of it.
var transitions = map[State][]State{
	StateBaseline:          {},
	StateNew:               {StateEnriched, StateRejected},
	StateEnriched:          {StateScored, StateRejected},
	StateScored:            {StatePushed, StateClaimedBeforePush, StateExpired},
	StatePushed:            {StateTracked, StateSkipped, StateSnoozed, StateExpired},
	StateSnoozed:           {StateScored, StateExpired},
	StateTracked:           {},
	StateSkipped:           {},
	StateRejected:          {},
	StateAgedOut:           {},
	StateClaimedBeforePush: {},
	StateExpired:           {},
	StateBackfilled:        {},
}

// claimable states are the ones a worker leases rows from. Anything else is
// either terminal or waiting on a human.
var claimable = map[State]bool{
	StateNew:      true,
	StateEnriched: true,
	StateScored:   true,
}

func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

func (s State) Claimable() bool { return claimable[s] }

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to State) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

func checkTransition(from, to State) error {
	if !from.Valid() {
		return fmt.Errorf("unknown state %q", from)
	}
	if !to.Valid() {
		return fmt.Errorf("unknown state %q", to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("illegal transition %s -> %s", from, to)
	}
	return nil
}
