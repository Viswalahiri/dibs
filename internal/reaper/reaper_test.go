package reaper

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
)

var now = time.Date(2026, 3, 20, 4, 0, 0, 0, time.UTC)

// TestExpireStale drops issues the push worker never reached.
func TestExpireStale(t *testing.T) {
	r, db, repoID := newReaper(t)
	seedIssue(t, db, repoID, 1, store.StateReady, now.Add(-20*24*time.Hour))
	seedIssue(t, db, repoID, 2, store.StateReady, now.Add(-2*24*time.Hour))

	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	counts, err := db.CountByState(context.Background(), repoID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[store.StateExpired] != 1 || counts[store.StateReady] != 1 {
		t.Fatalf("want one expired and one still ready, got %v", counts)
	}
}

// TestCadenceFollowsObservedVolume checks the table from the specification and
// the rate it is read with. The window is what dibs has actually watched, so a
// busy repository adopted yesterday is polled quickly today.
func TestCadenceFollowsObservedVolume(t *testing.T) {
	r, _, _ := newReaper(t)
	cases := []struct {
		name     string
		seen     int
		observed time.Duration
		want     int
	}{
		{"busy repo, a month of history", 60, 30 * 24 * time.Hour, 30},
		{"busy repo, judged on its first week", 10, 7 * 24 * time.Hour, 30},
		{"a handful a week", 30, 30 * 24 * time.Hour, 60},
		{"one or two a week", 6, 30 * 24 * time.Hour, 300},
		{"silent", 0, 30 * 24 * time.Hour, 1800},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.intervalFor(perWeek(tc.seen, tc.observed)); got != tc.want {
				t.Errorf("interval = %ds, want %ds", got, tc.want)
			}
		})
	}
}

