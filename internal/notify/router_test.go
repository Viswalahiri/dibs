package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

// TestTrackRefusesAnIssueTakenWhileDeciding covers the minutes between the
// alert landing and the button being pressed. On a busy repository that is long
// enough for somebody else to claim it, and tracking it anyway would be dibs
// telling you the race was still open when it was not.
func TestTrackRefusesAnIssueTakenWhileDeciding(t *testing.T) {
	r, db, sink, issueID := newRouter(t, githubIssue{assignee: "someone-else"})

	if err := r.Handle(context.Background(), press(ActionTrack, issueID)); err != nil {
		t.Fatal(err)
	}

	iss, err := db.IssueByID(context.Background(), issueID)
	if err != nil {
		t.Fatal(err)
	}
	if iss.State != store.StatePushed {
		t.Errorf("state is %q; a refused Track must leave the issue alone", iss.State)
	}
	if !strings.Contains(sink.only(t).text, "taken") {
		t.Errorf("the message was not replaced with a warning: %q", sink.only(t).text)
	}
}

// TestTrackRecordsAnAvailableIssue is the other half.
func TestTrackRecordsAnAvailableIssue(t *testing.T) {
	r, db, _, issueID := newRouter(t, githubIssue{})
	ctx := context.Background()

	if err := r.Handle(ctx, press(ActionTrack, issueID)); err != nil {
		t.Fatal(err)
	}

	iss, err := db.IssueByID(ctx, issueID)
	if err != nil {
		t.Fatal(err)
	}
	if iss.State != store.StateTracked {
		t.Fatalf("state is %q, want %q", iss.State, store.StateTracked)
	}
	due, err := db.TrackedDue(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Outcome != store.OutcomePending {
		t.Fatalf("the reaper has nothing to follow: %+v", due)
	}
}

// TestEveryButtonSurvivesADoubleTap is the idempotence rule. Slack will happily
// deliver the same press twice, and a second one must change nothing.
func TestEveryButtonSurvivesADoubleTap(t *testing.T) {
	cases := []struct {
		action string
		want   store.State
	}{
		{ActionTrack, store.StateTracked},
		{ActionSkip, store.StateSkipped},
		{ActionSnooze, store.StateSnoozed},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			r, db, sink, issueID := newRouter(t, githubIssue{})
			ctx := context.Background()

			for i := 0; i < 3; i++ {
				if err := r.Handle(ctx, press(tc.action, issueID)); err != nil {
					t.Fatalf("press %d: %v", i+1, err)
				}
			}

			iss, err := db.IssueByID(ctx, issueID)
			if err != nil {
				t.Fatal(err)
			}
			if iss.State != tc.want {
				t.Errorf("state is %q, want %q", iss.State, tc.want)
			}
			if n := sink.count(); n != 1 {
				t.Errorf("three presses rewrote the message %d times, want 1", n)
			}
		})
	}
}

// TestOpenRecordsNothing keeps claiming where it belongs. The link button goes
// to GitHub, and dibs deliberately learns nothing from it.
func TestOpenRecordsNothing(t *testing.T) {
	r, db, sink, issueID := newRouter(t, githubIssue{})
	ctx := context.Background()

	if err := r.Handle(ctx, press(ActionOpen, issueID)); err != nil {
		t.Fatal(err)
	}
	iss, err := db.IssueByID(ctx, issueID)
	if err != nil {
		t.Fatal(err)
	}
	if iss.State != store.StatePushed || !iss.DecidedAt.IsZero() {
		t.Errorf("Open recorded a decision: state %q, decided %v", iss.State, iss.DecidedAt)
	}
	if n := sink.count(); n != 0 {
		t.Errorf("Open produced %d messages, want none", n)
	}
}

// TestWhyReadsStoredJSON proves the breakdown comes out of the database. A
// button that re-called the model would cost money every time you were curious.
func TestWhyReadsStoredJSON(t *testing.T) {
	r, _, sink, issueID := newRouter(t, githubIssue{})

	if err := r.Handle(context.Background(), press(ActionWhy, issueID)); err != nil {
		t.Fatal(err)
	}
	got := sink.only(t)
	if !got.threaded {
		t.Error("the breakdown was not posted as a threaded reply")
	}
	for _, want := range []string{"scope_clarity", "already_taken", "repro steps"} {
		if !strings.Contains(got.text, want) {
			t.Errorf("breakdown is missing %q:\n%s", want, got.text)
		}
	}
}

