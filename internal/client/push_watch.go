package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// PushWatchEvent separates transfer receipts from ephemeral watch status. The
// callback runs serially; returning an error stops the watch after the current
// push finishes. Cancellation also drains an in-flight push, preserving its
// normal recovery contract instead of abandoning an ambiguous apply.
type PushWatchEvent struct {
	State  string
	Result *proto.PushResult
	Stats  TransferStats
	Err    error
}

func WatchPush(ctx context.Context, opts PushOptions, report func(PushWatchEvent) error) error {
	if opts.MaterializeConflicts && !opts.Apply {
		return errors.New("--conflicts requires --apply")
	}
	var ws proto.Workspace
	for delay := time.Second; ; delay = min(2*delay, 10*time.Second) {
		var err error
		ws, err = getWorkspaceDescriptor(opts.PeerURL, opts.Workspace)
		if err == nil {
			break
		}
		if !retryableWatchError(err) {
			return err
		}
		if err := report(PushWatchEvent{State: "reconnecting", Err: err}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
	// Resolve once. Recreating a workspace with the same name must never redirect
	// this watch into its new contents. All subsequent requests address this ID.
	opts.workspace = &ws
	opts.watchState = &pushWatchState{}
	dir, err := workspaceTransferDir(opts.PeerURL, ws.ID)
	if err != nil {
		return err
	}
	origin, err := readWorkspaceOrigin(dir)
	if err != nil {
		return err
	}
	if err := validateApplyCallerWorkspace(origin.Root, opts.Root); err != nil {
		return err
	}
	opts.origin = &origin
	watch, err := snapshot.WatchFiles(origin.Root, snapshot.SelectOptions{IncludeAll: opts.IncludeAll, Caches: ws.Selection.Caches})
	if err != nil {
		return err
	}
	defer watch.Close()
	opts.watchState.watcher = watch
	return runPushWatch(ctx, opts, watch, PushChanges, report)
}

func runPushWatch(ctx context.Context, opts PushOptions, watch *snapshot.Watch, push func(PushOptions) (proto.PushResult, error), report func(PushWatchEvent) error) error {
	const debounce = 5 * time.Millisecond
	const maxBatchDelay = 25 * time.Millisecond
	const resampleDelay = 50 * time.Millisecond
	completed := make(chan PushWatchEvent, 1)
	var busy, dirty bool
	var batchStarted time.Time
	var retryAt time.Time
	var generation uint64
	var sourceRetries int
	retryDelay := time.Second
	timer := time.NewTimer(0) // watcher is already installed before the initial push
	defer timer.Stop()
	defer func() {
		if busy {
			<-completed
		}
	}()
	status := func(state string, err error) error { return report(PushWatchEvent{State: state, Err: err}) }
	for {
		select {
		case <-ctx.Done():
			if busy {
				if err := status("stopping", nil); err != nil {
					return err
				}
				event := <-completed
				busy = false
				if normalizeWatchResult(&event) {
					if err := report(event); err != nil {
						return err
					}
				}
				if event.Err != nil {
					return event.Err
				}
			}
			return status("stopped", nil)
		case err := <-watch.Errors:
			// A watcher failure stops scheduling, but a submitted transaction must
			// still settle and report its receipt before the failure is returned.
			if busy {
				event := <-completed
				busy = false
				if normalizeWatchResult(&event) {
					err = errors.Join(err, report(event))
				}
				err = errors.Join(err, event.Err)
			}
			return err
		case <-watch.Changed:
			dirty = true
			if busy {
				if err := status("pending", nil); err != nil {
					return err
				}
				continue
			}
			if !retryAt.IsZero() {
				continue
			} // saves must not bypass network backoff
			if batchStarted.IsZero() {
				batchStarted = time.Now()
			}
			timer.Reset(max(time.Duration(0), min(debounce, time.Until(batchStarted.Add(maxBatchDelay)))))
		case <-timer.C:
			if ctx.Err() != nil {
				continue
			}
			if busy {
				continue
			}
			busy, dirty = true, false
			retryAt = time.Time{}
			batchStarted = time.Time{}
			generation = watch.Generation()
			if err := status("sending", nil); err != nil {
				busy = false
				return err
			}
			go func() {
				var stats TransferStats
				opts.Stats = &stats
				result, err := push(opts)
				completed <- PushWatchEvent{State: "receipt", Result: &result, Stats: stats, Err: err}
			}()
		case event := <-completed:
			busy = false
			visible := normalizeWatchResult(&event)
			var sourceError *pushSourceError
			if errors.As(event.Err, &sourceError) {
				watch.InvalidatePreparation()
				// Only proven mutation gets unlimited resampling. Unknown errors
				// stay bounded, even when mutations occur between those failures.
				mutation := snapshot.IsSourceChanged(sourceError)
				if mutation || sourceRetries < 3 {
					if !mutation {
						sourceRetries++
					}
					retryAt = time.Now().Add(resampleDelay)
					timer.Reset(resampleDelay)
					if err := status("resampling", nil); err != nil {
						return err
					}
					continue
				}
			}
			if visible {
				if err := report(event); err != nil {
					return err
				}
			}
			if event.Err != nil {
				if sourceError != nil || !retryableWatchError(event.Err) {
					return event.Err
				}
				if err := status("reconnecting", event.Err); err != nil {
					return err
				}
				retryAt = time.Now().Add(retryDelay)
				timer.Reset(retryDelay)
				retryDelay = min(2*retryDelay, 10*time.Second)
				continue
			}
			retryDelay = time.Second
			sourceRetries = 0
			// Recovery completes an older attempt. Always reconcile today's files
			// afterward, even if no filesystem event occurred in this process.
			if dirty || watch.Generation() != generation || event.Result.Recovered {
				timer.Reset(debounce)
			} else if err := status("watching", nil); err != nil {
				return err
			}
		}
	}
}

// The same completion rules apply when Ctrl-C drains an in-flight no-op pass.
func normalizeWatchResult(event *PushWatchEvent) bool {
	if errors.Is(event.Err, errWatchUnchanged) {
		event.Err = nil
		return false
	}
	if errors.Is(event.Err, changes.ErrNoTransferChanges) {
		event.State, event.Err = "unchanged", nil
	}
	return true
}

func retryableWatchError(err error) bool {
	var response *controlHTTPError
	if errors.As(err, &response) {
		return response.statusCode == http.StatusTooManyRequests || response.statusCode == http.StatusBadGateway || response.statusCode == http.StatusServiceUnavailable || response.statusCode == http.StatusGatewayTimeout
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
