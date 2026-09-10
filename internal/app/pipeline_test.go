package app_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/notify"
	"github.com/Viswalahiri/dibs/internal/store"
)

// TestPipeline runs one issue from a poll to a queued Slack alert against a
// fixture GitHub. It is the only test that proves the stages hand off
// correctly, because the database is the only thing between them and nothing
// else exercises that.
func TestPipeline(t *testing.T) {
	// Adoption happens at start, the issue is opened ten seconds later, and the
	// next poll runs a minute in. That ordering is the point: an issue is only
	// new if it appeared after the waterline was drawn.
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }

	fixture := newGitHub(t, start.Add(10*time.Second))
	github := httptest.NewServer(fixture)
	defer github.Close()

	cfg := testConfig()
	db := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if err := db.SyncRepos(ctx, []config.Repo{{Slug: "acme/widget"}},
		cfg.Polling.DefaultIntervalSec); err != nil {
		t.Fatal(err)
	}
	repo, err := db.RepoBySlug(ctx, "acme", "widget")
	if err != nil {
		t.Fatal(err)
	}

	client := gh.New("token", 4, gh.WithBaseURL(github.URL))
	poller := gh.NewPoller(client, db, cfg, log, nil, gh.WithClock(clock))

	// Adoption draws the waterline and produces no work.
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	counts, err := db.CountByState(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 0 {
		t.Fatalf("adoption produced work: %v", counts)
	}

	// The next tick sees the new issue.
	now = start.Add(time.Minute)
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	assertState(t, ctx, db, repo.ID, store.StateNew, 1)

	enricher := gh.NewEnricher(client, db, cfg, log)
	if n, err := enricher.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("enrich: drained %d, err %v", n, err)
	}
	assertState(t, ctx, db, repo.ID, store.StateReady, 1)

	pusher := notify.NewPusher(client, db, cfg, log)
	if n, err := pusher.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("push: drained %d, err %v", n, err)
	}
	assertState(t, ctx, db, repo.ID, store.StatePushed, 1)

	sink := &sink{}
	sender := notify.NewSender(sink, db, log)
	if n, err := sender.Flush(ctx); err != nil || n != 1 {
		t.Fatalf("flush: sent %d, err %v", n, err)
	}

	got := sink.only(t)
	for _, want := range []string{"acme/widget", "#7", "Drain deadlocks"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("alert text %q is missing %q", got.Text, want)
		}
	}
	if len(got.Blocks.BlockSet) == 0 {
		t.Error("alert carried no blocks")
	}
}

// TestEnrichmentCostsTwoRequests pins what an issue is worth to dibs. The doc
// and author-stats lookups went with the scoring, so anything that reappears
// here is a request nothing reads.
func TestEnrichmentCostsTwoRequests(t *testing.T) {
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	now := start

	fixture := newGitHub(t, start.Add(10*time.Second))
	github := httptest.NewServer(fixture)
	defer github.Close()

	cfg := testConfig()
	db := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if err := db.SyncRepos(ctx, []config.Repo{{Slug: "acme/widget"}}, 45); err != nil {
		t.Fatal(err)
	}
	repo, _ := db.RepoBySlug(ctx, "acme", "widget")

	client := gh.New("token", 4, gh.WithBaseURL(github.URL))
	poller := gh.NewPoller(client, db, cfg, log, nil,
		gh.WithClock(func() time.Time { return now }))
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Minute)
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}

	before := fixture.count()
	if _, err := gh.NewEnricher(client, db, cfg, log).Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fixture.count() - before; got != 2 {
		t.Errorf("enrichment made %d requests, want 2 (timeline and comments)", got)
	}
}