// TestWhyOnADegradedRowSaysSo keeps the fallback legible. An issue scored at 50
// after a model outage has no breakdown, and pretending otherwise would read as
// a judgment dibs never made.
func TestWhyOnADegradedRowSaysSo(t *testing.T) {
	r, db, sink, issueID := newRouter(t, githubIssue{})
	if _, err := db.DB().Exec(
		`UPDATE issues SET triage_json = ? WHERE id = ?`,
		`{"degraded":true,"error":"invalid x-api-key"}`, issueID); err != nil {
		t.Fatal(err)
	}

	if err := r.Handle(context.Background(), press(ActionWhy, issueID)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sink.only(t).text, "fallback") {
		t.Errorf("a degraded row produced %q", sink.only(t).text)
	}
}

// --- fixtures ---

type githubIssue struct {
	assignee string
	state    string
	comment  string
}

func newRouter(t *testing.T, current githubIssue) (*Router, *store.Store, *conversation, int64) {
	t.Helper()
	if current.state == "" {
		current.state = "open"
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("the router made a %s request; the token is read-only", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		if strings.HasSuffix(r.URL.Path, "/comments") {
			var comments []any
			if current.comment != "" {
				comments = append(comments, map[string]any{
					"body": current.comment, "user": map[string]string{"login": "someone-else"},
				})
			}
			json.NewEncoder(w).Encode(comments)
			return
		}
		issue := map[string]any{"number": 7, "state": current.state}
		if current.assignee != "" {
			issue["assignee"] = map[string]string{"login": current.assignee}
		}
		json.NewEncoder(w).Encode(issue)
	}))
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

	id, _, err := db.Insert(ctx, store.Issue{
		RepoID: repo.ID, Number: 7, NodeID: "I_7", Title: "Drain deadlocks",
		HTMLURL: "https://github.com/acme/widget/issues/7", Author: "reporter",
		AuthorAssoc: "NONE", CreatedAt: time.Now().UTC(), FirstSeenAt: time.Now().UTC(),
		State: store.StateScored,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(
		`UPDATE issues SET state = ?, score = 82, triage_json = ? WHERE id = ?`,
		string(store.StatePushed), verdictJSON, id); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Profile: config.Profile{GitHubLogin: "test-user", EffortCeilingHours: 16},
		Polling: config.Polling{MaxConcurrent: 4},
	}
	sink := &conversation{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewRouter(db, gh.New("token", 4, gh.WithBaseURL(server.URL)), sink, cfg, log)
	return router, db, sink, id
}

const verdictJSON = `{
  "vetoes": {
    "self_fixing": {"confidence": 0.0, "evidence": ""},
    "already_taken": {"confidence": 0.1, "evidence": "nobody has commented"},
    "poorly_scoped": {"confidence": 0.0, "evidence": ""}
  },
  "dimensions": {
    "scope_clarity": {"score": 5, "why": "expected behaviour stated in the body"},
    "concreteness": {"score": 5, "why": "repro steps and a version given"},
    "blast_radius": {"score": 4, "why": "confined to the drain path"},
    "maintainer_invitation": {"score": 2, "why": "no maintainer has commented"},
    "contention_risk": {"score": 4, "why": "no reactions"}
  },
  "effort": {"low_hours": 2, "high_hours": 6, "confidence": "medium"},
  "stack": ["go"],
  "positives": ["repro steps included", "one code path"],
  "top_risk": "concurrency bugs hide in the tests"
}`

func press(action string, issueID int64) slack.InteractionCallback {
	cb := slack.InteractionCallback{}
	cb.Channel.ID = "C123"
	cb.Message.Timestamp = "1700000000.000100"
	cb.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: action, Value: strconv.FormatInt(issueID, 10)},
	}
	return cb
}

// conversation records what the router said back to Slack.
type conversation struct {
	mu   sync.Mutex
	sent []spoken
}

type spoken struct {
	text     string
	threaded bool
}

func (c *conversation) Update(_ context.Context, _, _, text string, blocks []slack.Block) error {
	c.record(text+" "+flatten(blocks), false)
	return nil
}

func (c *conversation) Reply(_ context.Context, _, _ string, blocks []slack.Block) error {
	c.record(flatten(blocks), true)
	return nil
}

func (c *conversation) record(text string, threaded bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, spoken{text: text, threaded: threaded})
}

func (c *conversation) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func (c *conversation) only(t *testing.T) spoken {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) != 1 {
		t.Fatalf("got %d messages, want exactly 1", len(c.sent))
	}
	return c.sent[0]
}

func flatten(blocks []slack.Block) string {
	var b strings.Builder
	for _, block := range blocks {
		switch v := block.(type) {
		case *slack.SectionBlock:
			if v.Text != nil {
				b.WriteString(v.Text.Text)
			}
		case *slack.ContextBlock:
			for _, el := range v.ContextElements.Elements {
				if txt, ok := el.(*slack.TextBlockObject); ok {
					b.WriteString(txt.Text)
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
