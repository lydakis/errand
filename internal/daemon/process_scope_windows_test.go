//go:build windows

package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proctree"
)

type cleanupTestJob struct {
	before, after                         []int
	queryErr, afterQueryErr, terminateErr error
	closeErr                              error
	terminated                            bool
	terminations, closes                  int
}

func (j *cleanupTestJob) PIDs() ([]int, error) {
	if j.terminated {
		return j.after, j.afterQueryErr
	}
	return j.before, j.queryErr
}

func (j *cleanupTestJob) Terminate(code uint32) error {
	if code != terminatedExitCode {
		panic("unexpected termination code")
	}
	j.terminations++
	j.terminated = true
	return j.terminateErr
}

func (j *cleanupTestJob) Close() error {
	j.closes++
	return j.closeErr
}

func TestWindowsJobCleanupClosesEveryOutcomeOnWindows(t *testing.T) {
	queryErr, terminateErr, closeErr := errors.New("query failed"), errors.New("termination failed"), errors.New("close failed")
	for _, tc := range []struct {
		name         string
		job          cleanupTestJob
		wantErr      error
		wantPIDs     []int
		terminations int
		timeout      bool
	}{
		{name: "empty"},
		{name: "query-error", job: cleanupTestJob{queryErr: queryErr}, wantErr: queryErr},
		{name: "termination-error", job: cleanupTestJob{before: []int{123}, terminateErr: terminateErr}, wantPIDs: []int{123}, wantErr: terminateErr, terminations: 1},
		{name: "query-error-after-termination", job: cleanupTestJob{before: []int{123}, afterQueryErr: queryErr}, wantPIDs: []int{123}, wantErr: queryErr, terminations: 1},
		{name: "timeout", job: cleanupTestJob{before: []int{123}, after: []int{123}}, wantPIDs: []int{123}, timeout: true, terminations: 1},
		{name: "terminated", job: cleanupTestJob{before: []int{123}}, wantPIDs: []int{123}, terminations: 1},
		{name: "close-error", job: cleanupTestJob{closeErr: closeErr}, wantErr: closeErr},
		{name: "query-and-close-errors", job: cleanupTestJob{queryErr: queryErr, closeErr: closeErr}, wantErr: queryErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := tc.job
			pids, err := cleanupWindowsJob(&job, -time.Second)
			if job.closes != 1 || job.terminations != tc.terminations {
				t.Fatalf("close calls = %d, termination calls = %d; want 1, %d", job.closes, job.terminations, tc.terminations)
			}
			if !slices.Equal(pids, tc.wantPIDs) {
				t.Fatalf("pids = %v, want %v", pids, tc.wantPIDs)
			}
			if tc.timeout {
				if err == nil || !strings.Contains(err.Error(), "process scope still contains pids") {
					t.Fatalf("cleanup error = %v; want timeout", err)
				}
			} else if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("cleanup error = %v; want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if job.closeErr != nil && !errors.Is(err, job.closeErr) {
				t.Fatalf("cleanup error %v lost close error %v", err, job.closeErr)
			}
		})
	}
}

func TestProcessScopeCleanupClearsJobOnWindows(t *testing.T) {
	for _, closed := range []bool{false, true} {
		job, err := proctree.New()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { job.Close() })
		if closed {
			if err := job.Close(); err != nil {
				t.Fatal(err)
			}
		}
		scope := &processScope{job: job}
		_, err = scope.cleanup(time.Second)
		if (err != nil) != closed {
			t.Fatalf("closed=%v: cleanup error = %v", closed, err)
		}
		if scope.job != nil {
			t.Fatal("cleanup retained the native job")
		}
		if _, err := scope.cleanup(time.Second); err != nil {
			t.Fatalf("repeated cleanup: %v", err)
		}
		scope.close()
	}
}

func BenchmarkCleanupEmptyWindowsJob(b *testing.B) {
	job := &cleanupTestJob{}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cleanupWindowsJob(job, time.Second); err != nil {
			b.Fatal(err)
		}
	}
}
