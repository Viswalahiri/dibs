package triage

import (
	"context"
	"fmt"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
)

// Row is one issue re-scored under a candidate config, next to what actually
// happened to it.
type Row struct {
	Repo        string
	Number      int
	Title       string
	URL         string
	StoredScore int
	NewScore    int
	StoredPush  bool
	NewPush     bool
	NewReason   store.RejectReason
	Decision    store.State
}

// Changed reports whether the candidate config would have treated this issue
// differently.
func (r Row) Changed() bool { return r.StoredPush != r.NewPush }

// Summary is the number that decides whether a config change is an
// improvement.
//
// The two counts are not symmetric, and neither is their cost. A veto or a
// floor that kills something already tracked is the only failure that matters:
// it costs the issue. A pushed issue that was skipped costs one click. So a
// candidate config that trades several of the second for one of the first is a
// bad trade, no matter how much quieter it looks.
type Summary struct {
	Total         int
	WouldPush     int
	WouldReject   int
	NewlyPushed   int
	NewlyRejected int
	TrackedLost   int // tracked issues the candidate would have killed
	SkippedSpared int // skipped issues the candidate would have suppressed
	DegradedSkip  int // rows scored by the fallback, excluded from the counts
}

// Replay re-scores every stored model response under cfg. It reads only the
// database, so a config sweep costs nothing and can be run as often as it is
// useful.
func Replay(ctx context.Context, s *store.Store, cfg *config.Config) ([]Row, Summary, error) {
	issues, err := s.Triaged(ctx)
	if err != nil {
		return nil, Summary{}, err
	}

	repos := map[int64]store.Repo{}
	var rows []Row
	var sum Summary

	for _, iss := range issues {
		response, err := Decode([]byte(iss.TriageJSON))
		if err != nil {
			// A degraded row holds a failure marker, not a verdict. Counting it
			// as evidence would let an outage look like calibration data.
			sum.DegradedSkip++
			continue
		}
		repo, ok := repos[iss.RepoID]
		if !ok {
			if repo, err = s.RepoByID(ctx, iss.RepoID); err != nil {
				return nil, Summary{}, err
			}
			repos[iss.RepoID] = repo
		}

		score, rejected, reason := Composite(response, iss, repo, cfg)
		state := store.StateRejected
		if !rejected {
			state, reason = Route(score, cfg)
		}

		row := Row{
			Repo:        repo.Slug(),
			Number:      iss.Number,
			Title:       iss.Title,
			URL:         iss.HTMLURL,
			StoredScore: int(iss.Score.Int64),
			NewScore:    score,
			StoredPush:  wasPushed(iss.State),
			NewPush:     state == store.StateScored,
			NewReason:   reason,
			Decision:    iss.State,
		}
		rows = append(rows, row)

		sum.Total++
		if row.NewPush {
			sum.WouldPush++
		} else {
			sum.WouldReject++
		}
		switch {
		case row.NewPush && !row.StoredPush:
			sum.NewlyPushed++
		case !row.NewPush && row.StoredPush:
			sum.NewlyRejected++
		}
		if !row.NewPush && iss.State == store.StateTracked {
			sum.TrackedLost++
		}
		if !row.NewPush && iss.State == store.StateSkipped {
			sum.SkippedSpared++
		}
	}
	return rows, sum, nil
}

// wasPushed reports whether the issue actually reached Slack. Everything past
// `pushed` got there by being pushed.
func wasPushed(state store.State) bool {
	switch state {
	case store.StatePushed, store.StateTracked, store.StateSkipped,
		store.StateSnoozed, store.StateExpired:
		return true
	}
	return false
}

func (s Summary) String() string {
	return fmt.Sprintf(
		"%d scored issues: %d would push, %d would reject "+
			"(%d newly pushed, %d newly rejected)\n"+
			"  %d tracked issues would have been killed  <- the only error that costs anything\n"+
			"  %d skipped issues would have been suppressed\n"+
			"  %d degraded rows excluded",
		s.Total, s.WouldPush, s.WouldReject, s.NewlyPushed, s.NewlyRejected,
		s.TrackedLost, s.SkippedSpared, s.DegradedSkip)
}
