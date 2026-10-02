// Package jobs runs background work from the database queue.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"chokominto/internal/store"
)

type Handler func(ctx context.Context, j store.Job) error

type Runner struct {
	DB       *store.DB
	Handlers map[string]Handler
	Workers  int
	// Batch is how many jobs a worker claims at once (default 1). Claiming
	// and finishing are a disk sync each, so quick jobs go in batches.
	Batch int
	Log   *slog.Logger
	// Idle is how long a worker waits before checking again when the
	// queue is empty.
	Idle time.Duration
}

// backoff is how long to wait after a failed attempt.
func backoff(attempts int) time.Duration {
	switch attempts {
	case 1:
		return time.Minute
	case 2:
		return 10 * time.Minute
	case 3:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}

// Run works through due jobs until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	workers := max(r.Workers, 1)
	idle := r.Idle
	if idle == 0 {
		idle = 2 * time.Second
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for ctx.Err() == nil {
				if !r.RunOne(ctx) {
					select {
					case <-ctx.Done():
					case <-time.After(idle):
					}
				}
			}
		})
	}
	wg.Wait()
}

// RunOne runs the next due batch of jobs, if any, and reports whether
// there was one.
func (r *Runner) RunOne(ctx context.Context) bool {
	kinds := make([]string, 0, len(r.Handlers))
	for k := range r.Handlers {
		kinds = append(kinds, k)
	}
	js, err := r.DB.ClaimJobs(ctx, kinds, max(r.Batch, 1))
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			r.Log.Error("claiming a job failed", "err", err)
		}
		return false
	}
	var done []store.Job
	for i, j := range js {
		if ctx.Err() != nil {
			// Shutting down: hand back the jobs not started, so they don't
			// wait out their lease after a restart. One that was running
			// is retried when its lease runs out.
			release, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := r.DB.ReleaseJobs(release, js[i:]); err != nil {
				r.Log.Error("handing back jobs failed", "err", err)
			}
			cancel()
			break
		}
		h, ok := r.Handlers[j.Kind]
		if !ok {
			err = fmt.Errorf("no handler for %s jobs", j.Kind)
		} else {
			err = safely(ctx, h, j)
		}
		if err == nil {
			done = append(done, j)
			continue
		}
		if ctx.Err() != nil {
			continue // the loop hands back the rest, and this one's lease runs out
		}
		wait := backoff(j.Attempts)
		r.Log.Warn("job failed, will retry", "kind", j.Kind, "job", j.Key, "attempt", j.Attempts, "retry_in", wait, "err", err)
		if err := r.DB.RetryJob(ctx, j, wait, err); err != nil {
			r.Log.Error("rescheduling a job failed", "job", j.Key, "err", err)
		}
	}
	finish := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		finish, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}
	if err := r.DB.FinishJobs(finish, done); err != nil {
		r.Log.Error("finishing jobs failed", "count", len(done), "err", err)
	}
	return true
}

// Drain runs jobs until none are due. Used by tests and one-off commands.
func (r *Runner) Drain(ctx context.Context) {
	for r.RunOne(ctx) {
	}
}

func safely(ctx context.Context, h Handler, j store.Job) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return h(ctx, j)
}
