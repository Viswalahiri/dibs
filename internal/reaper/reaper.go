// Package reaper is the housekeeping worker. Everything here is periodic and
// read-only against GitHub: it returns abandoned rows to their queue, ages out
// issues nobody decided on, follows what became of the ones the operator took,
// and reports the handful of numbers that say whether the scoring is calibrated.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
	"github.com/Viswalahiri/dibs/internal/triage"
)

const (
	tickInterval = 15 * time.Minute

	// outcomeInterval is how long a tracked issue rests between checks. Each
	// check costs two requests, so at a handful of tracked issues this is
	// under a request an hour.
	outcomeInterval = 6 * time.Hour
	outcomeBatch    = 20

	// nudgeAfter is how long a tracked issue may sit with no pull request of
	// the operator's own before dibs asks about it. Once, ever.
	nudgeAfter = 5 * 24 * time.Hour

	// rubricMedianCeiling and rubricDays are the model-compliance instrument.
	// A model that has started agreeing with everything shows up as a median
	// that stays high, not as an error.
	rubricMedianCeiling = 75
	rubricDays          = 3

	// cadenceMinObservation is how long dibs watches a repository before it
	// will change its polling rate. Without it a repository adopted this
	// morning has produced nothing yet, reads as silent, and is dropped to
	// half-hourly polling, which is slow enough that its next issue is stale
	// on arrival. A week of the configured default costs nothing, because a
	// 304 consumes no rate-limit quota.
	cadenceMinObservation = 7 * 24 * time.Hour

	// skipRateCeiling is the alert-fatigue line. Past it, the junk floor is
	// admitting issues that are not worth the interruption.
	skipRateCeiling = 0.50

	// darkRepoRate is the share of rejections that means a repository has
	// acquired a label bot rather than gone quiet. Measured over a week, so a
	// slow repository with three bad issues does not trip it.
	darkRepoRate   = 0.90
	darkRepoWindow = 7 * 24 * time.Hour
	darkRepoFloor  = 10 // fewer issues than this in the window says nothing
)

// Reaper runs every pipeline chore that is not on the path from an issue to a
// Slack message.
type Reaper struct {
	client *gh.Client
	store  *store.Store
	cfg    *config.Config
	log    *slog.Logger
	warn   gh.Warner
	now    func() time.Time

	// lastDaily is the local date the daily pass last completed. It is held in
	// memory rather than persisted because every step of that pass converges:
	// the cadence write is idempotent and each report deduplicates on its own
	// day, so a restart at worst repeats work that changes nothing.
	lastDaily string
}

type Option func(*Reaper)

// WithClock replaces the clock, so tests can cross a day boundary without
// waiting for one.
func WithClock(f func() time.Time) Option { return func(r *Reaper) { r.now = f } }

