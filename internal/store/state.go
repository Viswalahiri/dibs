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
	// screened or surfaced.
	StateBaseline State = "baseline"

	StateNew   State = "new"   // inserted, awaiting the enricher
	StateReady State = "ready" // survived the filter, awaiting the push worker

	// StatePushed is the end of the line. Dibs sends the notification and
	// stops; claiming happens in the browser and dibs never hears about it.
	StatePushed State = "pushed"

	StateRejected State = "rejected" // killed by the filter; see RejectReason
	StateAgedOut  State = "aged_out" // older than the freshness cutoff at first sight

	// StateClaimedBeforePush means the re-check immediately before the Slack
	// send found the issue taken. No notification was sent.
	StateClaimedBeforePush State = "claimed_before_push"

	// StateExpired is a ready issue the push worker never got to, which in
	// practice means Slack was unreachable for long enough that claiming it is
	// no longer realistic.
	StateExpired State = "expired"
)

// RejectReason explains a StateRejected row. Every rejection carries one.
type RejectReason string

const (
	ReasonLinkedPRExists  RejectReason = "linked_pr_exists"
	ReasonKillfileLabel   RejectReason = "killfile_label"
	ReasonClaimedInThread RejectReason = "claimed_in_thread"
	ReasonTooThin         RejectReason = "too_thin"
)

// transitions is the complete set of legal moves. A state whose entry is
// empty is terminal, and most of them are: an issue either becomes a
// notification or it does not.
var transitions = map[State][]State{
	StateBaseline:          {},
	StateNew:               {StateReady, StateRejected},
	StateReady:             {StatePushed, StateClaimedBeforePush, StateExpired},
	StatePushed:            {},
	StateRejected:          {},
	StateAgedOut:           {},
	StateClaimedBeforePush: {},
	StateExpired:           {},
}

// claimable states are the ones a worker leases rows from. Anything else is
// either terminal or waiting on a human.
var claimable = map[State]bool{
	StateNew:   true,
	StateReady: true,
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
