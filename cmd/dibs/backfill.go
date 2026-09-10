package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/filter"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
	"github.com/Viswalahiri/dibs/internal/triage"
)

// backfillPageSize is a full page. Backfill is the one place that paginates,
// and it is a manual command, so fewer larger requests is the right trade.
const backfillPageSize = 100

// cmdBackfill scores a repository's recent history without notifying anyone.
// It exists to build a corpus: a week of live running produces a handful of
// scored issues, and calibrating the junk floor and the veto threshold against
// a handful is guesswork.
//
// Backfilled scores are indicative, not equivalent to live ones. An issue is
// scored as it stands today, comments, assignees and all, not as it stood in
// the minute it was opened. Read them as a sample of how the rubric behaves on
// this repository, not as a replay of what dibs would have pushed.
func cmdBackfill(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: dibs backfill <owner/repo> --since <duration>")
	}
	slug := args[0]

	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	since := fs.Duration("since", 0, "how far back to ingest, for example 720h")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *since <= 0 {
		return errors.New("backfill needs --since, for example --since 720h")
	}

	owner, name, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("%q is not an owner/repo slug", slug)
	}

	l, err := load(config.EnvGitHubToken, config.EnvAnthropicKey)
	if err != nil {
		return err
	}
	defer l.store.Close()

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// repos.yaml is the source of truth for which repositories dibs knows, and
	// backfill honours it rather than inventing an entry of its own.
	if err := l.store.SyncRepos(ctx, l.repos, l.cfg.Polling.DefaultIntervalSec); err != nil {
		return fmt.Errorf("sync repos: %w", err)
	}
	repo, err := l.store.RepoBySlug(ctx, owner, name)
	if err != nil {
		return fmt.Errorf("%s is not in %s; add it there first", slug, l.paths.Repos)
	}

	var clientOpts []gh.Option
	if base := os.Getenv("DIBS_GITHUB_API"); base != "" {
		clientOpts = append(clientOpts, gh.WithBaseURL(base))
	}
	client := gh.New(l.secrets.GitHubToken, l.cfg.Polling.MaxConcurrent, clientOpts...)

	cutoff := time.Now().UTC().Add(-*since)
	ingested, err := ingest(ctx, client, l.store, repo, cutoff)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d issues ingested since %s\n",
		repo.Slug(), ingested, cutoff.Format(time.DateOnly))

	return scoreBackfill(ctx, client, l, repo, log)
}

// ingest walks the repository's history newest first and records everything
// opened since the cutoff. Rows land terminal, so no pipeline worker can claim
// one and no backfilled issue can ever reach Slack.
//
// Closed issues are included. A month of open-only issues on a healthy
// repository is nearly empty, because the good ones were closed, which would
// leave a corpus made entirely of what nobody wanted.
func ingest(ctx context.Context, client *gh.Client, s *store.Store, repo store.Repo, cutoff time.Time) (int, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues?state=all&sort=created&direction=desc&per_page=%d",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), backfillPageSize)

	now := time.Now().UTC()
	ingested := 0
	for path != "" {
		var items []gh.Issue
		resp, _, err := client.GetJSON(ctx, path, "", &items)
		if err != nil {
			return ingested, fmt.Errorf("list %s: %w", repo.Slug(), err)
		}
		if len(items) == 0 {
			return ingested, nil
		}
		for _, item := range items {
			// The issues endpoint returns pull requests too.
			if item.IsPullRequest() {
				continue
			}
			// Sorted newest first, so the first issue past the cutoff ends the
			// walk rather than skipping one page's worth.
			if item.CreatedAt.Before(cutoff) {
				return ingested, nil
			}
			_, inserted, err := s.Insert(ctx, backfillIssue(repo, item, now))
			if err != nil {
				return ingested, err
			}
			if inserted {
				ingested++
			}
		}
		path = gh.NextPage(resp.Link)
	}
	return ingested, nil
}

