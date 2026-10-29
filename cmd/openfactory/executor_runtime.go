package main

import (
	"context"
	"errors"
	"reflect"
	"time"

	"openfactory/internal/config"
	"openfactory/internal/executor"
	"openfactory/internal/model"
)

// A policy-file change stops the executor rather than leaving an old profile
// name bound to stale argv. Exact request policy identity is checked by the
// runner as well: polling is lifecycle management, not execution authority.
func serveExperimentExecutor(parent context.Context, path string, policy model.Config, socket, root string, options executor.Options) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- executor.ServeWithOptions(ctx, socket, root, options) }()
	go func() { results <- watchExperimentPolicy(ctx, path, policy) }()
	var first error
	for remaining := 2; remaining > 0; remaining-- {
		err := <-results
		cancel()
		if first == nil && err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, parent.Err()) {
			first = err
		}
	}
	return first
}

func watchExperimentPolicy(ctx context.Context, path string, policy model.Config) error {
	interval := time.Duration(policy.PollSeconds) * time.Second
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			next, err := config.Load(path)
			if err != nil {
				return errors.New("operator configuration became invalid; experiment admission stopped")
			}
			if !reflect.DeepEqual(next.Discovery.ExperimentProfiles, policy.Discovery.ExperimentProfiles) || !reflect.DeepEqual(next.Discovery.ExperimentLimits, policy.Discovery.ExperimentLimits) {
				return errors.New("operator experiment policy changed; executor restart required")
			}
		}
	}
}
