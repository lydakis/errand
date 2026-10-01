package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
	"github.com/lydakis/errand/internal/termui"
)

type progressSink struct {
	writes, bytes atomic.Int64
}

func (s *progressSink) Write(p []byte) (int, error) {
	s.writes.Add(1)
	s.bytes.Add(int64(len(p)))
	return len(p), nil
}

func TestUploadProgressIsRateLimitedAndCompletesOnce(t *testing.T) {
	var sink progressSink
	opts := RunOptions{PeerName: "mini", Display: RunDisplay{UI: termui.New(io.Discard, &sink, termui.Options{ErrTTY: true})}}
	view := newRunView(opts)
	defer view.stopSpin()
	view.shipFiles, view.totalFiles, view.streamSize = 10000, 10000, 512*10000
	view.progress(opts, 0)
	// Hold the display interval open without relying on the test's speed.
	view.lastProgress = time.Now().Add(time.Hour)
	w := countingWriter{w: io.Discard, add: view.uploadProgress(opts)}
	buf := make([]byte, 512)
	for range 9999 {
		if _, err := w.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if view.lastProgressBytes != 0 {
		t.Fatalf("progress rendered inside the display interval: %d", view.lastProgressBytes)
	}
	if allocs := testing.AllocsPerRun(100, func() { view.progress(opts, w.total) }); allocs != 0 {
		t.Fatalf("suppressed progress allocated %g times", allocs)
	}
	view.lastProgress = time.Now().Add(-uploadProgressInterval)
	view.progress(opts, w.total)
	if view.lastProgressBytes != w.total {
		t.Fatalf("next interval didn't render current progress: %d", view.lastProgressBytes)
	}
	// Completion must render even inside the interval, including empty files
	// whose progress is counted in archive bytes rather than content bytes.
	view.lastProgress = time.Now().Add(time.Hour)
	if _, err := w.Write(buf); err != nil {
		t.Fatal(err)
	}
	if view.lastProgressBytes != view.streamSize {
		t.Fatal("completion was suppressed")
	}
	finished := view.lastProgress
	view.progress(opts, view.streamSize)
	view.progress(opts, view.streamSize+512)
	if !view.lastProgress.Equal(finished) {
		t.Fatal("duplicate completion redrew the progress bar")
	}
	view.stopSpin()
	// Include spinner ticks if a heavily loaded test machine took longer.
	maxWrites := int64(time.Since(view.began)/uploadProgressInterval) + 8
	if got := sink.writes.Load(); got > maxWrites {
		t.Fatalf("10000 archive writes produced %d terminal writes, limit %d", got, maxWrites)
	}
	t.Logf("10000 archive writes produced %d terminal writes and %d terminal bytes", sink.writes.Load(), sink.bytes.Load())
}

func TestCacheFallbackRefreshesUploadAccountingAndProgress(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"a": "first file payload", "b": "second file payload"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := snapshot.Build(root, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, initialPartial := range []bool{false, true} {
		for _, mode := range []struct {
			name       string
			tty, quiet bool
		}{
			{"pipe", false, false},
			{"terminal", true, false},
			{"quiet pipe", false, true},
			{"quiet terminal", true, true},
		} {
			t.Run(fmt.Sprintf("partial=%v/%s", initialPartial, mode.name), func(t *testing.T) {
				var attempts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if attempts.Add(1) == 1 {
						w.WriteHeader(http.StatusConflict)
						json.NewEncoder(w).Encode(proto.APIError{Code: proto.ErrorCodeSnapshotCacheMiss, Error: "cached file was evicted"})
						return
					}
					if !bytes.Contains(body, []byte("first file payload")) || !bytes.Contains(body, []byte("second file payload")) {
						t.Error("fallback didn't send both files")
					}
					json.NewEncoder(w).Encode(proto.JobStatus{ID: "same", State: proto.StateRunning})
				}))
				defer server.Close()
				var stderr bytes.Buffer
				opts := RunOptions{PeerName: "mini", PeerURL: server.URL, Root: root, Display: RunDisplay{
					UI: termui.New(io.Discard, &stderr, termui.Options{ErrTTY: mode.tty}), Quiet: mode.quiet, Verbose: true,
				}}
				view := newRunView(opts)
				defer view.stopSpin()
				view.prepared(opts, 2, 37, 0)
				var firstStart time.Time
				plans := 0
				opts.planUpload = func(plan shipPlan) func(int64) {
					view.planned(opts, plan, manifest, 2)
					progress := view.uploadProgress(opts)
					plans++
					if plans == 1 {
						firstStart = view.upStart
					} else if (progress != nil) != (mode.tty && !mode.quiet) {
						t.Error("full retry didn't update progress visibility")
					}
					return progress
				}
				opts.reshipping = view.reshipping
				plan := shipPlan{partial: true}
				if initialPartial {
					plan.hashes = map[string]bool{manifest.Entries[0].SHA256: true}
				}
				spec := proto.Spec{Argv: []string{"true"}, ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits()}
				if _, _, err := submit(opts, "same", spec, manifest, plan); err != nil {
					t.Fatal(err)
				}
				view.admitted(opts, "same", snapshot.GitInfo{}, 2)
				if attempts.Load() != 2 || plans != 2 || view.partial || view.shipFiles != 2 || view.shipBytes != 37 || view.upTime <= 0 {
					t.Fatalf("fallback accounting: attempts=%d plans=%d partial=%v files=%d bytes=%d time=%s", attempts.Load(), plans, view.partial, view.shipFiles, view.shipBytes, view.upTime)
				}
				if initialPartial && !view.upStart.Equal(firstStart) {
					t.Fatal("fallback discarded time spent on the partial upload")
				}
				if strings.Contains(stderr.String(), "already had all") {
					t.Fatalf("full upload reported cached-only transfer: %q", stderr.String())
				}
				if mode.quiet && strings.Contains(stderr.String(), "Uploading") {
					t.Fatal("quiet retry showed progress")
				}
			})
		}
	}
}

