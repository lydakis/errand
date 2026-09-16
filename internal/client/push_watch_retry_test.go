package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestWatchSourceRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		edits        int
		causes       []error
		wantSuccess  bool
		wantAttempts int
	}{
		{"mutation without notifications", 0, []error{snapshot.ErrSourceChanged}, true, 6},
		{"mutation with notifications", 5, []error{snapshot.ErrSourceChanged}, true, 6},
		{"unknown failure during edits", 5, []error{errors.New("invalid source")}, false, 4},
		{"unknown failure without edits", 0, []error{errors.New("invalid source")}, false, 4},
		{"permission failure during edits", 5, []error{os.ErrPermission}, false, 4},
		{"alternating disk and mutation failures", 5, []error{syscall.ENOSPC, snapshot.ErrSourceChanged}, false, 7},
		{"joined disk and mutation failure", 5, []error{errors.Join(syscall.ENOSPC, snapshot.ErrSourceChanged)}, false, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			name := filepath.Join(root, "value")
			for _, path := range []string{name, filepath.Join(root, ".errandignore")} {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			watch, err := snapshot.WatchFiles(root, snapshot.SelectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer watch.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			attempts, resamples := 0, 0
			delivered := false
			// Exercise the real scheduling loop with controlled preparation failures.
			// Native source events arrive while resampling, before the next attempt.
			push := func(PushOptions) (proto.PushResult, error) {
				attempts++
				if !tc.wantSuccess || attempts <= 5 {
					return proto.PushResult{}, &pushSourceError{tc.causes[(attempts-1)%len(tc.causes)]}
				}
				return proto.PushResult{}, nil
			}
			err = runPushWatch(ctx, PushOptions{}, watch, push, func(event PushWatchEvent) error {
				if event.State == "receipt" && event.Err == nil {
					delivered = true
					cancel()
				}
				if event.State != "resampling" || resamples >= tc.edits {
					return nil
				}
				resamples++
				before := watch.Generation()
				mode := os.FileMode(0600)
				if resamples%2 == 1 {
					mode = 0640
				}
				if err := os.Chmod(name, mode); err != nil {
					return err
				}
				for watch.Generation() == before {
					select {
					case <-time.After(time.Millisecond):
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			})
			if tc.wantSuccess {
				if err != nil || !delivered || attempts != tc.wantAttempts {
					t.Fatalf("attempts=%d delivered=%v error=%v", attempts, delivered, err)
				}
			} else if !errors.Is(err, tc.causes[0]) || delivered || attempts != tc.wantAttempts {
				t.Fatalf("attempts=%d delivered=%v error=%v", attempts, delivered, err)
			}
		})
	}
}
