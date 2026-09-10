package reaper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

var now = time.Date(2026, 3, 20, 4, 0, 0, 0, time.UTC)

// TestOutcomeLanded covers the one number outcome tracking exists to produce:
// an issue the operator took, closed with the operator's own pull request on
// it.
func TestOutcomeLanded(t *testing.T) {
	closed := now.Add(-2 * time.Hour)
	r, db, repoID := newReaper(t, githubFixture{
		issue: map[string]any{
			"number": 7, "state": "closed",
			"closed_at": closed.Format(time.RFC3339),
			"assignees": []map[string]string{{"login": "test-user"}},
		},
		timeline: crossReference("https://github.com/acme/widget/pull/9", "test-user", "closed"),
	})
	track(t, db, repoID, 7, now.Add(-24*time.Hour))

	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := outcome(t, db, 7)
	if got.Outcome != store.OutcomeLanded {
		t.Errorf("outcome = %q, want %q", got.Outcome, store.OutcomeLanded)
	}
	if got.PRURL != "https://github.com/acme/widget/pull/9" {
		t.Errorf("pr_url = %q", got.PRURL)
	}
	if !got.ClosedAt.Equal(closed) {
		t.Errorf("closed_at = %v, want the time GitHub reported, %v", got.ClosedAt, closed)
	}
	if got.AssignedAt.IsZero() {
		t.Error("assigned_at was not recorded")
	}
}

// TestOutcomeLostToSomeoneElse is the case the nudge depends on. Another
// contributor's pull request must not be recorded as the operator's, or a
// tracked issue that was quietly lost would look like one in progress.
func TestOutcomeLostToSomeoneElse(t *testing.T) {
	r, db, repoID := newReaper(t, githubFixture{
		issue: map[string]any{
			"number": 7, "state": "closed",
			"closed_at": now.Format(time.RFC3339),
		},
		timeline: crossReference("https://github.com/acme/widget/pull/9", "someone-else", "closed"),
	})
	track(t, db, repoID, 7, now.Add(-24*time.Hour))

	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := outcome(t, db, 7)
	if got.Outcome != store.OutcomeLost {
		t.Errorf("outcome = %q, want %q", got.Outcome, store.OutcomeLost)
	}
	if got.PRURL != "" {
		t.Errorf("recorded someone else's pull request as yours: %q", got.PRURL)
	}
}

// TestOutcomeStaysPendingWhileOpen keeps an open issue in the tracking loop.
func TestOutcomeStaysPendingWhileOpen(t *testing.T) {
	r, db, repoID := newReaper(t, githubFixture{
		issue:    map[string]any{"number": 7, "state": "open"},
		timeline: []any{},
	})
	track(t, db, repoID, 7, now.Add(-24*time.Hour))

	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := outcome(t, db, 7)
	if got.Outcome != store.OutcomePending {
		t.Errorf("outcome = %q, want %q", got.Outcome, store.OutcomePending)
	}
	if got.LastChecked.IsZero() {
		t.Error("last_checked was not stamped, so the issue would be re-checked immediately")
	}
}

// TestGoneIssueStopsBeingChecked closes out an issue that was deleted or
// transferred. Without this it would cost two requests every six hours forever.
func TestGoneIssueStopsBeingChecked(t *testing.T) {
	r, db, repoID := newReaper(t, githubFixture{missing: true})
	track(t, db, repoID, 7, now.Add(-24*time.Hour))

	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := outcome(t, db, 7); got.Outcome != store.OutcomeLost {
		t.Errorf("outcome = %q, want %q", got.Outcome, store.OutcomeLost)
	}
}