// TestCadenceLeavesAYoungRepoAlone is the guard against the rut. A repository
// adopted this morning has produced nothing yet, and reading that as silence
// would drop it to half-hourly polling, slow enough that its next issue is
// stale before dibs sees it.
func TestCadenceLeavesAYoungRepoAlone(t *testing.T) {
	r, db, repoID := newReaper(t)
	ctx := context.Background()
	adopt(t, db, repoID, now.Add(-6*time.Hour))

	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	repo, err := db.RepoByID(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if repo.PollIntervalSec != 45 {
		t.Fatalf("interval moved to %ds on the first day; want the configured 45s",
			repo.PollIntervalSec)
	}
}

// TestCadenceIsWritten proves the recompute reaches the row the poller reads.
func TestCadenceIsWritten(t *testing.T) {
	r, db, repoID := newReaper(t)
	ctx := context.Background()
	adopt(t, db, repoID, now.AddDate(0, 0, -30))
	for i := 1; i <= 60; i++ {
		seedIssue(t, db, repoID, i, store.StateRejected, now.Add(-time.Duration(i)*time.Hour))
	}

	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	repo, err := db.RepoByID(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if repo.PollIntervalSec != 30 || repo.IssuesLast30d != 60 {
		t.Fatalf("interval %ds over %d issues, want 30s over 60",
			repo.PollIntervalSec, repo.IssuesLast30d)
	}
}

// TestDarkRepoWarning catches a repository that has stopped producing anything
// dibs will surface. The symptom is silence, so nothing in the alert stream
// would ever show it.
func TestDarkRepoWarning(t *testing.T) {
	// The window is whole days, so the issues have to sit before last midnight.
	midnight := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)

	t.Run("nearly everything rejected warns", func(t *testing.T) {
		r, db, repoID := newReaper(t)
		for i := 1; i <= 12; i++ {
			state := store.StateRejected
			if i == 12 {
				state = store.StatePushed
			}
			seedIssue(t, db, repoID, i, state, midnight.Add(-time.Duration(i)*12*time.Hour))
		}
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertWarning(t, db, "dark_repo")
	})

	t.Run("a quiet week is not a dark repo", func(t *testing.T) {
		r, db, repoID := newReaper(t)
		for i := 1; i <= 5; i++ {
			seedIssue(t, db, repoID, i, store.StateRejected, midnight.Add(-time.Duration(i)*12*time.Hour))
		}
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertNoWarning(t, db, "dark_repo")
	})
}

// TestDailyRunsOnce keeps the report to one a day across the ticks in between.
func TestDailyRunsOnce(t *testing.T) {
	r, db, repoID := newReaper(t)
	seedDarkWeek(t, db, repoID)
	for i := 0; i < 4; i++ {
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := pending(t, db); n != 1 {
		t.Fatalf("four ticks produced %d messages, want exactly 1", n)
	}
}

// TestDailyWaitsForItsHour keeps the report off the small hours before the
// configured time.
func TestDailyWaitsForItsHour(t *testing.T) {
	r, db, repoID := newReaper(t)
	r.now = func() time.Time { return time.Date(2026, 3, 20, 1, 0, 0, 0, time.UTC) }
	seedDarkWeek(t, db, repoID)

	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := pending(t, db); n != 0 {
		t.Fatalf("reported %d messages before the configured hour", n)
	}
}

// --- fixtures ---

func newReaper(t *testing.T, mut ...func(*config.Config)) (*Reaper, *store.Store, int64) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	if err := db.SyncRepos(ctx, []config.Repo{{Slug: "acme/widget"}}, 45); err != nil {
		t.Fatal(err)
	}
	repo, err := db.RepoBySlug(ctx, "acme", "widget")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Profile: config.Profile{GitHubLogin: "test-user"},
		Polling: config.Polling{DefaultIntervalSec: 45, MinIntervalSec: 30, MaxConcurrent: 4},
		Reaper:  config.Reaper{ExpireAfterDays: 14, CadenceRecomputeHour: 3, LeaseTTLSec: 300},
	}
	for _, f := range mut {
		f(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	warn := func(kind, message string) {
		if _, err := db.Enqueue(ctx, store.Outgoing{
			Kind: "warning", DedupeKey: kind + ":" + message, Payload: `{"text":""}`,
		}, now); err != nil {
			t.Error(err)
		}
	}
	r := New(db, cfg, log, warn, WithClock(func() time.Time { return now }))
	return r, db, repo.ID
}

func seedIssue(t *testing.T, db *store.Store, repoID int64, number int, state store.State, firstSeen time.Time) int64 {
	t.Helper()
	res, err := db.DB().Exec(`
		INSERT INTO issues (repo_id, number, title, body, html_url, author,
		                    created_at, first_seen_at, state)
		VALUES (?, ?, ?, 'body', ?, 'reporter', ?, ?, ?)`,
		repoID, number, fmt.Sprintf("issue %d", number),
		fmt.Sprintf("https://github.com/acme/widget/issues/%d", number),
		firstSeen.Unix(), firstSeen.Unix(), string(state))
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// seedDarkWeek lays down enough rejections to trip the dark-repo warning, which
// is the only thing the daily pass has left to say.
func seedDarkWeek(t *testing.T, db *store.Store, repoID int64) {
	t.Helper()
	midnight := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 12; i++ {
		seedIssue(t, db, repoID, i, store.StateRejected, midnight.Add(-time.Duration(i)*12*time.Hour))
	}
}

func adopt(t *testing.T, db *store.Store, repoID int64, at time.Time) {
	t.Helper()
	if err := db.MarkAdopted(context.Background(), repoID, at, at); err != nil {
		t.Fatal(err)
	}
}

func pending(t *testing.T, db *store.Store) int {
	t.Helper()
	rows, err := db.Pending(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func assertWarning(t *testing.T, db *store.Store, want string) {
	t.Helper()
	if !hasWarning(t, db, want) {
		t.Errorf("no queued warning mentions %q", want)
	}
}

func assertNoWarning(t *testing.T, db *store.Store, want string) {
	t.Helper()
	if hasWarning(t, db, want) {
		t.Errorf("a warning mentioning %q was queued and should not have been", want)
	}
}

func hasWarning(t *testing.T, db *store.Store, want string) bool {
	t.Helper()
	rows, err := db.Pending(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row.DedupeKey, want) {
			return true
		}
	}
	return false
}
