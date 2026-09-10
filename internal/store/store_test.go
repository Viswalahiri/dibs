package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var epoch = time.Unix(1_700_000_000, 0).UTC()

func TestSchemaIsIdempotent(t *testing.T) {
	s := open(t)
	if _, err := s.db.Exec(schema); err != nil {
		t.Fatalf("re-applying schema: %v", err)
	}
}

func TestSyncReposPreservesRuntimeState(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	want := []config.Repo{
		{Slug: "golang/go", Notes: "compiler work"},
		{Slug: "rust-lang/rust"},
	}
	if err := s.SyncRepos(ctx, want, 45); err != nil {
		t.Fatalf("SyncRepos: %v", err)
	}

	repo, err := s.RepoBySlug(ctx, "golang", "go")
	if err != nil {
		t.Fatalf("RepoBySlug: %v", err)
	}
	if repo.Notes != "compiler work" {
		t.Errorf("notes = %q, want the configured text", repo.Notes)
	}
	if repo.Adopted() {
		t.Error("a freshly synced repo must not look adopted")
	}

	waterline := epoch
	if err := s.MarkAdopted(ctx, repo.ID, waterline, epoch); err != nil {
		t.Fatalf("MarkAdopted: %v", err)
	}
	if err := s.RecordPoll(ctx, repo.ID, `W/"abc"`, epoch.Add(time.Minute), waterline.Add(time.Minute)); err != nil {
		t.Fatalf("RecordPoll: %v", err)
	}

	// A SIGHUP re-sync must not re-adopt or rewind the waterline, otherwise
	// every reload would replay the repository's whole open issue list.
	want[0].Notes = "runtime work"
	if err := s.SyncRepos(ctx, want, 45); err != nil {
		t.Fatalf("re-SyncRepos: %v", err)
	}
	repo, err = s.RepoBySlug(ctx, "golang", "go")
	if err != nil {
		t.Fatalf("RepoBySlug: %v", err)
	}
	if repo.Notes != "runtime work" {
		t.Errorf("notes were not refreshed, got %q", repo.Notes)
	}
	if !repo.Adopted() {
		t.Error("adoption was lost across a re-sync")
	}
	if got, want := repo.WaterlineAt, epoch.Add(time.Minute); !got.Equal(want) {
		t.Errorf("waterline = %v, want %v", got, want)
	}
	if repo.ETag != `W/"abc"` {
		t.Errorf("etag = %q, want it preserved", repo.ETag)
	}
}

