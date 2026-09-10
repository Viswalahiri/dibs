package gh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
)

// enrichBatch is how many issues one lease claims at a time. Each issue costs
// three or four requests, so a small batch keeps a burst well under the
// secondary rate limit.
const enrichBatch = 5

// contributingPaths are tried in order. Most repositories use the first.
var contributingPaths = []string{"CONTRIBUTING.md", ".github/CONTRIBUTING.md"}

// Context is everything the filter and the model see beyond the issue row
// itself. It is fetched once, stored as JSON on the issue, and read back by
// both later stages so neither spends a request of its own.
type Context struct {
	HasLinkedPR bool        `json:"has_linked_pr"`
	Comments    []Comment   `json:"comments"`
	Doc         string      `json:"doc"`
	Author      AuthorStats `json:"author"`
}

type Comment struct {
	Login     string    `json:"login"`
	Assoc     string    `json:"assoc"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// AuthorStats counts what this author has filed in this repository. See the
// note on the author_stats table for why pull requests are counted rather than
// self-fixes.
type AuthorStats struct {
	IssuesOpened int `json:"issues_opened"`
	PRsOpened    int `json:"prs_opened"`
}

func (c Context) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode enrichment: %w", err)
	}
	return string(b), nil
}

// DecodeContext reads back what Encode wrote. An empty string decodes to a
// zero Context, which is what an issue that has not been enriched yet looks
// like.
func DecodeContext(s string) (Context, error) {
	if strings.TrimSpace(s) == "" {
		return Context{}, nil
	}
	var c Context
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return Context{}, fmt.Errorf("decode enrichment: %w", err)
	}
	return c, nil
}

// Enricher fetches the context for issues sitting in `new` and advances them
// to `enriched`. It is one of the pipeline workers: it claims rows under a
// lease, works, and commits the result and the state change together.
type Enricher struct {
	client *Client
	store  *store.Store
	cfg    *config.Config
	log    *slog.Logger
	owner  string
	now    func() time.Time
}

func NewEnricher(c *Client, s *store.Store, cfg *config.Config, log *slog.Logger) *Enricher {
	return &Enricher{
		client: c, store: s, cfg: cfg, log: log,
		owner: "enricher",
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// Run drains the `new` queue until ctx is cancelled. It polls rather than
// waiting on a signal, because the database is the queue and a poll is the
// only thing that survives a crash on either side.
func (e *Enricher) Run(ctx context.Context) error {
	const idle = 5 * time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		n, err := e.Drain(ctx)
		if err != nil && ctx.Err() == nil {
			e.log.Error("enrich", "err", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		// Keep going while there is work, back off to a poll when there is not.
		if n > 0 {
			timer.Reset(0)
		} else {
			timer.Reset(idle)
		}
	}
}

// Drain enriches one batch and reports how many issues it advanced.
func (e *Enricher) Drain(ctx context.Context) (int, error) {
	now := e.now()
	claimed, err := e.store.Claim(ctx, store.StateNew, enrichBatch, e.owner,
		e.cfg.Reaper.LeaseTTL(), now)
	if err != nil {
		return 0, err
	}

	done := 0
	for _, iss := range claimed {
		repo, err := e.store.RepoByID(ctx, iss.RepoID)
		if err != nil {
			return done, err
		}
		enriched, err := e.Enrich(ctx, repo, iss)
		if err != nil {
			// The lease expires and the row comes back round. Nothing is lost.
			e.log.Error("enrich issue", "repo", repo.Slug(), "number", iss.Number, "err", err)
			continue
		}
		encoded, err := enriched.Encode()
		if err != nil {
			return done, err
		}
		if err := e.store.SaveEnrichment(ctx, iss.ID, encoded); err != nil {
			if errors.Is(err, store.ErrNotClaimable) {
				continue
			}
			return done, err
		}
		e.log.Debug("enriched",
			"repo", repo.Slug(), "number", iss.Number,
			"comments", len(enriched.Comments), "linked_pr", enriched.HasLinkedPR)
		done++
	}
	return done, nil
}

// Enrich fetches one issue's context. The four sources are independent, so
// they run together; the client's own semaphore is what keeps the burst inside
// the secondary rate limit.
func (e *Enricher) Enrich(ctx context.Context, repo store.Repo, iss store.Issue) (Context, error) {
	var (
		out  Context
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}

	run(func() error {
		linked, err := e.hasLinkedPR(ctx, repo, iss.Number)
		if err != nil {
			return err
		}
		mu.Lock()
		out.HasLinkedPR = linked
		mu.Unlock()
		return nil
	})

	run(func() error {
		comments, err := e.comments(ctx, repo, iss.Number)
		if err != nil {
			return err
		}
		mu.Lock()
		out.Comments = comments
		mu.Unlock()
		return nil
	})

	// The document and the author stats both degrade to empty rather than
	// failing the issue. Neither is worth a retry: the doc is a nicety and the
	// search endpoint has its own tight limit.
	run(func() error {
		doc, err := e.doc(ctx, repo)
		if err != nil {
			e.log.Debug("contributing unavailable", "repo", repo.Slug(), "err", err)
			return nil
		}
		mu.Lock()
		out.Doc = doc
		mu.Unlock()
		return nil
	})

	run(func() error {
		stats, err := e.authorStats(ctx, repo, iss.Author)
		if err != nil {
			e.log.Debug("author stats unavailable",
				"repo", repo.Slug(), "login", iss.Author, "err", err)
			return nil
		}
		mu.Lock()
		out.Author = stats
		mu.Unlock()
		return nil
	})

	wg.Wait()
	if len(errs) > 0 {
		return Context{}, errors.Join(errs...)
	}
	return out, nil
}

// timelineEvent is the subset of the timeline payload that reveals a linked
// pull request. GitHub reports the link from the issue's side as a
// cross-referenced or connected event whose source is a pull request.
type timelineEvent struct {
	Event  string `json:"event"`
	Source *struct {
		Issue *struct {
			State       string          `json:"state"`
			PullRequest *PullRequestRef `json:"pull_request"`
		} `json:"issue"`
	} `json:"source"`
}

func (e *Enricher) hasLinkedPR(ctx context.Context, repo store.Repo, number int) (bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?per_page=100",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), number)

	var events []timelineEvent
	if _, _, err := e.client.getJSON(ctx, path, "", timelineAccept, &events); err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, ev := range events {
		if ev.Event != "cross-referenced" && ev.Event != "connected" {
			continue
		}
		// Only an open pull request means the work is genuinely under way. A
		// closed one is usually an abandoned attempt, which leaves the issue
		// available.
		if ev.Source != nil && ev.Source.Issue != nil &&
			ev.Source.Issue.PullRequest != nil && ev.Source.Issue.State == "open" {
			return true, nil
		}
	}
	return false, nil
}

type commentPayload struct {
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"created_at"`
	AuthorAssociation string    `json:"author_association"`
	User              *User     `json:"user"`
}