// TestPipelineRejectsAClaimedIssue proves the filter runs inside the enricher.
// Somebody saying they have taken the issue is the one signal that costs
// nothing to read and kills the issue outright.
func TestPipelineRejectsAClaimedIssue(t *testing.T) {
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	now := start

	fixture := newGitHub(t, start.Add(10*time.Second))
	fixture.comments = []any{map[string]any{
		"body":               "taking this",
		"created_at":         start.Format(time.RFC3339),
		"author_association": "NONE",
		"user":               map[string]string{"login": "someone-else"},
	}}
	github := httptest.NewServer(fixture)
	defer github.Close()

	cfg := testConfig()
	db := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if err := db.SyncRepos(ctx, []config.Repo{{Slug: "acme/widget"}}, 45); err != nil {
		t.Fatal(err)
	}
	repo, _ := db.RepoBySlug(ctx, "acme", "widget")

	client := gh.New("token", 4, gh.WithBaseURL(github.URL))
	poller := gh.NewPoller(client, db, cfg, log, nil,
		gh.WithClock(func() time.Time { return now }))
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Minute)
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gh.NewEnricher(client, db, cfg, log).Drain(ctx); err != nil {
		t.Fatal(err)
	}
	assertState(t, ctx, db, repo.ID, store.StateRejected, 1)

	sink := &sink{}
	if n, err := notify.NewSender(sink, db, log).Flush(ctx); err != nil || n != 0 {
		t.Fatalf("a claimed issue produced %d messages, err %v", n, err)
	}
}

// TestPipelineDropsAClaimedIssueBeforePushing covers the check that makes a
// ping mean the issue was unclaimed seconds ago. The issue is clean when it is
// screened and claimed by the time the push worker looks again.
func TestPipelineDropsAClaimedIssueBeforePushing(t *testing.T) {
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	now := start
	var claimed sync.Once
	takenNow := false

	fixture := newGitHub(t, start.Add(10*time.Second))
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The single-issue fetch is only made by the push worker's re-check.
		if r.URL.Path == "/repos/acme/widget/issues/7" {
			claimed.Do(func() { takenNow = true })
			writeJSON(w, map[string]any{
				"number": 7, "state": "open",
				"assignee": map[string]string{"login": "someone-else"},
			})
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	defer github.Close()

	cfg := testConfig()
	db := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if err := db.SyncRepos(ctx, []config.Repo{{Slug: "acme/widget"}}, 45); err != nil {
		t.Fatal(err)
	}
	repo, _ := db.RepoBySlug(ctx, "acme", "widget")

	client := gh.New("token", 4, gh.WithBaseURL(github.URL))
	poller := gh.NewPoller(client, db, cfg, log, nil,
		gh.WithClock(func() time.Time { return now }))
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Minute)
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gh.NewEnricher(client, db, cfg, log).Drain(ctx); err != nil {
		t.Fatal(err)
	}

	pusher := notify.NewPusher(client, db, cfg, log)
	if _, err := pusher.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if !takenNow {
		t.Fatal("the push worker never re-checked the issue")
	}
	assertState(t, ctx, db, repo.ID, store.StateClaimedBeforePush, 1)

	sink := &sink{}
	sender := notify.NewSender(sink, db, log)
	if n, err := sender.Flush(ctx); err != nil || n != 0 {
		t.Fatalf("a claimed issue produced %d messages, err %v", n, err)
	}
}

// TestACrashedWorkerLosesNoRow is the acceptance sentence for running on a
// laptop: a worker killed mid-enrichment leaves a leased row behind, and the
// issue completes on the next lease cycle rather than disappearing.
func TestACrashedWorkerLosesNoRow(t *testing.T) {
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	now := start

	github := httptest.NewServer(newGitHub(t, start.Add(10*time.Second)))
	defer github.Close()

	cfg := testConfig()
	db := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if err := db.SyncRepos(ctx, []config.Repo{{Slug: "acme/widget"}}, 45); err != nil {
		t.Fatal(err)
	}
	repo, err := db.RepoBySlug(ctx, "acme", "widget")
	if err != nil {
		t.Fatal(err)
	}

	client := gh.New("token", 4, gh.WithBaseURL(github.URL))
	poller := gh.NewPoller(client, db, cfg, log, nil,
		gh.WithClock(func() time.Time { return now }))
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Minute)
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	assertState(t, ctx, db, repo.ID, store.StateNew, 1)

	// The worker that took this row is gone. Nothing wrote the verdict, and
	// nothing will: only the lease it left behind says the row was ever taken.
	dead := time.Now().UTC().Add(-2 * time.Hour)
	claimed, err := db.Claim(ctx, store.StateNew, 5, "worker-that-died", cfg.Reaper.LeaseTTL(), dead)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claimed %d rows, err %v", len(claimed), err)
	}
	assertState(t, ctx, db, repo.ID, store.StateNew, 1)

	// A reaper pass finds the abandoned lease. The row is claimable either way,
	// but this is the path a restart actually takes.
	if n, err := db.ReleaseExpiredLeases(ctx, time.Now().UTC()); err != nil || n != 1 {
		t.Fatalf("released %d leases, err %v", n, err)
	}

	if n, err := gh.NewEnricher(client, db, cfg, log).Drain(ctx); err != nil || n != 1 {
		t.Fatalf("the replacement worker enriched %d rows, err %v", n, err)
	}
	assertState(t, ctx, db, repo.ID, store.StateReady, 1)
}