func TestRecordPollNeverRewindsTheWaterline(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.SyncRepos(ctx, []config.Repo{{Slug: "a/b"}}, 45); err != nil {
		t.Fatal(err)
	}
	repo, _ := s.RepoBySlug(ctx, "a", "b")

	if err := s.RecordPoll(ctx, repo.ID, "", epoch, epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPoll(ctx, repo.ID, "", epoch, epoch); err != nil {
		t.Fatal(err)
	}
	repo, _ = s.RepoBySlug(ctx, "a", "b")
	if got, want := repo.WaterlineAt, epoch.Add(time.Hour); !got.Equal(want) {
		t.Errorf("waterline = %v, want it to stay at %v", got, want)
	}
}

func TestSyncReposDisablesRemovedRepos(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.SyncRepos(ctx, []config.Repo{{Slug: "a/b"}, {Slug: "c/d"}}, 45); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncRepos(ctx, []config.Repo{{Slug: "a/b"}}, 45); err != nil {
		t.Fatal(err)
	}
	enabled, err := s.EnabledRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || enabled[0].Slug() != "a/b" {
		t.Fatalf("enabled = %v, want just a/b", enabled)
	}
	// The row survives so its issue history stays intact.
	if _, err := s.RepoBySlug(ctx, "c", "d"); err != nil {
		t.Errorf("removed repo was deleted rather than disabled: %v", err)
	}
}

func seedRepo(t *testing.T, s *Store) Repo {
	t.Helper()
	ctx := context.Background()
	if err := s.SyncRepos(ctx, []config.Repo{{Slug: "a/b"}}, 45); err != nil {
		t.Fatal(err)
	}
	r, err := s.RepoBySlug(ctx, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newIssue(repoID int64, number int, state State) Issue {
	return Issue{
		RepoID: repoID, Number: number, NodeID: "node", Title: "t",
		HTMLURL: "https://example.invalid", Author: "someone", AuthorAssoc: "NONE",
		Labels: []string{"bug"}, CreatedAt: epoch, FirstSeenAt: epoch, State: state,
	}
}

func TestInsertIsDeduplicatedByRepoAndNumber(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	repo := seedRepo(t, s)

	id, inserted, err := s.Insert(ctx, newIssue(repo.ID, 1, StateNew))
	if err != nil || !inserted {
		t.Fatalf("first insert: id=%d inserted=%v err=%v", id, inserted, err)
	}
	_, inserted, err = s.Insert(ctx, newIssue(repo.ID, 1, StateNew))
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if inserted {
		t.Error("the same issue was inserted twice; it would be emitted twice too")
	}
}

func TestClaimLeasesExclusively(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	repo := seedRepo(t, s)
	for i := 1; i <= 3; i++ {
		if _, _, err := s.Insert(ctx, newIssue(repo.ID, i, StateNew)); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.Claim(ctx, StateNew, 2, "w1", time.Minute, epoch)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("claimed %d, want 2", len(first))
	}
	second, err := s.Claim(ctx, StateNew, 2, "w2", time.Minute, epoch)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second claim got %d rows, want the 1 remaining", len(second))
	}
	for _, a := range first {
		for _, b := range second {
			if a.ID == b.ID {
				t.Fatalf("issue %d was claimed by two workers", a.ID)
			}
		}
	}
	// Claimed rows come back whole, not as bare ids.
	if first[0].Labels == nil || first[0].Labels[0] != "bug" {
		t.Errorf("claimed issue lost its labels: %v", first[0].Labels)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	repo := seedRepo(t, s)
	if _, _, err := s.Insert(ctx, newIssue(repo.ID, 1, StateNew)); err != nil {
		t.Fatal(err)
	}

	if got, err := s.Claim(ctx, StateNew, 10, "crashed", time.Minute, epoch); err != nil || len(got) != 1 {
		t.Fatalf("first claim: %d rows, %v", len(got), err)
	}
	if got, err := s.Claim(ctx, StateNew, 10, "w2", time.Minute, epoch); err != nil || len(got) != 0 {
		t.Fatalf("live lease was ignored: %d rows, %v", len(got), err)
	}
	later := epoch.Add(2 * time.Minute)
	got, err := s.Claim(ctx, StateNew, 10, "w2", time.Minute, later)
	if err != nil || len(got) != 1 {
		t.Fatalf("expired lease was not reclaimed: %d rows, %v", len(got), err)
	}
}

func TestReleaseExpiredLeases(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	repo := seedRepo(t, s)
	if _, _, err := s.Insert(ctx, newIssue(repo.ID, 1, StateNew)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, StateNew, 10, "crashed", time.Minute, epoch); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReleaseExpiredLeases(ctx, epoch.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("released %d leases, want 1", n)
	}
}

func TestAdvanceRefusesIllegalTransitions(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	repo := seedRepo(t, s)
	id, _, err := s.Insert(ctx, newIssue(repo.ID, 1, StateNew))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Advance(ctx, id, StateNew, StatePushed, ""); err == nil {
		t.Error("new -> pushed should be refused; it would skip the filter")
	}
	if err := s.Advance(ctx, id, StateNew, StateRejected, ""); err == nil {
		t.Error("a rejection without a reason should be refused")
	}
	if err := s.Advance(ctx, id, StateNew, StateRejected, ReasonTooThin); err != nil {
		t.Errorf("legal rejection failed: %v", err)
	}
}

func TestAdvanceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	repo := seedRepo(t, s)
	id, _, err := s.Insert(ctx, newIssue(repo.ID, 1, StateNew))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Advance(ctx, id, StateNew, StateReady, ""); err != nil {
		t.Fatalf("first advance: %v", err)
	}
	err = s.Advance(ctx, id, StateNew, StateReady, "")
	if !errors.Is(err, ErrNotClaimable) {
		t.Errorf("second advance returned %v, want ErrNotClaimable", err)
	}
	iss, err := s.IssueByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if iss.State != StateReady {
		t.Errorf("state = %q, want ready", iss.State)
	}
}

func TestAdvanceClearsTheLease(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	repo := seedRepo(t, s)
	id, _, err := s.Insert(ctx, newIssue(repo.ID, 1, StateNew))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, StateNew, 10, "w1", time.Minute, epoch); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance(ctx, id, StateNew, StateReady, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claim(ctx, StateReady, 10, "w2", time.Minute, epoch)
	if err != nil || len(got) != 1 {
		t.Fatalf("issue was not claimable in its new state: %d rows, %v", len(got), err)
	}
}

// Terminal states must have no outgoing transitions, and every state named in
// a transition list must itself be a known state.
func TestStateTableIsClosed(t *testing.T) {
	terminal := []State{
		StateBaseline, StatePushed, StateRejected,
		StateAgedOut, StateClaimedBeforePush, StateExpired,
	}
	for _, st := range terminal {
		if len(transitions[st]) != 0 {
			t.Errorf("%s is terminal but has outgoing transitions %v", st, transitions[st])
		}
	}
	for from, tos := range transitions {
		if !from.Valid() {
			t.Errorf("transition source %q is not a known state", from)
		}
		for _, to := range tos {
			if !to.Valid() {
				t.Errorf("%s -> %q targets an unknown state", from, to)
			}
		}
	}
	if StateBaseline.Claimable() {
		t.Error("baseline rows must never be claimed by a worker; adoption costs nothing by design")
	}
	if StateAgedOut.Claimable() {
		t.Error("aged-out rows must never be claimed; that is the point of the cutoff")
	}
}

func TestClaimRejectsNonClaimableState(t *testing.T) {
	s := open(t)
	if _, err := s.Claim(context.Background(), StateAgedOut, 1, "w", time.Minute, epoch); err == nil {
		t.Error("claiming an aged-out row should be refused")
	}
}