func (e *Enricher) comments(ctx context.Context, repo store.Repo, number int) ([]Comment, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=20",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), number)

	var payload []commentPayload
	if _, _, err := e.client.GetJSON(ctx, path, "", &payload); err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Comment, 0, len(payload))
	for _, c := range payload {
		login := ""
		if c.User != nil {
			login = c.User.Login
		}
		out = append(out, Comment{
			Login:     login,
			Assoc:     c.AuthorAssociation,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

type contentsPayload struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// doc returns the useful part of CONTRIBUTING.md, cached for a week. Most of
// such a file is setup boilerplate that costs tokens and says nothing about
// whether an issue can be claimed, so only the section that mentions claiming
// survives when one can be identified.
func (e *Enricher) doc(ctx context.Context, repo store.Repo) (string, error) {
	now := e.now()
	for _, path := range contributingPaths {
		cached, err := e.store.Doc(ctx, repo.ID, path)
		if err != nil {
			return "", err
		}
		if cached.Fresh(now) {
			if cached.Content == "" {
				continue // a fresh empty entry means this path is known absent
			}
			return cached.Content, nil
		}

		api := fmt.Sprintf("/repos/%s/%s/contents/%s",
			url.PathEscape(repo.Owner), url.PathEscape(repo.Name), path)
		var payload contentsPayload
		resp, notModified, err := e.client.GetJSON(ctx, api, cached.ETag, &payload)
		switch {
		case notModified:
			cached.FetchedAt = now
			if err := e.store.PutDoc(ctx, repo.ID, cached); err != nil {
				return "", err
			}
			if cached.Content == "" {
				continue
			}
			return cached.Content, nil
		case err != nil && notFound(err):
			// Record the absence so the next week of enrichments skips the
			// request entirely.
			if err := e.store.PutDoc(ctx, repo.ID, store.Doc{Path: path, FetchedAt: now}); err != nil {
				return "", err
			}
			continue
		case err != nil:
			return "", err
		}

		decoded, err := decodeContents(payload)
		if err != nil {
			return "", err
		}
		excerpt := claimingExcerpt(decoded, e.cfg.Triage.MaxDocChars)
		if err := e.store.PutDoc(ctx, repo.ID, store.Doc{
			Path: path, ETag: resp.ETag, Content: excerpt, FetchedAt: now,
		}); err != nil {
			return "", err
		}
		if excerpt == "" {
			continue
		}
		return excerpt, nil
	}
	return "", nil
}

func decodeContents(p contentsPayload) (string, error) {
	if p.Encoding != "base64" {
		return p.Content, nil
	}
	// GitHub wraps base64 contents at 60 columns.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(p.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode contents: %w", err)
	}
	return string(raw), nil
}

var (
	headingRe  = regexp.MustCompile(`(?m)^#{1,6}\s+.*$`)
	claimingRe = regexp.MustCompile(`(?i)\b(claim|assign|take|pick(ing)? up|good first|first[- ]time|new contributor)\b`)
)

// claimingExcerpt returns the section whose heading mentions claiming or
// assignment, falling back to the head of the document. The result is capped
// at limit characters.
func claimingExcerpt(doc string, limit int) string {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return ""
	}
	for _, loc := range headingRe.FindAllStringIndex(doc, -1) {
		if !claimingRe.MatchString(doc[loc[0]:loc[1]]) {
			continue
		}
		section := doc[loc[0]:]
		// Stop at the next heading of the same or a higher level.
		level := len(section) - len(strings.TrimLeft(section, "#"))
		if next := nextHeading(section[loc[1]-loc[0]:], level); next >= 0 {
			section = section[:loc[1]-loc[0]+next]
		}
		return Truncate(strings.TrimSpace(section), limit)
	}
	return Truncate(doc, limit)
}

// nextHeading returns the offset of the next heading at or above level, or -1.
func nextHeading(s string, level int) int {
	for _, loc := range headingRe.FindAllStringIndex(s, -1) {
		h := s[loc[0]:loc[1]]
		if len(h)-len(strings.TrimLeft(h, "#")) <= level {
			return loc[0]
		}
	}
	return -1
}

// Truncate cuts to at most limit bytes on a rune boundary and marks the cut,
// so the model is never shown a sentence that just stops.
func Truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "\n[truncated]"
}

