// Package workers runs the in-process background jobs (verify, health,
// federation sync) on a ticker, each under a leader lock so only one
// replica runs a job at a time.
package workers

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/boson-chat/files/internal/leader"
	"github.com/boson-chat/files/internal/logger"
	"github.com/boson-chat/files/metrics"
)

// Worker is one periodic job.
type Worker struct {
	Name  string
	Every time.Duration
	Run   func(ctx context.Context) error
}

type Runner struct {
	locker  leader.LockerImpl
	metrics *metrics.Metrics
}

func NewRunner(l leader.LockerImpl, m *metrics.Metrics) *Runner {
	if m == nil {
		m = metrics.Noop()
	}
	return &Runner{locker: l, metrics: m}
}

// Start runs each worker in its own goroutine until ctx is done. The
// first run is after a short random delay so replicas don't stampede.
func (r *Runner) Start(ctx context.Context, ws ...Worker) {
	for _, w := range ws {
		if w.Every <= 0 || w.Run == nil {
			continue
		}
		go r.loop(ctx, w)
	}
}

func (r *Runner) loop(ctx context.Context, w Worker) {
	delay := time.Duration(rand.Int64N(int64(5 * time.Second)))
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.Once(ctx, w)
		t.Reset(w.Every)
	}
}

// Once runs w now (under its lock).
func (r *Runner) Once(ctx context.Context, w Worker) {
	log := logger.FromCtx(ctx).With().Str("worker", w.Name).Logger()
	start := time.Now()
	ran, err := r.locker.Run(ctx, w.Name, w.Run)
	took := time.Since(start).Seconds()
	switch {
	case err != nil && ctx.Err() == nil:
		log.Warn().Err(err).Msg("worker run failed")
		r.metrics.WorkerRun(ctx, w.Name, "error", took)
	case !ran:
		r.metrics.WorkerRun(ctx, w.Name, "skipped", took)
	case err == nil:
		log.Debug().Float64("seconds", took).Msg("worker run")
		r.metrics.WorkerRun(ctx, w.Name, "ok", took)
	}
}
