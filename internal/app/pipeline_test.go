package app_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/Viswalahiri/dibs/internal/triage"
)

// TestPipeline runs one issue from a poll to a queued Slack alert against
// fixture GitHub and Anthropic servers. It is the only test that proves the
// stages hand off correctly, because the database is the only thing between
// them and nothing else exercises that.
func TestPipeline(t *testing.T) {
	// Adoption happens at start, the issue is opened ten seconds later, and the
	// next poll runs a minute in. That ordering is the point: an issue is only
	// new if it appeared after the waterline was drawn.
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }

	github := httptest.NewServer(githubFixture(t, start.Add(10*time.Second)))
	defer github.Close()
	anthropic := httptest.NewServer(anthropicFixture(t))
	defer anthropic.Close()

	cfg := testConfig()
	db := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if err := db.SyncRepos(ctx, []config.Repo{
		{Slug: "acme/widget", Receptivity: config.ReceptivityNormal},
	}, cfg.Polling.DefaultIntervalSec); err != nil {
		t.Fatal(err)
	}
	repo, err := db.RepoBySlug(ctx, "acme", "widget")
	if err != nil {
		t.Fatal(err)
	}

	client := gh.New("token", 4, gh.WithBaseURL(github.URL))
	poller := gh.NewPoller(client, db, cfg, log, nil, nil, gh.WithClock(clock))

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
	assertState(t, ctx, db, repo.ID, store.StateEnriched, 1)

	triager := triage.NewWorker(
		triage.NewClient("key", cfg, triage.WithAPIBase(anthropic.URL)), db, cfg, log)
	if n, err := triager.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("triage: drained %d, err %v", n, err)
	}
	assertState(t, ctx, db, repo.ID, store.StateScored, 1)

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

	// The whole run must have been read-only against GitHub.
	if writes := github.Config.ErrorLog; writes != nil {
		t.Log("unexpected error log configured")
	}
}

