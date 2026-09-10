package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

// TestIngestWalksHistoryAndStopsAtTheCutoff covers the only part of backfill
// that is not a straight line: it is the one command in dibs that paginates,
// and it has to stop on its own rather than walking a repository's entire
// history.
func TestIngestWalksHistoryAndStopsAtTheCutoff(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-48 * time.Hour)

	// Two pages. Page two ends with an issue older than the cutoff, and page
	// three exists but must never be requested.
	pages := map[string][]any{
		"1": {
			issueJSON(101, now.Add(-1*time.Hour)),
			pullRequestJSON(102, now.Add(-2*time.Hour)),
			issueJSON(103, now.Add(-3*time.Hour)),
		},
		"2": {
			issueJSON(104, now.Add(-40*time.Hour)),
			issueJSON(105, now.Add(-60*time.Hour)), // past the cutoff, ends the walk
			issueJSON(106, now.Add(-70*time.Hour)),
		},
		"3": {issueJSON(107, now.Add(-90*time.Hour))},
	}

	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("backfill made a %s request; the token is read-only", r.Method)
		}
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		requested = append(requested, page)
		if state := r.URL.Query().Get("state"); state != "all" {
			t.Errorf("listed state=%q; a corpus of open-only issues is a corpus of what nobody wanted", state)
		}
		if next, ok := map[string]string{"1": "2", "2": "3"}[page]; ok {
			// GitHub echoes the whole query string back in Link, so following
			// it keeps state=all rather than silently falling back to the
			// endpoint's open-only default.
			w.Header().Set("Link", fmt.Sprintf(
				`<http://%s/repos/acme/widget/issues?state=all&sort=created&direction=desc&per_page=100&page=%s>; rel="next"`,
				r.Host, next))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		json.NewEncoder(w).Encode(pages[page])
	}))
	defer server.Close()

	db, repo := backfillStore(t)
	client := gh.New("token", 4, gh.WithBaseURL(server.URL))
	ctx := context.Background()

	n, err := ingest(ctx, client, db, repo, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("ingested %d issues, want 3 (the pull request and everything past the cutoff excluded)", n)
	}
	if len(requested) != 2 {
		t.Errorf("requested pages %v; the walk should have stopped after page 2", requested)
	}

	counts, err := db.CountByState(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[store.StateBackfilled] != 3 || len(counts) != 1 {
		t.Fatalf("backfill put issues somewhere a worker could claim them: %v", counts)
	}

	// Terminal by construction is what keeps a backfilled issue off Slack.
	if store.StateBackfilled.Claimable() {
		t.Error("backfilled rows are claimable, so the push worker could surface one")
	}

	// A second run over the same window adds nothing.
	again, err := ingest(ctx, client, db, repo, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("re-running backfill ingested %d issues again, want 0", again)
	}
}

func backfillStore(t *testing.T) (*store.Store, store.Repo) {
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
	return db, repo
}

func issueJSON(number int, created time.Time) map[string]any {
	return map[string]any{
		"number": number, "node_id": fmt.Sprintf("I_%d", number),
		"state": "closed", "title": fmt.Sprintf("issue %d", number),
		"body":     "a body long enough to survive the thinness check in the filter stage",
		"html_url": fmt.Sprintf("https://github.com/acme/widget/issues/%d", number),
		"user":     map[string]string{"login": "reporter"},
		"comments": 0, "author_association": "NONE",
		"created_at": created.Format(time.RFC3339),
		"updated_at": created.Format(time.RFC3339),
	}
}

func pullRequestJSON(number int, created time.Time) map[string]any {
	item := issueJSON(number, created)
	item["pull_request"] = map[string]string{"url": "https://api.github.com/pulls/1"}
	return item
}
