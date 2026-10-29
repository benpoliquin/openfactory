package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"openfactory/internal/model"
)

// Run supervises stage workers independently. A slow repository command cannot
// hold up another attempt's polling, cancellation, or immutable deadline.
func (e *Engine) Run(ctx context.Context) error {
	e.cycleMu.Lock()
	if e.running {
		e.cycleMu.Unlock()
		return errors.New("engine is already running")
	}
	e.running = true
	e.cycleMu.Unlock()
	workctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	failures := make(chan error, 1)
	defer func() {
		cancel()
		// Drain owned commands before the caller closes the database. Durable BB
		// sessions and external-operation intents remain recoverable on restart.
		workers.Wait()
		e.cycleMu.Lock()
		e.running = false
		e.cycleMu.Unlock()
	}()
	seconds := e.cfg.PollSeconds
	if seconds < 1 {
		seconds = 5
	}
	timer := time.NewTicker(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	launch := func(a model.Attempt, paused bool, stagectx context.Context, stop context.CancelFunc) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer stop()
			err := e.advance(stagectx, a, paused)
			e.cycleMu.Lock()
			delete(e.workers, a.ID)
			e.cycleMu.Unlock()
			if err != nil && stagectx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				select {
				case failures <- err:
				default:
				}
			}
		}()
	}
	for {
		if err := e.cycle(workctx, launch); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-failures:
			return err
		case <-timer.C:
		}
	}
}

type stageLauncher func(model.Attempt, bool, context.Context, context.CancelFunc)

func (e *Engine) cycle(ctx context.Context, launch stageLauncher) error {
	// Keep the active snapshot and worker reservations atomic with completion.
	// Otherwise a just-finished worker could be restarted from a stale snapshot.
	e.cycleMu.Lock()
	defer e.cycleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	paused, err := e.db.Paused(ctx)
	if err != nil {
		return err
	}
	if err := e.coordinateIntakes(ctx); err != nil {
		return err
	}
	if err := e.coordinatePrograms(ctx); err != nil {
		return err
	}
	active, err := e.db.Active(ctx)
	if err != nil {
		return err
	}
	if e.workers == nil {
		e.workers = make(map[string]context.CancelFunc)
	}
	for _, a := range active {
		if stop, busy := e.workers[a.ID]; busy {
			f, err := e.db.Get(ctx, a.FeatureID)
			if err != nil {
				return err
			}
			if f.State == "cancelling" {
				stop()
			}
			continue
		}
		if launch == nil {
			if err := e.advance(ctx, a, paused); err != nil {
				return err
			}
			continue
		}
		stagectx, stop := context.WithCancel(ctx)
		e.workers[a.ID] = stop
		launch(a, paused, stagectx, stop)
	}
	return e.admit(ctx, paused, active)
}