func TestRunHintsQuoteURLHandles(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	const peer = "http://runner/build&test/$(printf expanded)"
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, tty := range []bool{false, true} {
		var stderr bytes.Buffer
		opts := RunOptions{Display: RunDisplay{UI: termui.New(io.Discard, &stderr, termui.Options{ErrTTY: tty})}}
		view := newRunView(opts)
		view.detachedLive(peer+"/"+id, id, peer)
		view.detachedBackground(peer+"/"+id, id, peer)
		zero := 0
		view.finished(proto.JobStatus{State: proto.StateExited, Result: &proto.Result{ExitCode: &zero, Started: true, ChangesOK: true, CleanupOK: true, LogsComplete: true}}, peer+"/"+id, peer, id,
			footerChanges{summary: &proto.ChangeSummary{PathCount: 1, Paths: []string{"changed"}}})
		hints := 0
		for _, line := range strings.Split(stderr.String(), "\n") {
			index := strings.Index(line, "errand attach ")
			if index < 0 {
				index = strings.Index(line, "errand fetch --apply ")
			}
			if index < 0 {
				continue
			}
			hints++
			command := line[index:]
			if i := strings.Index(command, " (follow"); i >= 0 {
				command = command[:i]
			} else if i := strings.Index(command, " (bring"); i >= 0 {
				command = command[:i]
			} else if tty {
				command = strings.TrimRight(command, " ")
				for _, why := range []string{"follow it again", "follow the logs", "bring them here"} {
					command = strings.TrimSuffix(command, "  "+why)
				}
			}
			got, err := exec.Command(bash, "-c", "printf '%s\\0' "+command).Output()
			handle := peer + "/" + id
			if tty {
				handle = peer + "/" + termui.ShortID(id)
			}
			if err != nil || !bytes.HasSuffix(got, []byte(handle+"\x00")) {
				t.Fatalf("hint doesn't preserve its handle: %q => %q, %v", command, got, err)
			}
		}
		wantHints := 2
		if tty {
			wantHints = 3
		}
		if hints != wantHints {
			t.Fatalf("run hints = %d, want %d", hints, wantHints)
		}
	}
}

