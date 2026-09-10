package gh

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
)

// fakeGitHub serves a programmable issue list and records every request, so a
// test can assert not just what the poller did but what it spent.
type fakeGitHub struct {
	mu        sync.Mutex
	items     []Issue
	etag      string
	requests  []string
	remaining int
	resetAt   time.Time
}

// budget makes the fake report a given rate-limit state, so tests drive the
// poller through the same response headers production does.
func (f *fakeGitHub) budget(remaining int, resetAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remaining, f.resetAt = remaining, resetAt
}

func (f *fakeGitHub) set(etag string, items ...Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.etag, f.items = etag, items
}

func (f *fakeGitHub) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.RequestURI())
	items, etag := f.items, f.etag
	remaining, resetAt := f.remaining, f.resetAt
	f.mu.Unlock()

	// Real GitHub bills /search to its own bucket and names it in
	// X-RateLimit-Resource. Thirty a minute, against core's five thousand
	// an hour.
	if strings.HasPrefix(r.URL.Path, "/search/") {
		w.Header().Set("X-RateLimit-Resource", "search")
		w.Header().Set("X-RateLimit-Limit", "30")
		w.Header().Set("X-RateLimit-Remaining", "28")
		w.Header().Set("X-RateLimit-Reset",
			strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
		w.Write([]byte(`{"total_count":3}`))
		return
	}

	w.Header().Set("X-RateLimit-Resource", "core")
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

	if etag != "" {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	body, _ := json.Marshal(items)
	w.Write(body)
}

type harness struct {
	poller *Poller
	store  *store.Store
	gh     *fakeGitHub
	repo   store.Repo
	sunk   []store.Issue
	warns  []string
	now    time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg, err := config.Load(writeConfig(t))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := st.SyncRepos(ctx, []config.Repo{{Slug: "acme/widget"}}, cfg.Polling.DefaultIntervalSec); err != nil {
		t.Fatal(err)
	}
	repo, err := st.RepoBySlug(ctx, "acme", "widget")
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeGitHub{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	h := &harness{store: st, gh: fake, repo: repo, now: time.Unix(1_700_000_000, 0).UTC()}
	fake.budget(4900, h.now.Add(time.Hour))
	client := New("t", 4, WithBaseURL(srv.URL), WithSleep(func(context.Context, time.Duration) error { return nil }))
	h.poller = NewPoller(client, st, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(_ store.Repo, iss store.Issue) { h.sunk = append(h.sunk, iss) },
		func(kind, msg string) { h.warns = append(h.warns, kind+": "+msg) },
		WithClock(func() time.Time { return h.now }),
	)
	return h
}

func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dibs.yaml")
	if err := os.WriteFile(path, []byte("profile:\n  github_login: test-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (h *harness) tick(t *testing.T) {
	t.Helper()
	if _, err := h.poller.Tick(context.Background(), h.repo.ID); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func (h *harness) reload(t *testing.T) {
	t.Helper()
	repo, err := h.store.RepoByID(context.Background(), h.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.repo = repo
}

func (h *harness) states(t *testing.T) map[store.State]int {
	t.Helper()
	counts, err := h.store.CountByState(context.Background(), h.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func issue(number int, created time.Time) Issue {
	return Issue{
		Number: number, NodeID: "n", Title: "issue", HTMLURL: "https://example.invalid",
		State: "open", CreatedAt: created, AuthorAssociation: "NONE",
		User: &User{Login: "someone"},
	}
}

func pullRequest(number int, created time.Time) Issue {
	i := issue(number, created)
	i.PullRequest = &PullRequestRef{URL: "https://example.invalid/pull"}
	return i
}

// Adoption is the guard that keeps a newly added repository from arriving as a
// pile of work. It must produce baseline rows and nothing else.
func TestAdoptionBaselinesEverythingAndSpendsNothing(t *testing.T) {
	h := newHarness(t)
	newest := h.now.Add(-2 * time.Hour)
	h.gh.set(`W/"1"`,
		issue(3, newest),
		issue(2, h.now.Add(-3*time.Hour)),
		issue(1, h.now.Add(-4*time.Hour)),
	)

	h.tick(t)
	h.reload(t)

	if got := h.states(t); got[store.StateBaseline] != 3 || got[store.StateNew] != 0 {
		t.Errorf("states = %v, want 3 baseline and 0 new", got)
	}
	if len(h.sunk) != 0 {
		t.Errorf("adoption emitted %d issues; it must emit none", len(h.sunk))
	}
	if !h.repo.Adopted() {
		t.Error("adopted_at was not set")
	}
	if !h.repo.WaterlineAt.Equal(newest) {
		t.Errorf("waterline = %v, want the newest open issue at %v", h.repo.WaterlineAt, newest)
	}
	if calls := h.gh.calls(); len(calls) != 1 {
		t.Errorf("adoption made %d requests, want exactly 1: %v", len(calls), calls)
	}
}

func TestAdoptionOfEmptyRepoStartsAtNow(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`)
	h.tick(t)
	h.reload(t)
	if !h.repo.WaterlineAt.Equal(h.now) {
		t.Errorf("waterline = %v, want now (%v) for a repo with no open issues", h.repo.WaterlineAt, h.now)
	}
}

func TestAdoptionSkipsPullRequests(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, pullRequest(9, h.now.Add(-time.Hour)), issue(8, h.now.Add(-2*time.Hour)))
	h.tick(t)
	if got := h.states(t); got[store.StateBaseline] != 1 {
		t.Errorf("baseline rows = %d, want 1; the pull request should not be recorded", got[store.StateBaseline])
	}
}

func TestNewIssueAfterAdoptionIsEmittedOnce(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-2*time.Hour)))
	h.tick(t) // adoption
	h.reload(t)

	h.now = h.now.Add(time.Minute)
	h.gh.set(`W/"2"`, issue(2, h.now.Add(-30*time.Second)), issue(1, h.now.Add(-2*time.Hour)))
	h.tick(t)
	h.reload(t)

	if len(h.sunk) != 1 || h.sunk[0].Number != 2 {
		t.Fatalf("emitted %+v, want just issue 2", h.sunk)
	}
	if got := h.states(t); got[store.StateNew] != 1 {
		t.Errorf("states = %v, want 1 new", got)
	}

	// Polling again with the same page must not emit it a second time.
	h.now = h.now.Add(time.Minute)
	h.gh.set(`W/"3"`, issue(2, h.now.Add(-90*time.Second)), issue(1, h.now.Add(-2*time.Hour)))
	h.tick(t)
	if len(h.sunk) != 1 {
		t.Errorf("issue 2 was emitted %d times, want once", len(h.sunk))
	}
}

// The whole point of the cutoff: an issue that is already old when dibs first
// sees it is recorded and dropped, without enrichment or a model call.
func TestStaleIssueIsAgedOutAndNeverEmitted(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-3*time.Hour)))
	h.tick(t) // adoption
	h.reload(t)

	h.now = h.now.Add(time.Minute)
	h.gh.set(`W/"2"`,
		issue(3, h.now.Add(-20*time.Minute)), // past the 15 minute cutoff
		issue(2, h.now.Add(-1*time.Minute)),  // fresh
	)
	h.tick(t)

	counts := h.states(t)
	if counts[store.StateAgedOut] != 1 {
		t.Errorf("aged_out rows = %d, want 1", counts[store.StateAgedOut])
	}
	if counts[store.StateNew] != 1 {
		t.Errorf("new rows = %d, want 1", counts[store.StateNew])
	}
	if len(h.sunk) != 1 || h.sunk[0].Number != 2 {
		t.Fatalf("emitted %+v, want only the fresh issue 2", h.sunk)
	}
}

func TestPullRequestsAreNeverTreatedAsIssues(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-3*time.Hour)))
	h.tick(t)
	h.reload(t)

	h.now = h.now.Add(time.Minute)
	h.gh.set(`W/"2"`, pullRequest(2, h.now.Add(-time.Minute)))
	h.tick(t)

	if len(h.sunk) != 0 {
		t.Errorf("a pull request was emitted as an issue: %+v", h.sunk)
	}
	if got := h.states(t); got[store.StateNew] != 0 {
		t.Errorf("states = %v, want no new rows", got)
	}
}

// Regression: an issue created in the same second the waterline was drawn must
// still be seen. Comparing with <= instead of < silently loses it.
func TestIssueCreatedOnTheWaterlineSecondIsNotLost(t *testing.T) {
	h := newHarness(t)
	waterline := h.now.Add(-2 * time.Hour)
	h.gh.set(`W/"1"`, issue(1, waterline))
	h.tick(t) // adoption draws the waterline at issue 1's creation time
	h.reload(t)
	if !h.repo.WaterlineAt.Equal(waterline) {
		t.Fatalf("waterline = %v, want %v", h.repo.WaterlineAt, waterline)
	}

	// Issue 2 was opened in the very same second and has never been seen.
	h.now = waterline.Add(30 * time.Second)
	h.gh.set(`W/"2"`, issue(2, waterline), issue(1, waterline))
	h.tick(t)

	if len(h.sunk) != 1 || h.sunk[0].Number != 2 {
		t.Fatalf("emitted %+v, want issue 2; it shares a second with the waterline", h.sunk)
	}
}

func TestNotModifiedCostsNothingAndKeepsState(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"stable"`, issue(1, h.now.Add(-3*time.Hour)))
	h.tick(t) // adoption
	h.reload(t)

	h.now = h.now.Add(time.Minute)
	h.tick(t) // first poll, stores the etag
	h.reload(t)
	if h.repo.ETag != `W/"stable"` {
		t.Fatalf("etag = %q, want it stored after the first poll", h.repo.ETag)
	}

	before := h.repo.WaterlineAt
	h.now = h.now.Add(time.Minute)
	h.tick(t) // second poll, served as 304
	h.reload(t)

	if len(h.sunk) != 0 {
		t.Errorf("a 304 emitted %d issues", len(h.sunk))
	}
	if !h.repo.WaterlineAt.Equal(before) {
		t.Errorf("waterline moved on a 304: %v -> %v", before, h.repo.WaterlineAt)
	}
	if !h.repo.LastPolledAt.Equal(h.now) {
		t.Errorf("last_polled_at = %v, want %v; a 304 is still a successful poll", h.repo.LastPolledAt, h.now)
	}

	calls := h.gh.calls()
	last := calls[len(calls)-1]
	if !strings.Contains(last, "per_page=30") {
		t.Errorf("poll request = %q, want the 30-item list page", last)
	}
}

func TestPollNeverPaginates(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-3*time.Hour)))
	h.tick(t)
	h.reload(t)
	h.now = h.now.Add(time.Minute)
	h.tick(t)

	for _, c := range h.gh.calls() {
		if strings.Contains(c, "page=2") || strings.Contains(c, "since=") {
			t.Errorf("request %q paginates or backfills; neither should ever happen", c)
		}
	}
}

func TestGapWarningFiresWithoutBackfilling(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-3*time.Hour)))
	h.tick(t) // adoption sets last_polled_at
	h.reload(t)

	callsAfterAdoption := len(h.gh.calls())

	// Simulate a closed lid: an hour passes with no polls.
	h.now = h.now.Add(time.Hour)
	h.gh.set(`W/"2"`, issue(2, h.now.Add(-45*time.Minute)))
	h.tick(t)

	if len(h.warns) != 1 || !strings.HasPrefix(h.warns[0], "poll_gap: ") {
		t.Fatalf("warnings = %v, want one poll_gap warning", h.warns)
	}
	if got := len(h.gh.calls()) - callsAfterAdoption; got != 1 {
		t.Errorf("recovery made %d requests, want 1; there is no backfill", got)
	}
	// Everything that appeared during the gap is stale by definition.
	if got := h.states(t); got[store.StateAgedOut] != 1 || got[store.StateNew] != 0 {
		t.Errorf("states = %v, want the gap issue aged out, not queued", got)
	}
}

