package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/filter"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

// triageBatch is small on purpose. Each row costs a model call, and claiming
// more than a handful under one lease means a crash re-drives more work than it
// needs to.
const triageBatch = 3

// degradedScore is where an issue lands when the model could not be reached,
// could not produce a usable answer, or was never asked because the spend cap
// had already tripped. It sits in the middle of the range deliberately: nothing
// about an unscored issue should silently swallow it, and a wrongly pushed
// issue costs one click.
const degradedScore = 50

// Worker scores enriched issues. It runs the deterministic filter first, so
// every issue that stage kills costs nothing at all.
type Worker struct {
	client *Client
	store  *store.Store
	cfg    *config.Config
	log    *slog.Logger
	owner  string
	now    func() time.Time
}

func NewWorker(c *Client, s *store.Store, cfg *config.Config, log *slog.Logger) *Worker {
	return &Worker{
		client: c, store: s, cfg: cfg, log: log,
		owner: "triager",
		now:   func() time.Time { return time.Now().UTC() },
	}
}

func (w *Worker) Run(ctx context.Context) error {
	const idle = 5 * time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}

		n, err := w.Drain(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Error("triage", "err", err)
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

// Drain scores one batch and reports how many issues it advanced.
func (w *Worker) Drain(ctx context.Context) (int, error) {
	now := w.now()
	claimed, err := w.store.Claim(ctx, store.StateEnriched, triageBatch, w.owner,
		w.cfg.Reaper.LeaseTTL(), now)
	if err != nil {
		return 0, err
	}

	done := 0
	for _, iss := range claimed {
		if err := w.one(ctx, iss); err != nil {
			if errors.Is(err, store.ErrNotClaimable) {
				continue
			}
			// The lease expires and the row comes back. Every stage is safe to
			// run twice, so nothing is lost by giving up on it here.
			w.log.Error("triage issue", "issue_id", iss.ID, "number", iss.Number, "err", err)
			continue
		}
		done++
	}
	return done, nil
}

func (w *Worker) one(ctx context.Context, iss store.Issue) error {
	repo, err := w.store.RepoByID(ctx, iss.RepoID)
	if err != nil {
		return err
	}
	enriched, err := gh.DecodeContext(iss.EnrichmentJSON)
	if err != nil {
		return err
	}

	// The free stage first. Everything it rejects is an issue dibs never pays
	// to think about.
	if verdict := filter.Apply(iss, enriched, w.cfg); verdict.Rejected {
		w.log.Info("filtered",
			"repo", repo.Slug(), "number", iss.Number, "reason", verdict.Reason)
		return w.store.SaveTriage(ctx, iss.ID, store.TriageResult{
			State: store.StateRejected, RejectReason: verdict.Reason,
		})
	}

	now := w.now()
	system := SystemPrompt(w.cfg)
	user := RenderUserMessage(iss, enriched, repo, w.cfg, now)

	if reason, blocked, err := w.spendBlocked(ctx); err != nil {
		return err
	} else if blocked {
		// Fail open, exactly as a model outage does below. Parking the issue
		// until the window rolls over would surface it hours late, and an issue
		// surfaced hours late is one somebody else has already taken. The cap
		// still does its job: no further model call is made.
		w.log.Error("triage capped, scoring open",
			"repo", repo.Slug(), "number", iss.Number, "reason", reason)
		return w.store.SaveTriage(ctx, iss.ID, store.TriageResult{
			Score: degradedScore,
			Input: user,
			JSON:  degradedJSON(errors.New(reason)),
			State: store.StateScored,
		})
	}

	result, attempts, scoreErr := w.client.Score(ctx, system, user)
	for _, a := range attempts {
		run := store.TriageRun{
			IssueID: iss.ID, Model: a.Model, InputTok: a.InputTok,
			OutputTok: a.OutputTok, LatencyMS: a.LatencyMS, OK: a.OK, Err: a.Err,
		}
		if err := w.store.RecordTriageRun(ctx, run, w.now()); err != nil {
			return err
		}
	}

	if scoreErr != nil {
		// Fail open. An outage that quietly dropped issues would be the one
		// failure this system cannot tolerate.
		w.log.Error("triage failed, scoring open",
			"repo", repo.Slug(), "number", iss.Number, "err", scoreErr)
		return w.store.SaveTriage(ctx, iss.ID, store.TriageResult{
			Score: degradedScore,
			Input: user,
			JSON:  degradedJSON(scoreErr),
			State: store.StateScored,
		})
	}

	score, rejected, reason := Composite(result.Response, iss, repo, w.cfg)
	state := store.StateRejected
	if !rejected {
		state, reason = Route(score, w.cfg)
	}

	w.log.Info("scored",
		"repo", repo.Slug(), "number", iss.Number,
		"score", score, "state", state, "reason", reason)

	return w.store.SaveTriage(ctx, iss.ID, store.TriageResult{
		Score:        score,
		EffortLowH:   result.Response.Effort.LowHours,
		EffortHighH:  result.Response.Effort.HighHours,
		Input:        user,
		JSON:         result.Raw,
		State:        state,
		RejectReason: reason,
	})
}

// spendBlocked reports whether the daily call cap or the monthly budget has
// stopped triage, and enqueues one deduplicated warning per period when it has.
//
// Both caps exist to bound a bug, not to manage a budget. At the volume this
// system runs at the real bill is about a dollar a month, so anything that
// trips these is a loop, not a busy week.
func (w *Worker) spendBlocked(ctx context.Context) (string, bool, error) {
	now := w.now()
	loc := w.cfg.Profile.Location()
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)

	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	calls, err := w.store.CallsSince(ctx, midnight)
	if err != nil {
		return "", false, err
	}
	if calls >= w.cfg.Triage.DailyCallCap {
		msg := fmt.Sprintf("daily triage cap reached: %d calls since midnight, paused until tomorrow", calls)
		if err := w.warn(ctx, "call_cap:"+midnight.Format("2006-01-02"), msg); err != nil {
			return "", false, err
		}
		w.log.Error("daily call cap reached", "calls", calls, "cap", w.cfg.Triage.DailyCallCap)
		return msg, true, nil
	}

	monthStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	inTok, outTok, err := w.store.TokensSince(ctx, monthStart)
	if err != nil {
		return "", false, err
	}
	spent := Cost(inTok, outTok, w.cfg.Triage.Cost)
	budget := w.cfg.Triage.Cost.MonthlyBudgetUSD
	period := monthStart.Format("2006-01")

	if spent >= budget {
		msg := fmt.Sprintf("monthly triage budget spent: $%.2f of $%.2f, paused until next month", spent, budget)
		if err := w.warn(ctx, "budget_stop:"+period, msg); err != nil {
			return "", false, err
		}
		w.log.Error("monthly budget exhausted", "spent_usd", spent, "budget_usd", budget)
		return msg, true, nil
	}
	if spent >= 0.8*budget {
		msg := fmt.Sprintf("monthly triage spend at $%.2f of $%.2f", spent, budget)
		if err := w.warn(ctx, "budget_warn:"+period, msg); err != nil {
			return "", false, err
		}
	}
	return "", false, nil
}

func (w *Worker) warn(ctx context.Context, dedupeKey, message string) error {
	payload, err := json.Marshal(map[string]string{"text": message})
	if err != nil {
		return err
	}
	_, err = w.store.Enqueue(ctx, store.Outgoing{
		Kind: "warning", DedupeKey: dedupeKey, Payload: string(payload),
	}, w.now())
	return err
}

// Cost converts token counts to dollars at the configured rates.
func Cost(inputTok, outputTok int, c config.Cost) float64 {
	return float64(inputTok)/1e6*c.InputPerMTokUSD +
		float64(outputTok)/1e6*c.OutputPerMTokUSD
}

// degradedJSON marks a score that came from the fallback rather than the model,
// so `dibs replay` and `dibs status` can tell the two apart and calibration
// does not treat an outage as a judgment.
func degradedJSON(cause error) string {
	b, err := json.Marshal(map[string]any{
		"degraded": true,
		"error":    cause.Error(),
	})
	if err != nil {
		return `{"degraded":true}`
	}
	return string(b)
}
