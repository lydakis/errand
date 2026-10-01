package client

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestStagedChangesReadsMetadataWithoutPayloads(t *testing.T) {
	_, bundle, staged := testChangeApplyFixture(t, "old", "new")
	metadata := filepath.Join(staged, "bundle.json")
	if err := writeLocalJSON(metadata, bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStagedBundle(staged); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{"base", "remote"} {
		if err := os.RemoveAll(filepath.Join(staged, tree)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := StagedChanges(staged)
	want := []PathChange{{Path: "artifact", Kind: 'M'}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("display metadata = %+v, %v, want %+v", got, err, want)
	}
	if _, err := loadStagedBundle(staged); err == nil {
		t.Fatal("apply loader accepted missing payloads")
	}
	bundle.V = 0
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := writeLocalJSON(metadata, bundle); err != nil {
		t.Fatal(err)
	}
	if _, err := StagedChanges(staged); err == nil {
		t.Fatal("display loader accepted invalid metadata")
	}
}

func TestAppliedFooterSkipsPayloadReads(t *testing.T) {
	readBytes := func() int64 {
		raw, err := os.ReadFile("/proc/self/io")
		if err != nil {
			t.Skip("process I/O accounting is unavailable")
		}
		var n int64
		if _, err := fmt.Sscanf(string(raw), "rchar: %d", &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	readBytes()
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprintf("quiet=%v", quiet), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			local, bundle, staged := testChangeApplyFixture(t, strings.Repeat("a", 256<<10), strings.Repeat("b", 256<<10))
			if err := writeLocalJSON(filepath.Join(staged, "bundle.json"), bundle); err != nil {
				t.Fatal(err)
			}
			peer, id := "http://runner.test", proto.NewULID()
			if err := saveLocalChangeState(localChangeState{JobID: id, PeerURL: peer, Root: local, ManifestRoot: bundle.BaselineRoot, SubmissionStarted: true, AdmissionConfirmed: true, Terminal: true, ApplyOnSuccess: true, AutomaticApply: automaticApplyApplied, AutomaticApplyDir: staged}); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			opts := RunOptions{PeerURL: peer, Root: local, Stdout: io.Discard, Stderr: &stderr, Display: RunDisplay{Quiet: quiet}}
			view := newRunView(opts)
			zero := 0
			final := proto.JobStatus{ID: id, State: proto.StateExited, Result: &proto.Result{ExitCode: &zero, Started: true, CleanupOK: true, ChangesOK: true, LogsComplete: true, Changes: &proto.ChangeSummary{Paths: bundle.Paths, PathCount: len(bundle.Paths), Bytes: bundle.Bytes}}}
			before := readBytes()
			code := finishTerminalChanges(opts, view, id, peer+"/"+id, final)
			read := readBytes() - before
			if code != 0 || read >= bundle.Bytes {
				t.Fatalf("footer code=%d read=%d payload=%d", code, read, bundle.Bytes)
			}
			if quiet && stderr.Len() != 0 || !quiet && !strings.Contains(stderr.String(), "M artifact") {
				t.Fatalf("applied footer = %q", stderr.String())
			}
			t.Logf("footer read %d bytes for a %d-byte payload", read, bundle.Bytes)
		})
	}
}