func New(client *gh.Client, s *store.Store, cfg *config.Config, log *slog.Logger, warn gh.Warner, opts ...Option) *Reaper {
	r := &Reaper{
		client: client, store: s, cfg: cfg, log: log, warn: warn,
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func (r *Reaper) Run(ctx context.Context) error {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("reap", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Tick runs one pass. Each step is independent, so one failing does not skip
// the rest; the next tick is fifteen minutes away and nothing here is urgent.
func (r *Reaper) Tick(ctx context.Context) error {
	now := r.now()
	steps := []struct {
		name string
		run  func(context.Context, time.Time) error
	}{
		{"release leases", r.releaseLeases},
		{"expire stale issues", r.expireStale},
		{"track outcomes", r.trackOutcomes},
		{"nudge", r.nudge},
		{"daily", r.daily},
	}
	var errs []error
	for _, step := range steps {
		if err := step.run(ctx, now); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", step.name, err))
		}
	}
	return errors.Join(errs...)
}

// releaseLeases returns rows held by a worker that died to their own queue.
// Claim already ignores an expired lease, so this is housekeeping rather than
// recovery: it keeps a crashed worker's name off rows it no longer owns.
func (r *Reaper) releaseLeases(ctx context.Context, now time.Time) error {
	n, err := r.store.ReleaseExpiredLeases(ctx, now)
	if err != nil {
		return err
	}
	if n > 0 {
		r.log.Info("released expired leases", "count", n)
	}
	return nil
}

// expireStale drops issues nobody decided on. A scored issue that never got
// pushed, or a snoozed one that never came back, is long past the window where
// claiming it was realistic.
func (r *Reaper) expireStale(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-r.cfg.Reaper.ExpireAfter())
	var errs []error
	for _, state := range []store.State{store.StateScored, store.StateSnoozed} {
		stale, err := r.store.StaleIssues(ctx, state, cutoff)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, iss := range stale {
			if err := r.store.Expire(ctx, iss.ID, state); err != nil {
				if errors.Is(err, store.ErrNotClaimable) {
					continue // a worker moved it on between the read and here
				}
				errs = append(errs, err)
				continue
			}
			r.log.Info("expired", "issue_id", iss.ID, "number", iss.Number, "from", state)
		}
	}
	return errors.Join(errs...)
}

// trackOutcomes follows the issues the operator took. It reads GitHub and
// writes SQLite, never the other way round: the assignment and the pull
// request it records are ones the operator made by hand.
func (r *Reaper) trackOutcomes(ctx context.Context, now time.Time) error {
	due, err := r.store.TrackedDue(ctx, now.Add(-outcomeInterval), outcomeBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range due {
		if err := r.checkOutcome(ctx, t, now); err != nil {
			errs = append(errs, fmt.Errorf("issue %d: %w", t.IssueID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reaper) checkOutcome(ctx context.Context, t store.Tracked, now time.Time) error {
	iss, err := r.store.IssueByID(ctx, t.IssueID)
	if err != nil {
		return err
	}
	repo, err := r.store.RepoByID(ctx, iss.RepoID)
	if err != nil {
		return err
	}
	self := r.cfg.Profile.GitHubLogin

	var current gh.Issue
	path := fmt.Sprintf("/repos/%s/%s/issues/%d",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), iss.Number)
	if _, _, err := r.client.GetJSON(ctx, path, "", &current); err != nil {
		// A transferred or deleted issue will never answer. Closing it out
		// stops dibs asking about it twice a day forever.
		if notFound(err) {
			t.ClosedAt = now
			t.Outcome = store.OutcomeLost
			r.log.Info("tracked issue is gone", "repo", repo.Slug(), "number", iss.Number)
			return r.store.SaveOutcome(ctx, t, now)
		}
		return err
	}

	if t.AssignedAt.IsZero() && slices.Contains(current.AssigneeLogins(), self) {
		t.AssignedAt = now
		r.log.Info("assigned on github", "repo", repo.Slug(), "number", iss.Number)
	}

	prs, err := r.client.LinkedPRs(ctx, repo.Owner, repo.Name, iss.Number)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		// Only the operator's own pull request counts. Somebody else's means
		// the issue was lost, and silencing the nudge over it would hide that.
		if pr.Author == self {
			t.PRURL = pr.HTMLURL
			break
		}
	}

	if current.State == "closed" {
		t.ClosedAt = now
		if current.ClosedAt != nil {
			t.ClosedAt = current.ClosedAt.UTC()
		}
		t.Outcome = store.OutcomeLost
		if t.PRURL != "" {
			t.Outcome = store.OutcomeLanded
		}
		r.log.Info("outcome",
			"repo", repo.Slug(), "number", iss.Number, "outcome", t.Outcome, "pr", t.PRURL)
	}
	return r.store.SaveOutcome(ctx, t, now)
}

// nudge asks once about a tracked issue that has gone five days without a pull
// request. Releasing it is a browser action; dibs only raises the question.
func (r *Reaper) nudge(ctx context.Context, now time.Time) error {
	due, err := r.store.NudgeDue(ctx, now.Add(-nudgeAfter))
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range due {
		iss, err := r.store.IssueByID(ctx, t.IssueID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		repo, err := r.store.RepoByID(ctx, iss.RepoID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// The message carries the tracked date rather than an elapsed time, so
		// it is identical on every tick and the outbox dedupe key holds.
		r.warn(fmt.Sprintf("nudge:%d", t.IssueID), fmt.Sprintf(
			"still on <%s|%s #%d>? Tracked %s, no PR of yours linked yet.",
			iss.HTMLURL, repo.Slug(), iss.Number, t.TrackedAt.Format("2 Jan")))
	}
	return errors.Join(errs...)
}

// daily runs the reports and the cadence recompute once a day, at or after the
// configured hour. A laptop asleep at three in the morning runs them on wake
// instead of skipping the day.
func (r *Reaper) daily(ctx context.Context, now time.Time) error {
	loc := r.location()
	local := now.In(loc)
	if local.Hour() < r.cfg.Reaper.CadenceRecomputeHour {
		return nil
	}
	day := local.Format("2006-01-02")
	if r.lastDaily == day {
		return nil
	}

	// Every report reads the previous whole local day, so its numbers cannot
	// change after the fact and a repeated pass sends a byte-identical message
	// that the outbox drops.
	end := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	start := end.AddDate(0, 0, -1)

	errs := []error{
		r.recomputeCadence(ctx, now),
		r.reportScores(ctx, end),
		r.reportSpend(ctx, start, end),
		r.reportSkipRate(ctx, start, end),
		r.reportDarkRepos(ctx, end),
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	r.lastDaily = day
	return nil
}

// recomputeCadence sets each repository's polling interval from how many
// issues it has actually produced. The rate is measured over the time dibs has
// been watching rather than a flat thirty days, so a busy repository added
// yesterday is polled quickly today instead of in a month.
func (r *Reaper) recomputeCadence(ctx context.Context, now time.Time) error {
	repos, err := r.store.EnabledRepos(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, repo := range repos {
		if !repo.Adopted() || now.Sub(repo.AdoptedAt) < cadenceMinObservation {
			continue
		}
		// Issues that aged out on arrival are counted along with the rest. They
		// are what lifts a repository back off the slow cadence: a repository
		// polled too slowly produces nothing but aged-out issues, and counting
		// them is what turns that into evidence rather than a rut.
		seen, err := r.store.IssuesSeenSince(ctx, repo.ID, now.AddDate(0, 0, -30))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		sec := r.intervalFor(perWeek(seen, now.Sub(repo.AdoptedAt)))
		if sec == repo.PollIntervalSec && seen == repo.IssuesLast30d {
			continue
		}
		if err := r.store.SetCadence(ctx, repo.ID, seen, sec); err != nil {
			errs = append(errs, err)
			continue
		}
		r.log.Info("cadence",
			"repo", repo.Slug(), "issues_30d", seen, "interval_sec", sec)
	}
	return errors.Join(errs...)
}

// perWeek converts an issue count to a weekly rate over the window it was
// observed in, rather than a flat thirty days. A repository dibs has watched
// for nine days is judged on those nine days, so its rate is right as soon as
// it is measurable instead of a month later. The window is bounded by the same
// two limits as the count it divides.
func perWeek(seen int, observed time.Duration) float64 {
	const weekHours = 7 * 24
	observed = min(max(observed, cadenceMinObservation), 30*24*time.Hour)
	// Scaled before the division, so a repository sitting exactly on a
	// boundary of the table below is not moved off it by rounding.
	return float64(seen) * weekHours / observed.Hours()
}

// intervalFor is the cadence table from the specification, floored at the
// configured minimum so a busy repository cannot be polled faster than the
// operator allows.
func (r *Reaper) intervalFor(rate float64) int {
	var sec int
	switch {
	case rate >= 10:
		sec = 30
	case rate >= 3:
		sec = 60
	case rate >= 1:
		sec = 300
	default:
		sec = 1800
	}
	return max(sec, r.cfg.Polling.MinIntervalSec)
}

// reportScores logs the day's score distribution and warns when the model has
// stopped discriminating. A rubric the model agrees with everywhere produces a
// median that stays high, which no single response would reveal.
func (r *Reaper) reportScores(ctx context.Context, end time.Time) error {
	medians := make([]int, 0, rubricDays)
	for i := 0; i < rubricDays; i++ {
		scores, err := r.store.ScoresBetween(ctx, end.AddDate(0, 0, -i-1), end.AddDate(0, 0, -i))
		if err != nil {
			return err
		}
		if i == 0 {
			if len(scores) == 0 {
				r.log.Info("scores", "count", 0)
				return nil
			}
			r.log.Info("scores",
				"count", len(scores),
				"median", percentile(scores, 0.50),
				"p90", percentile(scores, 0.90))
		}
		if len(scores) == 0 {
			return nil // an idle day breaks the run rather than extending it
		}
		medians = append(medians, percentile(scores, 0.50))
	}
	for _, m := range medians {
		if m <= rubricMedianCeiling {
			return nil
		}
	}
	r.warn("rubric", fmt.Sprintf(
		"scoring median has been above %d for %d days (%v). The rubric needs recalibration.",
		rubricMedianCeiling, rubricDays, medians))
	return nil
}

// reportSpend logs what the day's triage actually cost. It is the measured
// number that replaces the estimate in the plan.
func (r *Reaper) reportSpend(ctx context.Context, start, end time.Time) error {
	calls, input, output, err := r.store.SpendBetween(ctx, start, end)
	if err != nil {
		return err
	}
	if calls == 0 {
		return nil
	}
	r.log.Info("spend",
		"calls", calls,
		"input_tok_per_call", input/calls,
		"output_tok_per_call", output/calls,
		"usd", triage.Cost(input, output, r.cfg.Triage.Cost))
	return nil
}

// reportSkipRate is the alert-fatigue instrument. A day where most pushes were
// skipped means the junk floor is too low, and the fix is one line of config.
func (r *Reaper) reportSkipRate(ctx context.Context, start, end time.Time) error {
	counts, err := r.store.DecisionCounts(ctx, start, end)
	if err != nil {
		return err
	}
	decided := counts[store.StateTracked] + counts[store.StateSkipped]
	if decided == 0 {
		return nil
	}
	rate := float64(counts[store.StateSkipped]) / float64(decided)
	r.log.Info("decisions",
		"tracked", counts[store.StateTracked],
		"skipped", counts[store.StateSkipped],
		"snoozed", counts[store.StateSnoozed],
		"skip_rate", rate)
	if rate > skipRateCeiling {
		r.warn("skip_rate", fmt.Sprintf(
			"%.0f%% of yesterday's alerts were skipped (%d of %d). Consider raising scoring.junk_floor.",
			rate*100, counts[store.StateSkipped], decided))
	}
	return nil
}

// reportDarkRepos catches a repository that has stopped producing anything
// dibs will surface. That is almost always a new label bot rather than a quiet
// week, and it is invisible from the alert stream because the symptom is
// silence.
func (r *Reaper) reportDarkRepos(ctx context.Context, end time.Time) error {
	repos, err := r.store.EnabledRepos(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, repo := range repos {
		seen, rejected, err := r.store.RejectionCounts(ctx, repo.ID, end.Add(-darkRepoWindow), end)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if seen < darkRepoFloor {
			continue
		}
		if rate := float64(rejected) / float64(seen); rate >= darkRepoRate {
			r.warn("dark_repo", fmt.Sprintf(
				"%s: %d of %d issues rejected this week. Check its labels and the killfile.",
				repo.Slug(), rejected, seen))
		}
	}
	return errors.Join(errs...)
}

func (r *Reaper) location() *time.Location {
	if loc := r.cfg.Profile.Location(); loc != nil {
		return loc
	}
	return time.UTC
}

// percentile reads the nearest rank from an ascending slice. Exact enough for
// a daily line about five scores.
func percentile(ascending []int, p float64) int {
	if len(ascending) == 0 {
		return 0
	}
	i := int(p*float64(len(ascending))+0.9999) - 1
	return ascending[min(max(i, 0), len(ascending)-1)]
}

// notFound reports whether err is a 404, which for a tracked issue means it
// was deleted or transferred.
func notFound(err error) bool {
	var se *gh.StatusError
	return errors.As(err, &se) && se.NotFound()
}
