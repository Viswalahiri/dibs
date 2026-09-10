package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/filter"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
	"github.com/slack-go/slack"
)

const pushBatch = 5

// KindAlert and KindWarning are the two outbox message kinds. An alert hangs
// off an issue; a warning is a standalone operational line.
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

// Pusher turns screened issues into queued Slack alerts. Between the poll and
// the push, dibs has spent a few seconds fetching the thread, which on a busy
// repository is long enough for someone to claim the issue. So it looks again
// immediately before sending.
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
	claimed, err := p.store.Claim(ctx, store.StateReady, pushBatch, p.owner,
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

	taken, why, err := StillAvailable(ctx, p.client, repo, iss.Number, p.cfg.Profile.GitHubLogin, iss.Assignees)
	if err != nil {
		return err
	}
	if taken {
		p.log.Info("claimed before push",
			"repo", repo.Slug(), "number", iss.Number, "why", why)
		return p.store.ClaimedBeforePush(ctx, iss.ID)
	}

	now := p.now()
	blocks := Alert(iss, repo, now)
	payload, err := json.Marshal(Message{
		IssueID: iss.ID,
		Text:    fmt.Sprintf("%s #%d · %s", repo.Slug(), iss.Number, iss.Title),
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
// It runs immediately before the push, which is the last moment dibs can tell
// the difference.
//
// known is the assignee list dibs recorded when it first saw the issue. Only a name
// that was not there then counts as taken. An issue assigned before dibs ever
// saw it was surfaced deliberately, because projects hand assignments out by bot
// and by round-robin, and rejecting it here would quietly undo that one stage
// later.
func StillAvailable(ctx context.Context, client *gh.Client, repo store.Repo, number int, self string, known []string) (
	taken bool, why string, err error) {

	base := fmt.Sprintf("/repos/%s/%s/issues/%d",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), number)

	var current gh.Issue
	if _, _, err := client.GetJSON(ctx, base, "", &current); err != nil {
		return false, "", err
	}
	if newlyAssigned(current.AssigneeLogins(), known, self) {
		return true, "assigned", nil
	}
	if current.State == "closed" {
		return true, "closed", nil
	}

	comments, err := client.Comments(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return false, "", err
	}
	if filter.ClaimedByOther(comments, self) {
		return true, "claimed in thread", nil
	}
	return false, "", nil
}

// newlyAssigned reports whether anyone has taken the issue since dibs recorded
// it. The operator's own name never counts: holding it is the outcome this
// system exists to produce.
func newlyAssigned(current, known []string, self string) bool {
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(self)): true}
	for _, k := range known {
		seen[strings.ToLower(strings.TrimSpace(k))] = true
	}
	for _, c := range current {
		if !seen[strings.ToLower(strings.TrimSpace(c))] {
			return true
		}
	}
	return false
}