// --- fixtures ---

func testConfig() *config.Config {
	return &config.Config{
		Profile: config.Profile{GitHubLogin: "test-user"},
		Polling: config.Polling{
			DefaultIntervalSec: 45, MinIntervalSec: 30, MaxConcurrent: 4,
			FreshnessCutoffMin: 15, GapWarnMin: 15,
			RateLimitSlowAt: 500, RateLimitPauseAt: 100,
		},
		Slack:  config.Slack{DeliverTo: "dm"},
		Reaper: config.Reaper{ExpireAfterDays: 14, CadenceRecomputeHour: 3, LeaseTTLSec: 300},
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// githubStub serves one issue opened at openedAt. The first list call returns
// an empty repository, which is the adoption call; every call after it returns
// the issue. Any path it does not know is a failure, which is what keeps a
// request nothing reads from creeping back in.
type githubStub struct {
	mux      *http.ServeMux
	comments []any

	mu       sync.Mutex
	polls    int
	requests int
}

func newGitHub(t *testing.T, openedAt time.Time) *githubStub {
	t.Helper()
	s := &githubStub{mux: http.NewServeMux(), comments: []any{}}

	issue := map[string]any{
		"number": 7, "node_id": "I_7", "state": "open",
		"title": "Drain deadlocks when called twice",
		"body": "Calling Drain concurrently deadlocks on the second call. " +
			"Reproduced on v1.4.2 with GOMAXPROCS=8, expected the second call to be a no-op.",
		"html_url":           "https://github.com/acme/widget/issues/7",
		"user":               map[string]string{"login": "reporter"},
		"comments":           0,
		"created_at":         openedAt.Format(time.RFC3339),
		"updated_at":         openedAt.Format(time.RFC3339),
		"author_association": "NONE",
		"labels":             []map[string]string{{"name": "bug"}},
	}

	s.mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"resources": map[string]any{
			"core": map[string]any{
				"limit": 5000, "remaining": 4999, "reset": openedAt.Add(time.Hour).Unix(),
			},
		}})
	})
	s.mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"login": "test-user"})
	})
	s.mux.HandleFunc("/repos/acme/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.polls++
		if s.polls == 1 {
			// Adoption. An empty repository draws the waterline at now.
			writeJSON(w, []any{})
			return
		}
		// A pull request in the issues list must be skipped, not treated as an
		// issue. This is the most common bug in this pattern.
		writeJSON(w, []any{
			map[string]any{
				"number": 8, "node_id": "PR_8", "title": "a pull request",
				"created_at":   openedAt.Format(time.RFC3339),
				"pull_request": map[string]string{"url": "https://api.github.com/pulls/8"},
			},
			issue,
		})
	})
	s.mux.HandleFunc("/repos/acme/widget/issues/7/timeline", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []any{})
	})
	s.mux.HandleFunc("/repos/acme/widget/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.comments)
	})
	s.mux.HandleFunc("/repos/acme/widget/issues/7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, issue)
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return s
}

func (s *githubStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	s.mux.ServeHTTP(w, r)
}

func (s *githubStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Remaining", "4999")
	w.Header().Set("X-RateLimit-Limit", "5000")
	json.NewEncoder(w).Encode(v)
}

type sink struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (s *sink) Post(_ context.Context, m notify.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

func (s *sink) only(t *testing.T) notify.Message {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) != 1 {
		t.Fatalf("got %d messages, want exactly 1", len(s.sent))
	}
	return s.sent[0]
}

func assertState(t *testing.T, ctx context.Context, db *store.Store, repoID int64, want store.State, n int) {
	t.Helper()
	counts, err := db.CountByState(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[want] != n {
		t.Fatalf("want %d issues in %s, got %v", n, want, counts)
	}
}