func TestRateLimitPauseSkipsTheRequestEntirely(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-3*time.Hour)))
	reset := h.now.Add(20 * time.Minute)
	h.gh.budget(50, reset) // below pause_at (100)

	h.tick(t) // adoption; the response records the drained budget
	h.reload(t)
	before := len(h.gh.calls())

	wait, err := h.poller.Tick(context.Background(), h.repo.ID)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := len(h.gh.calls()) - before; got != 0 {
		t.Errorf("paused poller made %d requests, want 0", got)
	}
	if wait < 19*time.Minute {
		t.Errorf("wait = %v, want it to cover the reset window", wait)
	}
	if len(h.warns) != 1 || !strings.HasPrefix(h.warns[0], "rate_limit: ") {
		t.Fatalf("warnings = %v, want one rate_limit warning", h.warns)
	}

	// A second paused tick in the same window must stay quiet.
	if _, err := h.poller.Tick(context.Background(), h.repo.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.warns) != 1 {
		t.Errorf("warnings = %v, want the rate-limit warning deduplicated per reset window", h.warns)
	}
}

// A paused poller issues no requests, so nothing can refresh its budget. Once
// the reset time passes, the stale reading has to be discarded or the pause
// would never lift.
func TestPauseLiftsOnceTheResetTimePasses(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-3*time.Hour)))
	reset := h.now.Add(10 * time.Minute)
	h.gh.budget(10, reset)

	h.tick(t) // adoption records the drained budget
	h.reload(t)
	before := len(h.gh.calls())

	if _, err := h.poller.Tick(context.Background(), h.repo.ID); err != nil {
		t.Fatal(err)
	}
	if got := len(h.gh.calls()) - before; got != 0 {
		t.Fatalf("expected the poller to be paused, but it made %d requests", got)
	}

	h.now = reset.Add(time.Second)
	h.gh.budget(5000, h.now.Add(time.Hour))
	if _, err := h.poller.Tick(context.Background(), h.repo.ID); err != nil {
		t.Fatal(err)
	}
	if got := len(h.gh.calls()) - before; got != 1 {
		t.Errorf("poller made %d requests after the reset passed, want 1", got)
	}
}

