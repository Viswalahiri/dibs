package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/slack-go/slack"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
	"github.com/Viswalahiri/dibs/internal/triage"
)

// Conversation is the part of Slack the router talks back to. Slack is the
// only implementation; the interface exists so a button press can be exercised
// without a workspace, which is where the two rules that matter live: Track
// refuses an issue taken while you were deciding, and a double tap changes
// nothing.
type Conversation interface {
	Update(ctx context.Context, channel, timestamp, text string, blocks []slack.Block) error
	Reply(ctx context.Context, channel, timestamp string, blocks []slack.Block) error
}

// Router handles button presses. Every handler mutates local SQLite and
// nothing else. Dibs holds a read-only GitHub token; claiming happens in the
// browser, by hand.
//
// Every handler is idempotent. The state change is conditional on the row still
// being in `pushed`, so a double-tapped button is a no-op rather than a double
// process.
type Router struct {
	store  *store.Store
	client *gh.Client
	slack  Conversation
	cfg    *config.Config
	log    *slog.Logger
	now    func() time.Time
}

func NewRouter(s *store.Store, client *gh.Client, sl Conversation, cfg *config.Config, log *slog.Logger) *Router {
	return &Router{
		store: s, client: client, slack: sl, cfg: cfg, log: log,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (r *Router) Handle(ctx context.Context, cb slack.InteractionCallback) error {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return nil
	}
	action := cb.ActionCallback.BlockActions[0]

	// Open is a plain URL button. Slack still reports the click; dibs records
	// nothing, because that is where claiming happens.
	if action.ActionID == ActionOpen {
		return nil
	}

	issueID, err := strconv.ParseInt(action.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("unreadable action value %q: %w", action.Value, err)
	}
	iss, err := r.store.IssueByID(ctx, issueID)
	if err != nil {
		return err
	}
	repo, err := r.store.RepoByID(ctx, iss.RepoID)
	if err != nil {
		return err
	}

	switch action.ActionID {
	case ActionWhy:
		return r.why(ctx, iss, cb)
	case ActionTrack:
		return r.track(ctx, iss, repo, cb)
	case ActionSkip:
		return r.decide(ctx, iss, repo, store.StateSkipped, cb)
	case ActionSnooze:
		return r.decide(ctx, iss, repo, store.StateSnoozed, cb)
	}
	return fmt.Errorf("unknown action %q", action.ActionID)
}

// why posts the stored breakdown as a threaded reply. It reads triage_json and
// never calls the model again.
func (r *Router) why(ctx context.Context, iss store.Issue, cb slack.InteractionCallback) error {
	response, err := triage.Decode([]byte(iss.TriageJSON))
	if err != nil {
		return r.slack.Reply(ctx, cb.Channel.ID, cb.Message.Timestamp,
			Text("No breakdown stored. This issue was scored by the fallback after a model failure."))
	}
	return r.slack.Reply(ctx, cb.Channel.ID, cb.Message.Timestamp, Why(response))
}

// track re-checks before recording. Between the push and the click are the
// minutes spent deciding, which on a busy repository is enough for someone else
// to take it.
func (r *Router) track(ctx context.Context, iss store.Issue, repo store.Repo, cb slack.InteractionCallback) error {
	taken, why, err := StillAvailable(ctx, r.client, repo, iss.Number, r.cfg.Profile.GitHubLogin, iss.Assignees)
	if err != nil {
		return err
	}
	if taken {
		r.log.Info("track refused, issue taken",
			"repo", repo.Slug(), "number", iss.Number, "why", why)
		return r.slack.Update(ctx, cb.Channel.ID, cb.Message.Timestamp,
			"taken", Taken(iss, repo))
	}

	now := r.now()
	if err := r.store.Decide(ctx, iss.ID, store.StateTracked, now); err != nil {
		if errors.Is(err, store.ErrNotClaimable) {
			return nil // a double tap
		}
		return err
	}
	if err := r.store.Track(ctx, iss.ID, now); err != nil {
		return err
	}
	return r.slack.Update(ctx, cb.Channel.ID, cb.Message.Timestamp,
		"tracking", Decided(iss, repo, store.StateTracked, now))
}

// decide records a Skip or a Snooze and collapses the message. The score is
// kept either way: a skip is calibration data, and it is the only evidence M4
// has that the floor is set right.
func (r *Router) decide(ctx context.Context, iss store.Issue, repo store.Repo,
	state store.State, cb slack.InteractionCallback) error {

	now := r.now()
	if err := r.store.Decide(ctx, iss.ID, state, now); err != nil {
		if errors.Is(err, store.ErrNotClaimable) {
			return nil
		}
		return err
	}
	return r.slack.Update(ctx, cb.Channel.ID, cb.Message.Timestamp,
		string(state), Decided(iss, repo, state, now))
}
