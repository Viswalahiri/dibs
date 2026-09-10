package gh

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
)

// supervisorInterval is how often Run re-reads the enabled repository set.
// SIGHUP writes the new config to the database and the supervisor picks it up
// here, so there is no signal plumbing between the two.
const supervisorInterval = 30 * time.Second

// listPageSize is deliberately small. Nothing in dibs paginates: anything
// beyond the newest page in a 45-second window is either already known or
// already stale.
const listPageSize = 30

// adoptPageSize is one full page. Only the newest open issues matter, because
// the waterline drawn from them hides everything older regardless.
const adoptPageSize = 100

// Warner reports operational conditions that eventually become Slack messages.
type Warner func(kind, message string)

type Poller struct {
	client *Client
	store  *store.Store
	cfg    *config.Config
	log    *slog.Logger
	warn   Warner

	// now is injectable so tests can place issues either side of the freshness
	// cutoff without sleeping.
	now func() time.Time

	// warnedResets remembers which rate-limit reset windows have already
	// produced a warning, so a paused poller stays quiet.
	warnedResets sync.Map
}

type PollerOption func(*Poller)

func WithClock(f func() time.Time) PollerOption { return func(p *Poller) { p.now = f } }

func NewPoller(c *Client, s *store.Store, cfg *config.Config, log *slog.Logger, warn Warner, opts ...PollerOption) *Poller {
	p := &Poller{
		client: c, store: s, cfg: cfg, log: log,
		warn: warn,
		now:  func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Run supervises one goroutine per enabled repository until ctx is cancelled.
// It re-reads the repository set periodically, so repositories added or
// removed by a SIGHUP reload start and stop without a restart.
func (p *Poller) Run(ctx context.Context) error {
	running := map[int64]context.CancelFunc{}
	var wg sync.WaitGroup

	defer func() {
		for _, cancel := range running {
			cancel()
		}
		wg.Wait()
	}()

	ticker := time.NewTicker(supervisorInterval)
	defer ticker.Stop()

	for {
		repos, err := p.store.EnabledRepos(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			p.log.Error("read enabled repos", "err", err)
		} else {
			enabled := make(map[int64]bool, len(repos))
			for _, repo := range repos {
				enabled[repo.ID] = true
				if _, ok := running[repo.ID]; ok {
					continue
				}
				repoCtx, cancel := context.WithCancel(ctx)
				running[repo.ID] = cancel
				wg.Add(1)
				go func(id int64) {
					defer wg.Done()
					p.runRepo(repoCtx, id)
				}(repo.ID)
			}
			for id, cancel := range running {
				if !enabled[id] {
					cancel()
					delete(running, id)
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// runRepo polls one repository until its context is cancelled. The interval is
// re-read from the database each round so a cadence change by the reaper takes
// effect without a restart.
func (p *Poller) runRepo(ctx context.Context, repoID int64) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		wait, err := p.Tick(ctx, repoID)
		if err != nil && ctx.Err() == nil {
			p.log.Error("poll tick", "repo_id", repoID, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
		timer.Reset(wait)
	}
}

// Tick runs one poll for one repository and returns how long to wait before
// the next. It adopts the repository first if adoption has not run.
func (p *Poller) Tick(ctx context.Context, repoID int64) (time.Duration, error) {
	repo, err := p.store.RepoByID(ctx, repoID)
	if err != nil {
		return p.cfg.Polling.DefaultInterval(), err
	}

	interval := repo.PollInterval()
	if interval < p.cfg.Polling.MinInterval() {
		interval = p.cfg.Polling.MinInterval()
	}

	// The budget is only ever refreshed by a response, so a reading from before
	// its own reset time is stale and must not hold the poller back. Without
	// this, a pause would never lift: a paused poller makes no requests, and no
	// requests means no fresher reading.
	remaining, resetAt := p.client.Budget()
	if !resetAt.IsZero() && !p.now().Before(resetAt) {
		remaining = -1
	}

	switch {
	case remaining >= 0 && remaining < p.cfg.Polling.RateLimitPauseAt:
		p.warnRateLimit(resetAt, remaining)
		// Wait out the window rather than spending the last of the budget on
		// polls that would be rejected anyway.
		return p.pauseFor(resetAt, interval), nil
	case remaining >= 0 && remaining < p.cfg.Polling.RateLimitSlowAt:
		// Doubled per tick rather than persisted, so a recovered budget
		// restores the normal cadence immediately instead of compounding.
		interval *= 2
	}

	if !repo.Adopted() {
		if err := p.adopt(ctx, repo); err != nil {
			return interval, fmt.Errorf("adopt %s: %w", repo.Slug(), err)
		}
		return interval, nil
	}

	p.warnIfGap(repo)

	if err := p.poll(ctx, repo); err != nil {
		return interval, fmt.Errorf("poll %s: %w", repo.Slug(), err)
	}
	return interval, nil
}

// adopt records the repository's currently open issues as baseline rows and
// draws the waterline at the newest of them. Nothing is enriched, scored, or
// surfaced, so adding a repository costs zero enrichment requests and zero
// model calls.
func (p *Poller) adopt(ctx context.Context, repo store.Repo) error {
	now := p.now()
	path := fmt.Sprintf("/repos/%s/%s/issues?state=open&sort=created&direction=desc&per_page=%d",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), adoptPageSize)

	var items []Issue
	if _, _, err := p.client.GetJSON(ctx, path, "", &items); err != nil {
		return err
	}

	waterline := time.Time{}
	baselined := 0
	for _, item := range items {
		if item.IsPullRequest() {
			continue
		}
		if item.CreatedAt.After(waterline) {
			waterline = item.CreatedAt
		}
		if _, _, err := p.store.Insert(ctx, toStoreIssue(repo, item, now, store.StateBaseline)); err != nil {
			return err
		}
		baselined++
	}
	// A repository with no open issues starts from now, so its next new issue
	// is genuinely its next one.
	if waterline.IsZero() {
		waterline = now
	}

	if err := p.store.MarkAdopted(ctx, repo.ID, waterline, now); err != nil {
		return err
	}
	p.log.Info("adopted repository",
		"repo", repo.Slug(), "baseline_issues", baselined, "waterline", waterline.UTC())
	return nil
}

func (p *Poller) poll(ctx context.Context, repo store.Repo) error {
	now := p.now()
	path := fmt.Sprintf("/repos/%s/%s/issues?state=open&sort=created&direction=desc&per_page=%d",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), listPageSize)

	var items []Issue
	resp, notModified, err := p.client.GetJSON(ctx, path, repo.ETag, &items)
	if err != nil {
		return err
	}
	if notModified {
		// Nothing changed and no quota was spent. This is the common case and
		// the reason the cadence is affordable.
		return p.store.RecordPoll(ctx, repo.ID, repo.ETag, now, repo.WaterlineAt)
	}

	waterline := repo.WaterlineAt
	cutoff := p.cfg.Polling.FreshnessCutoff()

	for _, item := range items {
		// The issues endpoint returns pull requests too. Treating one as an
		// issue is the most common bug in this pattern.
		if item.IsPullRequest() {
			continue
		}
		// Strictly before, not at or before. An issue created in the same
		// second the waterline was drawn falls through to the insert, where the
		// (repo, number) constraint decides whether it is genuinely new.
		if item.CreatedAt.Before(repo.WaterlineAt) {
			continue
		}
		if item.CreatedAt.After(waterline) {
			waterline = item.CreatedAt
		}

		state := store.StateNew
		aged := now.Sub(item.CreatedAt) > cutoff
		if aged {
			// Recorded so it is never reconsidered, and dropped before any
			// enrichment request or model call.
			state = store.StateAgedOut
		}

		_, inserted, err := p.store.Insert(ctx, toStoreIssue(repo, item, now, state))
		if err != nil {
			return err
		}
		if !inserted {
			continue
		}
		if aged {
			p.log.Debug("issue aged out on arrival",
				"repo", repo.Slug(), "number", item.Number,
				"age", now.Sub(item.CreatedAt).Round(time.Second))
			continue
		}

		p.log.Info("new issue",
			"repo", repo.Slug(), "number", item.Number, "title", item.Title,
			"age", now.Sub(item.CreatedAt).Round(time.Second))
	}

	return p.store.RecordPoll(ctx, repo.ID, resp.ETag, now, waterline)
}

// warnIfGap reports a suspend or outage. Nothing is recovered: everything that
// appeared during the gap is past the freshness cutoff by definition. The
// warning exists so the operator can see what running on a laptop is costing.
func (p *Poller) warnIfGap(repo store.Repo) {
	if repo.LastPolledAt.IsZero() {
		return
	}
	gap := p.now().Sub(repo.LastPolledAt)
	if gap <= p.cfg.Polling.GapWarn() {
		return
	}
	msg := fmt.Sprintf("gap: %s, resuming %s", gap.Round(time.Minute), repo.Slug())
	p.log.Warn("poll gap", "repo", repo.Slug(), "gap", gap.Round(time.Second))
	if p.warn != nil {
		p.warn("poll_gap", msg)
	}
}

func (p *Poller) warnRateLimit(resetAt time.Time, remaining int) {
	key := resetAt.Unix()
	if _, seen := p.warnedResets.LoadOrStore(key, true); seen {
		return
	}
	msg := fmt.Sprintf("GitHub rate limit low: %d remaining, polling paused until %s",
		remaining, resetAt.Format(time.RFC3339))
	p.log.Warn("rate limit pause", "remaining", remaining, "reset_at", resetAt)
	if p.warn != nil {
		p.warn("rate_limit", msg)
	}
}

// pauseFor waits out the rate-limit window, never less than one interval so a
// stale or missing reset time cannot spin.
func (p *Poller) pauseFor(resetAt time.Time, interval time.Duration) time.Duration {
	wait := resetAt.Sub(p.now()) + 5*time.Second
	if wait < interval {
		return interval
	}
	return wait
}

func toStoreIssue(repo store.Repo, item Issue, seenAt time.Time, state store.State) store.Issue {
	return store.Issue{
		RepoID:       repo.ID,
		Number:       item.Number,
		Title:        item.Title,
		Body:         item.Body,
		HTMLURL:      item.HTMLURL,
		Author:       item.AuthorLogin(),
		Labels:       item.LabelNames(),
		Assignees:    item.AssigneeLogins(),
		CommentCount: item.Comments,
		CreatedAt:    item.CreatedAt,
		FirstSeenAt:  seenAt,
		State:        state,
	}
}
