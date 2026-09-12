package changes

import (
	"context"
	"errors"
	"sync"
)

// Bound concurrent disk work and open files per staging operation.
const stagingWorkers = 16

// Every task finishes before return, including after an error. Callers that own
// temporary permissions must restore all members; cancellable copies can skip
// remaining work in their callback after canceling their context.
func runStagingTasks(count int, work func(int) error) error {
	if count == 1 {
		return work(0)
	}
	queue := make(chan int)
	results := make(chan error, min(stagingWorkers, count))
	var workers sync.WaitGroup
	for range min(stagingWorkers, count) {
		workers.Go(func() {
			var err error
			for i := range queue {
				err = errors.Join(err, work(i))
			}
			results <- err
		})
	}
	for i := range count {
		queue <- i
	}
	close(queue)
	workers.Wait()
	close(results)
	var err error
	for result := range results {
		err = errors.Join(err, result)
	}
	return err
}

// Stop starting disk work after the first failure or caller cancellation, but
// still join all in-flight tasks before returning ownership of the staging tree.
func runStagingTasksContext(ctx context.Context, count int, work func(context.Context, int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	_ = runStagingTasks(count, func(i int) error {
		if ctx.Err() != nil {
			return nil
		}
		err := work(ctx, i)
		if err != nil {
			cancel(err)
		}
		return err
	})
	// Cancellation in sibling workers is an implementation detail. Preserve the
	// first cause, so a storage failure is not misreported as caller cancellation.
	return context.Cause(ctx)
}
