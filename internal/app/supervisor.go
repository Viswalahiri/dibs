// Package app runs the pipeline workers. Workers never talk to each other;
// the database is the only thing between them. All this does is start them,
// keep them alive together, and stop them together.
package app

import (
	"context"
	"log/slog"
	"sync"
)

// Worker is one long-running pipeline stage. Run must return when its context
// is cancelled.
type Worker struct {
	Name string
	Run  func(context.Context) error
}

// Supervise runs every worker until ctx is cancelled or one of them returns an
// error, then cancels the rest and waits. It reports the first error, which is
// the one that caused the shutdown.
//
// A worker returning at all is treated as fatal. Each is written to loop until
// cancelled, so an early return means something is wrong that restarting the
// same loop in the same process would not fix.
func Supervise(ctx context.Context, log *slog.Logger, workers ...Worker) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for _, w := range workers {
		wg.Add(1)
		go func(w Worker) {
			defer wg.Done()
			err := w.Run(ctx)
			if err != nil && ctx.Err() == nil {
				log.Error("worker stopped", "worker", w.Name, "err", err)
				once.Do(func() { first = err })
			}
			cancel()
		}(w)
	}
	wg.Wait()
	return first
}
