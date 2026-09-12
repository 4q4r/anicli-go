package netclient

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Parallel runs fn over items with at most limit concurrent invocations,
// for provider fan-out (config Network.MaxParallel). A limit <= 0 runs
// unbounded: errgroup.SetLimit(0) would permit no goroutines at all (only
// negative means unlimited there), so zero is treated as "no cap" here to
// keep the usual Go semantics. The context passed to fn is canceled as
// soon as any invocation fails, and Parallel returns the first error.
func Parallel[T any](ctx context.Context, items []T, limit int, fn func(context.Context, T) error) error {
	g, ctx := errgroup.WithContext(ctx)
	if limit > 0 {
		g.SetLimit(limit)
	}
	for _, item := range items {
		g.Go(func() error {
			return fn(ctx, item)
		})
	}
	return g.Wait()
}