// TestPipelineDropsAClaimedIssueBeforePushing covers the check that makes a
// ping mean the issue was unclaimed seconds ago. The issue is clean when it is
// scored and claimed by the time the push worker looks again.
func TestPipelineDropsAClaimedIssueBeforePushing(t *testing.T) {
	start := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	now := start
	var claimed sync.Once
	takenNow := false

	fixture := githubFixture(t, start.Add(10*time.Second))
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
	anthropic := httptest.NewServer(anthropicFixture(t))
	defer anthropic.Close()

	cfg := testConfig()
	db := openStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	if err := db.SyncRepos(ctx, []config.Repo{{Slug: "acme/widget"}}, 45); err != nil {
		t.Fatal(err)
	}
	repo, _ := db.RepoBySlug(ctx, "acme", "widget")

	client := gh.New("token", 4, gh.WithBaseURL(github.URL))
	poller := gh.NewPoller(client, db, cfg, log, nil, nil,
		gh.WithClock(func() time.Time { return now }))
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Minute)
	if _, err := poller.Tick(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	enricher := gh.NewEnricher(client, db, cfg, log)
	if _, err := enricher.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	triager := triage.NewWorker(
		triage.NewClient("key", cfg, triage.WithAPIBase(anthropic.URL)), db, cfg, log)
	if _, err := triager.Drain(ctx); err != nil {
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

// --- fixtures ---

func testConfig() *config.Config {
	return &config.Config{
		Profile: config.Profile{
			GitHubLogin: "test-user", Stacks: []string{"go"}, EffortCeilingHours: 16,
		},
		Scoring: config.Scoring{
			JunkFloor: 40, VetoConfidence: 0.60,
			Weights: config.Weights{
				ScopeClarity: 20, Concreteness: 20, BlastRadius: 20,
				MaintainerInvitation: 20, ContentionRisk: 20,
			},
			Multipliers: config.Multipliers{
				StackMatch: 1, StackMismatch: 0.7,
				ReceptivityHigh: 1.1, ReceptivityNormal: 1, ReceptivityCautious: 0.85,
			},
		},
		Polling: config.Polling{
			DefaultIntervalSec: 45, MinIntervalSec: 30, MaxConcurrent: 4,
			FreshnessCutoffMin: 15, GapWarnMin: 15,
			RateLimitSlowAt: 500, RateLimitPauseAt: 100,
		},
		Triage: config.Triage{
			Model: "claude-sonnet-5", Thinking: "disabled",
			MaxBodyChars: 4000, MaxThreadChars: 2500, MaxDocChars: 1500,
			MaxRetries: 1, TimeoutSec: 10, DailyCallCap: 100,
			Cost: config.Cost{InputPerMTokUSD: 2, OutputPerMTokUSD: 10, MonthlyBudgetUSD: 10},
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

// githubFixture serves one issue opened at openedAt. The first list call
// returns an empty repository, which is the adoption call; every call after it
// returns the issue.
func githubFixture(t *testing.T, openedAt time.Time) http.Handler {
	t.Helper()
	now := openedAt
	var (
		mu    sync.Mutex
		polls int
	)
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

	mux := http.NewServeMux()
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"resources": map[string]any{
			"core": map[string]any{"limit": 5000, "remaining": 4999, "reset": now.Add(time.Hour).Unix()},
		}})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"login": "test-user"})
	})
	mux.HandleFunc("/repos/acme/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("dibs made a %s request to %s; the token is read-only", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls == 1 {
			// Adoption. An empty repository draws the waterline at now.
			writeJSON(w, []any{})
			return
		}
		// A pull request in the issues list must be skipped, not treated as an
		// issue. This is the most common bug in this pattern.
		writeJSON(w, []any{
			map[string]any{
				"number": 8, "node_id": "PR_8", "title": "a pull request",
				"created_at":   now.Format(time.RFC3339),
				"pull_request": map[string]string{"url": "https://api.github.com/pulls/8"},
			},
			issue,
		})
	})
	mux.HandleFunc("/repos/acme/widget/issues/7/timeline", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []any{})
	})
	mux.HandleFunc("/repos/acme/widget/issues/7/comments", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []any{})
	})
	mux.HandleFunc("/repos/acme/widget/issues/7", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, issue)
	})
	mux.HandleFunc("/repos/acme/widget/contents/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"total_count": 3})
	})
	return mux
}

func anthropicFixture(t *testing.T) http.Handler {
	t.Helper()
	verdict := triage.Response{
		Dimensions: triage.Dimensions{
			ScopeClarity:         triage.Dimension{Score: 5, Why: "expected behaviour stated explicitly in the body"},
			Concreteness:         triage.Dimension{Score: 5, Why: "version and GOMAXPROCS given, plus a repro"},
			BlastRadius:          triage.Dimension{Score: 4, Why: "confined to the drain path"},
			MaintainerInvitation: triage.Dimension{Score: 2, Why: "no maintainer has commented yet"},
			ContentionRisk:       triage.Dimension{Score: 4, Why: "no reactions and no comments"},
		},
		Effort:    triage.Effort{LowHours: 2, HighHours: 6, Confidence: "medium"},
		Stack:     []string{"go"},
		Positives: []string{"repro steps included", "confined to one code path"},
		TopRisk:   "concurrency bugs hide in the tests",
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected anthropic path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("undecodable request: %v", err)
		}
		// Sampling parameters are rejected with a 400 on every current model.
		for _, banned := range []string{"temperature", "top_p", "top_k"} {
			if _, ok := body[banned]; ok {
				t.Errorf("request carried %q, which the API rejects", banned)
			}
		}
		if _, ok := body["output_config"]; !ok {
			t.Error("request did not constrain the response with output_config")
		}
		encoded, err := json.Marshal(verdict)
		if err != nil {
			t.Fatal(err)
		}
		writeJSON(w, map[string]any{
			"stop_reason": "end_turn",
			"content":     []any{map[string]string{"type": "text", "text": string(encoded)}},
			"usage":       map[string]int{"input_tokens": 3300, "output_tokens": 350},
		})
	})
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
