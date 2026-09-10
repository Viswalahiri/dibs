package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/slack-go/slack"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/filter"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
	"github.com/Viswalahiri/dibs/internal/triage"
)

const pushBatch = 5

// KindAlert and KindWarning are the two outbox message kinds. An alert hangs
// off an issue and can be updated later; a warning is a standalone line.
const (
	KindAlert   = "alert"
	KindWarning = "warning"
)

// Message is what the outbox stores. The rendered blocks travel with it so the
// sender does no work beyond posting, and a message queued before a config
// change goes out exactly as it was composed.
type Message struct {
	IssueID int64        `json:"issue_id,omitempty"`
	Text    string       `json:"text"`
	Blocks  slack.Blocks `json:"blocks"`
}

// Pusher turns scored issues into queued Slack alerts. Between scoring and
// pushing, dibs has spent five to fifteen seconds on enrichment and a model
// call, which on a busy repository is long enough for someone to claim the
// issue. So it looks again immediately before sending.
type Pusher struct {
	client *gh.Client
	store  *store.Store
	cfg    *config.Config
	log    *slog.Logger
	owner  string
	now    func() time.Time
}

func NewPusher(c *gh.Client, s *store.Store, cfg *config.Config, log *slog.Logger) *Pusher {
	return &Pusher{
		client: c, store: s, cfg: cfg, log: log,
		owner: "pusher",
		now:   func() time.Time { return time.Now().UTC() },
	}
}

func (p *Pusher) Run(ctx context.Context) error {
	const idle = 3 * time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		n, err := p.Drain(ctx)
		if err != nil && ctx.Err() == nil {
			p.log.Error("push", "err", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if n > 0 {
			timer.Reset(0)
		} else {
			timer.Reset(idle)
		}
	}
}

func (p *Pusher) Drain(ctx context.Context) (int, error) {
	claimed, err := p.store.Claim(ctx, store.StateScored, pushBatch, p.owner,
		p.cfg.Reaper.LeaseTTL(), p.now())
	if err != nil {
		return 0, err
	}
	done := 0
	for _, iss := range claimed {
		if err := p.one(ctx, iss); err != nil {
			if errors.Is(err, store.ErrNotClaimable) {
				continue
			}
			p.log.Error("push issue", "issue_id", iss.ID, "number", iss.Number, "err", err)
			continue
		}
		done++
	}
	return done, nil
}

func (p *Pusher) one(ctx context.Context, iss store.Issue) error {
	repo, err := p.store.RepoByID(ctx, iss.RepoID)
	if err != nil {
		return err
	}

	taken, why, err := StillAvailable(ctx, p.client, repo, iss.Number, p.cfg.Profile.GitHubLogin)
	if err != nil {
		return err
	}
	if taken {
		p.log.Info("claimed before push",
			"repo", repo.Slug(), "number", iss.Number, "why", why)
		return p.store.ClaimedBeforePush(ctx, iss.ID)
	}

	var response triage.Response
	if iss.TriageJSON != "" {
		// A degraded row carries a marker rather than a verdict. Decoding it
		// fails, and an alert with no summary is still worth sending.
		if decoded, err := triage.Decode([]byte(iss.TriageJSON)); err == nil {
			response = decoded
		}
	}

	now := p.now()
	blocks := Alert(iss, repo, response, p.cfg, now)
	payload, err := json.Marshal(Message{
		IssueID: iss.ID,
		Text:    fmt.Sprintf("%d · %s #%d · %s", iss.Score.Int64, repo.Slug(), iss.Number, iss.Title),
		Blocks:  slack.Blocks{BlockSet: blocks},
	})
	if err != nil {
		return fmt.Errorf("encode alert for issue %d: %w", iss.ID, err)
	}

	if _, err := p.store.Enqueue(ctx, store.Outgoing{
		Kind:      KindAlert,
		DedupeKey: fmt.Sprintf("alert:%d", iss.ID),
		Payload:   string(payload),
	}, now); err != nil {
		return err
	}
	return p.store.Surface(ctx, iss.ID, now)
}

// StillAvailable re-reads the issue and reports whether it has been taken since
// dibs last looked. It costs a couple of requests and buys the thing that makes
// the whole system worth running: a ping means the issue was unclaimed seconds
// ago.
//
// It runs immediately before the push, and again when Track is pressed, which
// covers the minutes spent deciding.
func StillAvailable(ctx context.Context, client *gh.Client, repo store.Repo, number int, self string) (
	taken bool, why string, err error) {

	base := fmt.Sprintf("/repos/%s/%s/issues/%d",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), number)

	var current gh.Issue
	if _, _, err := client.GetJSON(ctx, base, "", &current); err != nil {
		return false, "", err
	}
	if current.IsAssigned() {
		return true, "assigned", nil
	}
	if current.State == "closed" {
		return true, "closed", nil
	}

	var comments []struct {
		Body string `json:"body"`
		User *struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if _, _, err := client.GetJSON(ctx, base+"/comments?per_page=20", "", &comments); err != nil {
		return false, "", err
	}
	for _, c := range comments {
		if c.User != nil && c.User.Login == self {
			continue
		}
		if filter.IsClaim(c.Body) {
			return true, "claimed in thread", nil
		}
	}
	return false, "", nil
}

// snoozeWindow is how long Snooze holds an issue back. Most snoozed issues get
// claimed inside the hour and die silently at the re-check, which is the point.
const snoozeWindow = time.Hour

// Resurfacer returns snoozed issues to the push queue once their hour is up.
// They go back through `scored`, so the push worker's freshness re-check runs
// again rather than needing a second copy of it here.
type Resurfacer struct {
	store *store.Store
	log   *slog.Logger
	now   func() time.Time
}

func NewResurfacer(s *store.Store, log *slog.Logger) *Resurfacer {
	return &Resurfacer{store: s, log: log, now: func() time.Time { return time.Now().UTC() }}
}

func (r *Resurfacer) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		due, err := r.store.Snoozed(ctx, r.now().Add(-snoozeWindow))
		if err != nil {
			r.log.Error("read snoozed issues", "err", err)
			continue
		}
		for _, iss := range due {
			if err := r.store.Resurface(ctx, iss.ID); err != nil && !errors.Is(err, store.ErrNotClaimable) {
				r.log.Error("resurface issue", "issue_id", iss.ID, "err", err)
				continue
			}
			r.log.Info("resurfaced", "issue_id", iss.ID, "number", iss.Number)
		}
	}
}
