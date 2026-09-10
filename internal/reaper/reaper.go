// Package reaper is the housekeeping worker. Everything here is periodic and
// entirely local: it returns abandoned rows to their queue, ages out issues the
// push worker never reached, sets each repository's polling rate from how many
// issues it actually produces, and warns when a repository goes dark. It makes
// no requests of its own.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

const (
	tickInterval = 15 * time.Minute

	// cadenceMinObservation is how long dibs watches a repository before it
	// will change its polling rate. Without it a repository adopted this
	// morning has produced nothing yet, reads as silent, and is dropped to
	// half-hourly polling, which is slow enough that its next issue is stale
	// on arrival. A week of the configured default costs nothing, because a
	// 304 consumes no rate-limit quota.
	cadenceMinObservation = 7 * 24 * time.Hour

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
	store *store.Store
	cfg   *config.Config
	log   *slog.Logger
	warn  gh.Warner
	now   func() time.Time

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

func New(s *store.Store, cfg *config.Config, log *slog.Logger, warn gh.Warner, opts ...Option) *Reaper {
	r := &Reaper{
		store: s, cfg: cfg, log: log, warn: warn,
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

// expireStale drops issues the push worker never reached. In practice that
// means Slack was unreachable for a fortnight, by which point claiming the
// issue is no longer realistic.
func (r *Reaper) expireStale(ctx context.Context, now time.Time) error {
	stale, err := r.store.StaleIssues(ctx, store.StateReady, now.Add(-r.cfg.Reaper.ExpireAfter()))
	if err != nil {
		return err
	}
	var errs []error
	for _, iss := range stale {
		if err := r.store.Expire(ctx, iss.ID); err != nil {
			if errors.Is(err, store.ErrNotClaimable) {
				continue // the push worker moved it on between the read and here
			}
			errs = append(errs, err)
			continue
		}
		r.log.Info("expired", "issue_id", iss.ID, "number", iss.Number)
	}
	return errors.Join(errs...)
}

// daily runs the cadence recompute and the dark-repo report once a day, at or
// after the configured hour. A laptop asleep at three in the morning runs them on wake
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

	// The dark-repo report reads whole days that have already ended, so its
	// numbers cannot change after the fact and a repeated pass sends a
	// byte-identical message that the outbox drops.
	end := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	errs := []error{
		r.recomputeCadence(ctx, now),
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