// TestNudgeAsksOnce covers both halves of the nudge: it waits five days, and it
// never asks twice.
func TestNudgeAsksOnce(t *testing.T) {
	r, db, repoID := newReaper(t, githubFixture{
		issue:    map[string]any{"number": 7, "state": "open"},
		timeline: []any{},
	})
	ctx := context.Background()

	track(t, db, repoID, 7, now.Add(-4*24*time.Hour))
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := pending(t, db); n != 0 {
		t.Fatalf("nudged after four days: %d messages", n)
	}

	setTrackedAt(t, db, 7, now.Add(-6*24*time.Hour))
	for i := 0; i < 3; i++ {
		if err := r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := pending(t, db); n != 1 {
		t.Fatalf("three ticks past the deadline produced %d messages, want exactly 1", n)
	}
}

// TestExpireStale drops issues nobody ever decided on.
func TestExpireStale(t *testing.T) {
	r, db, repoID := newReaper(t, githubFixture{})
	seedIssue(t, db, repoID, 1, store.StateScored, 80, now.Add(-20*24*time.Hour))
	seedIssue(t, db, repoID, 2, store.StateScored, 80, now.Add(-2*24*time.Hour))

	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	counts, err := db.CountByState(context.Background(), repoID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[store.StateExpired] != 1 || counts[store.StateScored] != 1 {
		t.Fatalf("want one expired and one still scored, got %v", counts)
	}
}

// TestCadenceFollowsObservedVolume checks the table from the specification and
// the rate it is read with. The window is what dibs has actually watched, so a
// busy repository adopted yesterday is polled quickly today.
func TestCadenceFollowsObservedVolume(t *testing.T) {
	r, _, _ := newReaper(t, githubFixture{})
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
	r, db, repoID := newReaper(t, githubFixture{})
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
	r, db, repoID := newReaper(t, githubFixture{})
	ctx := context.Background()
	adopt(t, db, repoID, now.AddDate(0, 0, -30))
	for i := 1; i <= 60; i++ {
		seedIssue(t, db, repoID, i, store.StateRejected, nil, now.Add(-time.Duration(i)*time.Hour))
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

// TestRubricWarningNeedsAStreak is the guard against a model that has started
// agreeing with everything. One high day is a good day; three in a row means
// the rubric has stopped discriminating.
func TestRubricWarningNeedsAStreak(t *testing.T) {
	midnight := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)

	t.Run("two high days and one ordinary one stay quiet", func(t *testing.T) {
		r, db, repoID := newReaper(t, githubFixture{})
		seedDay(t, db, repoID, midnight.AddDate(0, 0, -1), 90, 92, 95)
		seedDay(t, db, repoID, midnight.AddDate(0, 0, -2), 88, 90, 91)
		seedDay(t, db, repoID, midnight.AddDate(0, 0, -3), 40, 50, 60)
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertNoWarning(t, db, "recalibration")
	})

	t.Run("three high days warn", func(t *testing.T) {
		r, db, repoID := newReaper(t, githubFixture{})
		for i := 1; i <= 3; i++ {
			seedDay(t, db, repoID, midnight.AddDate(0, 0, -i), 90, 92, 95)
		}
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertWarning(t, db, "recalibration")
	})
}

// TestSkipRateWarning is the alert-fatigue instrument. Skipping most of what
// arrives means the junk floor is too low.
func TestSkipRateWarning(t *testing.T) {
	yesterday := time.Date(2026, 3, 19, 12, 0, 0, 0, time.UTC)
	seedSkips := func(t *testing.T, db *store.Store, repoID int64) {
		t.Helper()
		for i := 1; i <= 4; i++ {
			decide(t, db, repoID, i, store.StateSkipped, yesterday)
		}
		decide(t, db, repoID, 5, store.StateTracked, yesterday)
	}

	t.Run("a floor that admits junk warns", func(t *testing.T) {
		r, db, repoID := newReaper(t, githubFixture{})
		seedSkips(t, db, repoID)
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertWarning(t, db, "junk_floor")
	})

	// At a floor of zero the operator asked to see everything, so skipping most
	// of it is the arrangement working. Warning daily about a deliberate choice
	// is how an instrument stops being read.
	t.Run("no floor means no warning", func(t *testing.T) {
		r, db, repoID := newReaper(t, githubFixture{}, func(c *config.Config) {
			c.Scoring.JunkFloor = 0
		})
		seedSkips(t, db, repoID)
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertNoWarning(t, db, "junk_floor")
	})
}

// TestDarkRepoWarning catches a repository that has stopped producing anything
// dibs will surface. The symptom is silence, so nothing in the alert stream
// would ever show it.
func TestDarkRepoWarning(t *testing.T) {
	// The window is whole days, so the issues have to sit before last midnight.
	midnight := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)

	t.Run("nearly everything rejected warns", func(t *testing.T) {
		r, db, repoID := newReaper(t, githubFixture{})
		for i := 1; i <= 12; i++ {
			state := store.StateRejected
			if i == 12 {
				state = store.StatePushed
			}
			seedIssue(t, db, repoID, i, state, 30, midnight.Add(-time.Duration(i)*12*time.Hour))
		}
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertWarning(t, db, "dark_repo")
	})

	t.Run("a quiet week is not a dark repo", func(t *testing.T) {
		r, db, repoID := newReaper(t, githubFixture{})
		for i := 1; i <= 5; i++ {
			seedIssue(t, db, repoID, i, store.StateRejected, 30, midnight.Add(-time.Duration(i)*12*time.Hour))
		}
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertNoWarning(t, db, "dark_repo")
	})
}

// TestDailyRunsOnce keeps the reports to one a day across the ticks in between.
func TestDailyRunsOnce(t *testing.T) {
	r, db, repoID := newReaper(t, githubFixture{})
	yesterday := time.Date(2026, 3, 19, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 4; i++ {
		decide(t, db, repoID, i, store.StateSkipped, yesterday)
	}
	for i := 0; i < 4; i++ {
		if err := r.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := pending(t, db); n != 1 {
		t.Fatalf("four ticks produced %d messages, want exactly 1", n)
	}
}

// TestDailyWaitsForItsHour keeps the reports off the small hours before the
// configured time.
func TestDailyWaitsForItsHour(t *testing.T) {
	r, db, repoID := newReaper(t, githubFixture{})
	r.now = func() time.Time { return time.Date(2026, 3, 20, 1, 0, 0, 0, time.UTC) }
	yesterday := time.Date(2026, 3, 19, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 4; i++ {
		decide(t, db, repoID, i, store.StateSkipped, yesterday)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := pending(t, db); n != 0 {
		t.Fatalf("reported %d messages before the configured hour", n)
	}
}

func TestPercentile(t *testing.T) {
	cases := []struct {
		name   string
		values []int
		p      float64
		want   int
	}{
		{"empty", nil, 0.5, 0},
		{"single", []int{62}, 0.5, 62},
		{"odd median", []int{40, 50, 60}, 0.5, 50},
		{"even median takes the lower", []int{40, 50, 60, 70}, 0.5, 50},
		{"p90 of ten", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.90, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.values, tc.p); got != tc.want {
				t.Errorf("percentile = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- fixtures ---

// githubFixture serves the two endpoints outcome tracking reads. A zero value
// serves nothing, which is what the tests that never reach GitHub want.
type githubFixture struct {
	issue    map[string]any
	timeline []any
	missing  bool
}

func (f githubFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("dibs made a %s request to %s; the token is read-only", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("Content-Type", "application/json")
		if f.missing {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/timeline") {
			json.NewEncoder(w).Encode(f.timeline)
			return
		}
		json.NewEncoder(w).Encode(f.issue)
	})
}

func crossReference(prURL, author, state string) []any {
	return []any{map[string]any{
		"event": "cross-referenced",
		"source": map[string]any{"issue": map[string]any{
			"state": state, "html_url": prURL,
			"user":         map[string]string{"login": author},
			"pull_request": map[string]string{"url": prURL},
		}},
	}}
}

func newReaper(t *testing.T, fixture githubFixture, mut ...func(*config.Config)) (*Reaper, *store.Store, int64) {
	t.Helper()
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)

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

	// The floor is set here rather than left at zero because most of these tests
	// are about instruments that only mean something once a floor exists.
	cfg := &config.Config{
		Profile: config.Profile{GitHubLogin: "test-user", EffortCeilingHours: 16},
		Scoring: config.Scoring{JunkFloor: 40, VetoConfidence: 0.60},
		Polling: config.Polling{DefaultIntervalSec: 45, MinIntervalSec: 30, MaxConcurrent: 4},
		Triage:  config.Triage{Cost: config.Cost{InputPerMTokUSD: 2, OutputPerMTokUSD: 10}},
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
	client := gh.New("token", 4, gh.WithBaseURL(server.URL))
	r := New(client, db, cfg, log, warn, WithClock(func() time.Time { return now }))
	return r, db, repo.ID
}

func seedIssue(t *testing.T, db *store.Store, repoID int64, number int, state store.State, score any, firstSeen time.Time) int64 {
	t.Helper()
	res, err := db.DB().Exec(`
		INSERT INTO issues (repo_id, number, node_id, title, body, html_url, author,
		                    author_assoc, created_at, first_seen_at, state, score)
		VALUES (?, ?, ?, ?, 'body', ?, 'reporter', 'NONE', ?, ?, ?, ?)`,
		repoID, number, fmt.Sprintf("I_%d", number), fmt.Sprintf("issue %d", number),
		fmt.Sprintf("https://github.com/acme/widget/issues/%d", number),
		firstSeen.Unix(), firstSeen.Unix(), string(state), score)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedDay(t *testing.T, db *store.Store, repoID int64, midnight time.Time, scores ...int) {
	t.Helper()
	for i, score := range scores {
		seedIssue(t, db, repoID, int(midnight.Unix())+i, store.StateSkipped, score,
			midnight.Add(time.Duration(i+1)*time.Hour))
	}
}

func decide(t *testing.T, db *store.Store, repoID int64, number int, state store.State, at time.Time) {
	t.Helper()
	id := seedIssue(t, db, repoID, number, state, 60, at)
	if _, err := db.DB().Exec(`UPDATE issues SET decided_at = ? WHERE id = ?`, at.Unix(), id); err != nil {
		t.Fatal(err)
	}
}

func adopt(t *testing.T, db *store.Store, repoID int64, at time.Time) {
	t.Helper()
	if err := db.MarkAdopted(context.Background(), repoID, at, at); err != nil {
		t.Fatal(err)
	}
}

func track(t *testing.T, db *store.Store, repoID int64, number int, at time.Time) {
	t.Helper()
	id := seedIssue(t, db, repoID, number, store.StateTracked, 80, at)
	if err := db.Track(context.Background(), id, at); err != nil {
		t.Fatal(err)
	}
}

func setTrackedAt(t *testing.T, db *store.Store, number int, at time.Time) {
	t.Helper()
	_, err := db.DB().Exec(`
		UPDATE tracked SET tracked_at = ?
		 WHERE issue_id = (SELECT id FROM issues WHERE number = ?)`, at.Unix(), number)
	if err != nil {
		t.Fatal(err)
	}
}

func outcome(t *testing.T, db *store.Store, number int) store.Tracked {
	t.Helper()
	rows, err := db.DB().Query(`
		SELECT issue_id, tracked_at, assigned_at, COALESCE(pr_url, ''), closed_at, outcome, last_checked
		  FROM tracked
		 WHERE issue_id = (SELECT id FROM issues WHERE number = ?)`, number)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("issue %d is not tracked", number)
	}
	var (
		got                       store.Tracked
		trackedAt                 int64
		assigned, closed, checked *int64
	)
	if err := rows.Scan(&got.IssueID, &trackedAt, &assigned, &got.PRURL,
		&closed, &got.Outcome, &checked); err != nil {
		t.Fatal(err)
	}
	got.TrackedAt = time.Unix(trackedAt, 0).UTC()
	for _, pair := range []struct {
		src *int64
		dst *time.Time
	}{{assigned, &got.AssignedAt}, {closed, &got.ClosedAt}, {checked, &got.LastChecked}} {
		if pair.src != nil {
			*pair.dst = time.Unix(*pair.src, 0).UTC()
		}
	}
	return got
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