func TestLowBudgetDoublesTheIntervalWithoutCompounding(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-3*time.Hour)))
	h.gh.budget(200, h.now.Add(time.Hour)) // below slow_at (500), above pause_at (100)

	h.tick(t) // adoption records the low budget
	h.reload(t)

	first, err := h.poller.Tick(context.Background(), h.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.poller.Tick(context.Background(), h.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first != 90*time.Second {
		t.Errorf("interval = %v, want 90s (45s doubled)", first)
	}
	if second != first {
		t.Errorf("interval compounded across ticks: %v then %v", first, second)
	}

	h.gh.budget(4900, h.now.Add(time.Hour))
	if _, err := h.poller.Tick(context.Background(), h.repo.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := h.poller.Tick(context.Background(), h.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 45*time.Second {
		t.Errorf("interval = %v, want the normal 45s once the budget recovers", restored)
	}
}

// The enricher's author-stats lookup hits /search, which answers with its own
// bucket: twenty-eight of thirty left, resetting within the minute. That
// reading used to land in the core budget and pause a poller on a full tank.
func TestSearchBucketDoesNotPauseThePoller(t *testing.T) {
	h := newHarness(t)
	h.gh.set(`W/"1"`, issue(1, h.now.Add(-3*time.Hour)))
	h.gh.budget(4900, h.now.Add(30*time.Minute))

	h.tick(t) // adoption records a healthy core budget
	h.reload(t)

	var res searchResult
	if _, _, err := h.poller.client.GetJSON(
		context.Background(), "/search/issues?per_page=1&q=x", "", &res); err != nil {
		t.Fatalf("search: %v", err)
	}

	before := len(h.gh.calls())
	if _, err := h.poller.Tick(context.Background(), h.repo.ID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := len(h.gh.calls()) - before; got != 1 {
		t.Errorf("poller made %d requests after a search response, want 1", got)
	}
	if len(h.warns) != 0 {
		t.Errorf("warnings = %v, want none; the core budget was never spent", h.warns)
	}
}
