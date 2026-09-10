package gh

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/filter"
	"github.com/Viswalahiri/dibs/internal/store"
)

// enrichBatch is how many issues one lease claims at a time. Each issue costs
// two requests, so a small batch keeps a burst well under the secondary rate
// limit.
const enrichBatch = 5

// Enricher fetches what the filter needs and applies it, advancing issues out
// of `new` to either `ready` or `rejected`. It is one of the pipeline workers:
// it claims rows under a lease, works, and commits the verdict and the state
// change together.
//
// Fetching and filtering happen in one stage because the filter is four pure
// predicates over bytes this worker has just fetched. Releasing the lease and
// re-claiming the row between them would buy nothing.
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

// Drain screens one batch and reports how many issues it advanced.
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

		state, reason := store.StateReady, store.RejectReason("")
		if verdict := filter.Apply(iss, enriched, e.cfg); verdict.Rejected {
			state, reason = store.StateRejected, verdict.Reason
			e.log.Info("filtered",
				"repo", repo.Slug(), "number", iss.Number, "reason", reason)
		}
		if err := e.store.SaveVerdict(ctx, iss.ID, state, reason); err != nil {
			if errors.Is(err, store.ErrNotClaimable) {
				continue
			}
			return done, err
		}
		e.log.Debug("screened",
			"repo", repo.Slug(), "number", iss.Number, "state", state,
			"comments", len(enriched.Comments), "linked_pr", enriched.HasLinkedPR)
		done++
	}
	return done, nil
}

// Enrich fetches one issue's context. The two sources are independent, so they
// run together; the client's own semaphore is what keeps the burst inside the
// secondary rate limit.
func (e *Enricher) Enrich(ctx context.Context, repo store.Repo, iss store.Issue) (filter.Context, error) {
	var (
		out  filter.Context
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
		linked, err := e.client.HasOpenLinkedPR(ctx, repo.Owner, repo.Name, iss.Number)
		if err != nil {
			return err
		}
		mu.Lock()
		out.HasLinkedPR = linked
		mu.Unlock()
		return nil
	})

	run(func() error {
		comments, err := e.client.Comments(ctx, repo.Owner, repo.Name, iss.Number)
		if err != nil {
			return err
		}
		mu.Lock()
		out.Comments = comments
		mu.Unlock()
		return nil
	})

	wg.Wait()
	if len(errs) > 0 {
		return filter.Context{}, errors.Join(errs...)
	}
	return out, nil
}

// notFound reports whether err is a 404. A missing sub-resource means the
// issue was deleted or transferred, and enriching around it beats retrying a
// request that will never succeed.
func notFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.NotFound()
}
