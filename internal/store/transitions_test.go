package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedPushed(t *testing.T, s *Store) (int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	res, err := s.DB().ExecContext(ctx,
		`INSERT INTO repos (owner, name) VALUES ('acme', 'widget')`)
	if err != nil {
		t.Fatal(err)
	}
	repoID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	id, inserted, err := s.Insert(ctx, Issue{
		RepoID: repoID, Number: 7, NodeID: "I_7", Title: "Drain deadlocks",
		HTMLURL: "https://example.invalid/7", Author: "reporter", AuthorAssoc: "NONE",
		CreatedAt: now, FirstSeenAt: now, State: StateEnriched,
	})
	if err != nil || !inserted {
		t.Fatalf("seed: inserted=%v err=%v", inserted, err)
	}
	if err := s.SaveTriage(ctx, id, TriageResult{Score: 82, State: StateScored}); err != nil {
		t.Fatal(err)
	}
	if err := s.Surface(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	return id, now
}

// Every button handler guards with a conditional update and checks how many
// rows it changed, so a double tap is a no-op rather than a double process.
// This is the property that makes the Slack handlers safe to be careless with.
func TestDecideIsIdempotent(t *testing.T) {
	for _, state := range []State{StateTracked, StateSkipped, StateSnoozed} {
		t.Run(string(state), func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			id, now := seedPushed(t, s)

			if err := s.Decide(ctx, id, state, now); err != nil {
				t.Fatalf("first press: %v", err)
			}
			err := s.Decide(ctx, id, state, now.Add(time.Second))
			if !errors.Is(err, ErrNotClaimable) {
				t.Fatalf("second press returned %v, want ErrNotClaimable", err)
			}

			iss, err := s.IssueByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if iss.State != state {
				t.Errorf("state is %s, want %s", iss.State, state)
			}
			if !iss.DecidedAt.Equal(now) {
				t.Errorf("the second press moved decided_at to %v", iss.DecidedAt)
			}
		})
	}
}

// Pressing a different button after the first one is also refused. The row has
// left `pushed`, and there is no path back.
func TestDecideRefusesASecondDifferentButton(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, now := seedPushed(t, s)

	if err := s.Decide(ctx, id, StateTracked, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, id, StateSkipped, now); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("skip after track returned %v, want ErrNotClaimable", err)
	}
}

// Track is recorded once no matter how many times it is pressed, so the outcome
// table never grows a duplicate.
func TestTrackIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, now := seedPushed(t, s)

	for i := 0; i < 3; i++ {
		if err := s.Track(ctx, id, now); err != nil {
			t.Fatalf("track %d: %v", i, err)
		}
	}
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracked WHERE issue_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("tracked %d times, want 1", n)
	}
}

// An illegal move is a programming error, caught at the call rather than
// silently writing a state the pipeline has no worker for.
func TestIllegalTransitionsAreRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, now := seedPushed(t, s)

	// pushed -> scored is not in the table.
	if err := s.Surface(ctx, id, now); err == nil {
		t.Fatal("surfacing an already pushed issue was allowed")
	}
	// A rejection with no reason is refused: every rejection carries one.
	if err := s.SaveTriage(ctx, id, TriageResult{State: StateRejected}); err == nil {
		t.Fatal("a reasonless rejection was allowed")
	}
}

// Two workers racing for the same row must produce one winner. This is the
// property the whole database-as-queue design rests on.
func TestClaimHandsARowToOneWorker(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	res, err := s.DB().ExecContext(ctx, `INSERT INTO repos (owner, name) VALUES ('acme', 'widget')`)
	if err != nil {
		t.Fatal(err)
	}
	repoID, _ := res.LastInsertId()
	if _, _, err := s.Insert(ctx, Issue{
		RepoID: repoID, Number: 1, NodeID: "I_1", Title: "t", HTMLURL: "u",
		Author: "a", AuthorAssoc: "NONE", CreatedAt: now, FirstSeenAt: now, State: StateNew,
	}); err != nil {
		t.Fatal(err)
	}

	first, err := s.Claim(ctx, StateNew, 10, "worker-a", 5*time.Minute, now)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %d rows, err %v", len(first), err)
	}
	second, err := s.Claim(ctx, StateNew, 10, "worker-b", 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("a leased row was handed to a second worker: %d rows", len(second))
	}

	// Once the lease expires the row comes back, which is the whole of crash
	// recovery.
	third, err := s.Claim(ctx, StateNew, 10, "worker-b", 5*time.Minute, now.Add(6*time.Minute))
	if err != nil || len(third) != 1 {
		t.Fatalf("an expired lease did not release the row: %d rows, err %v", len(third), err)
	}
}