func backfillIssue(repo store.Repo, item gh.Issue, seenAt time.Time) store.Issue {
	return store.Issue{
		RepoID:       repo.ID,
		Number:       item.Number,
		NodeID:       item.NodeID,
		Title:        item.Title,
		Body:         item.Body,
		HTMLURL:      item.HTMLURL,
		Author:       item.AuthorLogin(),
		AuthorAssoc:  item.AuthorAssociation,
		Labels:       item.LabelNames(),
		Assignees:    item.AssigneeLogins(),
		CommentCount: item.Comments,
		CreatedAt:    item.CreatedAt,
		FirstSeenAt:  seenAt,
		State:        store.StateBackfilled,
	}
}

// scoreBackfill enriches and scores every backfilled row that has no score
// yet, which includes anything a previous run was interrupted partway through.
// It stops at the daily call cap rather than spending past it, and reports
// where it got to so the next run can pick up tomorrow.
func scoreBackfill(ctx context.Context, client *gh.Client, l *loaded, repo store.Repo, log *slog.Logger) error {
	pending, err := l.store.BackfillPending(ctx, repo.ID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Println("nothing left to score")
		return nil
	}

	enricher := gh.NewEnricher(client, l.store, l.cfg, log)
	scorer := triage.NewClient(l.secrets.AnthropicKey, l.cfg)
	loc := l.cfg.Profile.Location()
	if loc == nil {
		loc = time.UTC
	}

	scored, filtered := 0, 0
	for _, iss := range pending {
		enriched, err := enricher.Enrich(ctx, repo, iss)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  #%d: enrichment failed, skipping: %v\n", iss.Number, err)
			continue
		}
		encoded, err := enriched.Encode()
		if err != nil {
			return err
		}

		// The free stage first, exactly as the pipeline runs it. What it kills
		// costs nothing and is still worth recording.
		if verdict := filter.Apply(iss, enriched, l.cfg); verdict.Rejected {
			if err := l.store.SaveBackfill(ctx, iss.ID, encoded, store.TriageResult{
				RejectReason: verdict.Reason,
			}); err != nil && !errors.Is(err, store.ErrNotClaimable) {
				return err
			}
			filtered++
			continue
		}

		local := time.Now().In(loc)
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		calls, err := l.store.CallsSince(ctx, midnight)
		if err != nil {
			return err
		}
		if calls >= l.cfg.Triage.DailyCallCap {
			fmt.Printf("stopped at the daily call cap of %d; run again tomorrow to finish\n",
				l.cfg.Triage.DailyCallCap)
			break
		}

		now := time.Now().UTC()
		user := triage.RenderUserMessage(iss, enriched, repo, l.cfg, now)
		result, attempts, scoreErr := scorer.Score(ctx, triage.SystemPrompt(l.cfg), user)
		for _, a := range attempts {
			if err := l.store.RecordTriageRun(ctx, store.TriageRun{
				IssueID: iss.ID, Model: a.Model, InputTok: a.InputTok,
				OutputTok: a.OutputTok, LatencyMS: a.LatencyMS, OK: a.OK, Err: a.Err,
			}, time.Now().UTC()); err != nil {
				return err
			}
		}
		if scoreErr != nil {
			// Unlike the live pipeline there is nothing to fail open for: a
			// corpus entry scored by the fallback is not evidence, so the row
			// is left unscored for the next run to retry.
			fmt.Fprintf(os.Stderr, "  #%d: scoring failed, leaving it for the next run: %v\n",
				iss.Number, scoreErr)
			continue
		}

		score, rejected, reason := triage.Composite(result.Response, iss, repo, l.cfg)
		if !rejected {
			_, reason = triage.Route(score, l.cfg)
		}
		if err := l.store.SaveBackfill(ctx, iss.ID, encoded, store.TriageResult{
			Score:        score,
			EffortLowH:   result.Response.Effort.LowHours,
			EffortHighH:  result.Response.Effort.HighHours,
			Input:        user,
			JSON:         result.Raw,
			RejectReason: reason,
		}); err != nil && !errors.Is(err, store.ErrNotClaimable) {
			return err
		}
		scored++
	}

	fmt.Printf("%d scored, %d killed by the filter for free\n", scored, filtered)
	fmt.Println("nothing was sent to Slack; `dibs replay` reads the corpus")
	return nil
}