func BenchmarkTTYUploadWriteProgress(b *testing.B) {
	opts := RunOptions{PeerName: "mini", Display: RunDisplay{UI: termui.New(io.Discard, io.Discard, termui.Options{ErrTTY: true})}}
	view := newRunView(opts)
	defer view.stopSpin()
	view.shipFiles, view.totalFiles, view.streamSize = 10000, 10000, 1<<60
	w := countingWriter{w: io.Discard, add: view.uploadProgress(opts)}
	buf := make([]byte, 512)
	b.ReportAllocs()
	b.SetBytes(512)
	for b.Loop() {
		if _, err := w.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func TestUploadProgressOnlyWhenVisible(t *testing.T) {
	manifest := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "sent", Type: proto.EntryFile, Size: 5, Mode: 0600, SHA256: "sent"},
		{Path: "cached", Type: proto.EntryFile, Size: 8, Mode: 0600, SHA256: "cached"},
	}}
	plan := shipPlan{partial: true, hashes: map[string]bool{"sent": true}}
	for _, test := range []struct {
		name                  string
		outTTY, errTTY, quiet bool
	}{
		{"terminal", true, true, false},
		{"piped", false, false, false},
		{"stderr redirected", true, false, false},
		{"quiet terminal", true, true, true},
		{"quiet pipe", false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			con := termui.New(io.Discard, &stderr, termui.Options{OutTTY: test.outTTY, ErrTTY: test.errTTY})
			opts := RunOptions{PeerName: "mini", Display: RunDisplay{UI: con, Quiet: test.quiet}}
			view := newRunView(opts)
			defer view.stopSpin()
			view.prepared(opts, 2, 13, 0)
			view.planned(opts, plan, manifest, 2)
			progress := view.uploadProgress(opts)
			visible := test.errTTY && !test.quiet
			if (progress != nil) != visible {
				t.Fatalf("progress callback present=%v, visible=%v", progress != nil, visible)
			}
			if progress != nil {
				progress(1024)
			} else if allocs := testing.AllocsPerRun(100, func() { view.progress(opts, 1024) }); allocs != 0 {
				t.Fatalf("invisible progress allocated %g times", allocs)
			}
			view.uploaded()
			if view.shipFiles != 1 || view.shipBytes != 5 || view.upTime <= 0 {
				t.Fatalf("upload accounting lost: files=%d bytes=%d time=%s", view.shipFiles, view.shipBytes, view.upTime)
			}
			view.admitted(opts, "job", snapshot.GitInfo{}, 2)
			if !test.errTTY && !test.quiet && !strings.Contains(stderr.String(), "uploaded 1 file (5 B)") {
				t.Fatalf("piped header lost upload counts: %q", stderr.String())
			}
			if test.quiet && stderr.Len() != 0 {
				t.Fatalf("quiet upload printed %q", stderr.String())
			}
		})
	}
}

func BenchmarkNonTTYUploadPlanning(b *testing.B) {
	manifest := proto.Manifest{Entries: make([]proto.ManifestEntry, 100000)}
	for i := range manifest.Entries {
		manifest.Entries[i] = proto.ManifestEntry{Path: fmt.Sprintf("file%06d", i), Type: proto.EntryFile, Size: 1, Mode: 0600}
	}
	for _, quiet := range []bool{false, true} {
		b.Run(fmt.Sprintf("quiet=%v", quiet), func(b *testing.B) {
			opts := RunOptions{PeerName: "mini", Stdout: io.Discard, Stderr: io.Discard, Display: RunDisplay{Quiet: quiet}}
			view := newRunView(opts)
			b.ReportAllocs()
			for b.Loop() {
				view.planned(opts, shipPlan{}, manifest, len(manifest.Entries))
			}
		})
	}
}

func BenchmarkNonTTYUploadWriteProgress(b *testing.B) {
	for _, quiet := range []bool{false, true} {
		b.Run(fmt.Sprintf("quiet=%v", quiet), func(b *testing.B) {
			opts := RunOptions{PeerName: "mini", Stdout: io.Discard, Stderr: io.Discard, Display: RunDisplay{Quiet: quiet}}
			view := newRunView(opts)
			view.shipFiles, view.totalFiles, view.shipBytes, view.streamSize = 100000, 100000, 100000, 102401024
			b.ReportAllocs()
			for b.Loop() {
				view.progress(opts, 65536)
			}
		})
	}
}

func TestWatchQueueStopCancelsPendingProbe(t *testing.T) {
	for _, blockedProbe := range []int{1, 2} {
		t.Run(fmt.Sprintf("probe%d", blockedProbe), func(t *testing.T) {
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls < blockedProbe {
					json.NewEncoder(w).Encode(proto.JobDetails{JobStatus: proto.JobStatus{State: proto.StateQueued}})
					return
				}
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					close(canceled)
				}
			}))
			defer server.Close()
			opts := RunOptions{PeerURL: server.URL, Stdout: io.Discard, Stderr: io.Discard}
			view := newRunView(opts)
			stop := watchQueue(opts, view, "job", proto.JobStatus{State: proto.StateQueued})
			defer func() { close(release); stop() }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("queue status probe never started")
			}
			done := make(chan struct{})
			go func() { stop(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("stopping the queue watcher waited for the status request")
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("the runner's status request was not canceled")
			}
			if blockedProbe == 1 && !view.queued.IsZero() {
				t.Fatal("canceled status probe rendered a stale queue message")
			}
		})
	}
}
