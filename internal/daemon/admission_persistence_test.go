package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestSubmitReceiptWriteFailureDoesNotAdmitJob(t *testing.T) {
	for _, receipt := range []string{"spec.json", "admission.json"} {
		t.Run(receipt, func(t *testing.T) {
			d, err := New(Config{StateDir: t.TempDir(), InsecureNoAuth: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			// Fail one receipt, then permit a same-ID retry after storage recovers.
			failed := false
			d.writeAdmissionReceipt = func(j *Job, name string, value any) error {
				if name == receipt && !failed {
					failed = true
					return &os.PathError{Op: "write", Path: filepath.Join(j.Dir, name), Err: syscall.EIO}
				}
				return j.writeJSON(name, value)
			}
			ts := httptest.NewServer(d.Handler())
			t.Cleanup(ts.Close)
			root := workspaceWith(t, nil)
			id := proto.NewULID()
			resp := rawSubmit(t, ts.URL, id, root, []string{"/usr/bin/true"})
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusInternalServerError {
				// Settle an incorrectly admitted job before cleaning up the test daemon.
				if resp.StatusCode == http.StatusCreated {
					waitTerminal(t, ts.URL, id)
				}
				t.Fatalf("failed %s write returned %s, want 500: %s", receipt, resp.Status, body)
			}
			if !strings.Contains(string(body), receipt) {
				t.Fatalf("failure does not identify %s: %s", receipt, body)
			}
			d.mu.Lock()
			jobs, queued, running := len(d.jobs), len(d.queue), len(d.running)
			d.mu.Unlock()
			if jobs != 0 || queued != 0 || running != 0 {
				t.Fatalf("failed admission retained job state: jobs=%d queued=%d running=%d", jobs, queued, running)
			}
			entries, err := os.ReadDir(d.jobsDir())
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("failed admission left receipt or staging directories: %v", entries)
			}

			resp = rawSubmit(t, ts.URL, id, root, []string{"/usr/bin/true"})
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("retry after storage recovery = %s, want 201", resp.Status)
			}
			status := waitTerminal(t, ts.URL, id)
			if status.Result == nil || status.Result.ExitCode == nil || *status.Result.ExitCode != 0 ||
				!status.Result.CleanupOK || !status.Result.ChangesOK || !status.Result.LogsComplete {
				t.Fatalf("retry did not complete successfully: %+v", status.Result)
			}
			for _, name := range []string{"spec.json", "admission.json"} {
				if _, err := os.Stat(filepath.Join(d.jobsDir(), id, name)); err != nil {
					t.Fatalf("retry did not persist %s: %v", name, err)
				}
			}
		})
	}
}
