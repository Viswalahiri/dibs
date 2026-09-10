package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedNew(t *testing.T, s *Store) (int64, time.Time) {
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
		CreatedAt: now, FirstSeenAt: now, State: StateNew,
	})
	if err != nil || !inserted {
		t.Fatalf("seed: inserted=%v err=%v", inserted, err)
	}
	return id, now
}

// Every write that advances an issue is a conditional update that checks how
// many rows it changed. A worker whose lease expired mid-flight can therefore
// finish and write its result harmlessly, which is the property that lets the
// reaper hand rows back without coordinating with anyone.
func TestSaveVerdictIsIdempotent(t *testing.T) {
	for _, tt := range []struct {
		state  State
		reason RejectReason
	}{
		{StateReady, ""},
		{StateRejected, ReasonKillfileLabel},
	} {
		t.Run(string(tt.state), func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			id, _ := seedNew(t, s)

			if err := s.SaveVerdict(ctx, id, tt.state, tt.reason); err != nil {
				t.Fatalf("first write: %v", err)
			}
			err := s.SaveVerdict(ctx, id, tt.state, tt.reason)
			if !errors.Is(err, ErrNotClaimable) {
				t.Fatalf("second write returned %v, want ErrNotClaimable", err)
			}

			iss, err := s.IssueByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if iss.State != tt.state {
				t.Errorf("state is %s, want %s", iss.State, tt.state)
			}
			if iss.RejectReason != tt.reason {
				t.Errorf("reason is %q, want %q", iss.RejectReason, tt.reason)
			}
		})
	}
}

// An illegal move is a programming error, caught at the call rather than
// silently writing a state the pipeline has no worker for.
func TestIllegalTransitionsAreRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, now := seedNew(t, s)

	// new -> pushed skips the filter, and is not in the table.
	if err := s.Surface(ctx, id, now); err == nil {
		t.Fatal("surfacing an unscreened issue was allowed")
	}
	// Every rejection carries a reason, so a reasonless one is refused before
	// it reaches the database.
	if err := s.SaveVerdict(ctx, id, StateRejected, ""); err == nil {
		t.Fatal("a reasonless rejection was allowed")
	}
	// The row is untouched by either refusal.
	iss, err := s.IssueByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if iss.State != StateNew {
		t.Errorf("state is %s, want it left in %s", iss.State, StateNew)
	}
}

// Pushing is the end of the line. There is no path out of it, so a repeated
// send is refused rather than notifying twice.
func TestSurfaceHappensOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, now := seedNew(t, s)

	if err := s.SaveVerdict(ctx, id, StateReady, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Surface(ctx, id, now); err != nil {
		t.Fatalf("first surface: %v", err)
	}
	if err := s.Surface(ctx, id, now.Add(time.Second)); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("second surface returned %v, want ErrNotClaimable", err)
	}
	iss, err := s.IssueByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !iss.SurfacedAt.Equal(now) {
		t.Errorf("the second send moved surfaced_at to %v", iss.SurfacedAt)
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