type searchResult struct {
	TotalCount int `json:"total_count"`
}

// authorStats counts this author's issues and pull requests in this
// repository, cached for a week. The search endpoint carries its own
// thirty-per-minute limit, which is why nothing here runs in a loop and why a
// failure degrades to zeros instead of erroring the issue.
func (e *Enricher) authorStats(ctx context.Context, repo store.Repo, login string) (AuthorStats, error) {
	if login == "" {
		return AuthorStats{}, nil
	}
	now := e.now()
	cached, err := e.store.AuthorStats(ctx, repo.ID, login)
	if err != nil {
		return AuthorStats{}, err
	}
	if cached.Fresh(now) {
		return AuthorStats{IssuesOpened: cached.IssuesOpened, PRsOpened: cached.PRsOpened}, nil
	}

	count := func(kind string) (int, error) {
		q := fmt.Sprintf("repo:%s/%s author:%s type:%s", repo.Owner, repo.Name, login, kind)
		path := "/search/issues?per_page=1&q=" + url.QueryEscape(q)
		var res searchResult
		if _, _, err := e.client.GetJSON(ctx, path, "", &res); err != nil {
			return 0, err
		}
		return res.TotalCount, nil
	}

	issues, err := count("issue")
	if err != nil {
		return AuthorStats{}, err
	}
	prs, err := count("pr")
	if err != nil {
		return AuthorStats{}, err
	}

	stats := store.AuthorStats{
		Login: login, IssuesOpened: issues, PRsOpened: prs, ComputedAt: now,
	}
	if err := e.store.PutAuthorStats(ctx, repo.ID, stats); err != nil {
		return AuthorStats{}, err
	}
	return AuthorStats{IssuesOpened: issues, PRsOpened: prs}, nil
}

// notFound reports whether err is a 404. A missing sub-resource means the
// issue was deleted or transferred, and enriching around it beats retrying a
// request that will never succeed.
func notFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.NotFound()
}
