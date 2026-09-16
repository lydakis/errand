//go:build darwin || linux

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestObservationBypassDoesNotResolveUnusedRoot(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	want, err := BuildBoundedContext(ctx, ".", nil, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BuildObservedContext(ctx, ".", PrepareBuildPaths(nil), ObservationOptions{MaxBytes: -1, MaxEntries: -1})
	if err != nil || got.Manifest.RootHash() != want.RootHash() {
		t.Fatalf("disabled collection resolved an unused root: %v", err)
	}
}

func TestObservationPublicationConsumesOnlyCompleteVerifiedBuilds(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	file := filepath.Join(root, "a")
	if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := PrepareBuildPaths([]string{"a"})
	build := func() *ObservedBuild {
		t.Helper()
		b, err := BuildObservedContext(ctx, root, paths, ObservationOptions{Collect: true, MaxBytes: -1, MaxEntries: -1})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first := build()
	copied := *first
	// Public manifest metadata must not alias the pending observation batch.
	first.Manifest.Entries[0].SHA256 = "mutated"
	verified, err := first.Verify(ctx)
	if err != nil || !verified.Valid() || verified.Len() != 1 || verified.At(0).Entry.SHA256 == "mutated" {
		t.Fatalf("publication: %v %v", verified, err)
	}
	copy := verified.At(0)
	copy.Entry.SHA256 = "mutated"
	if verified.At(0).Entry.SHA256 == copy.Entry.SHA256 {
		t.Fatal("mutable verified batch")
	}
	if _, err := copied.Verify(ctx); err == nil {
		t.Fatal("copy bypassed consumption")
	}
	if _, err := first.Verify(ctx); err == nil {
		t.Fatal("reused consumed build")
	}
	for _, scenario := range []string{"cancel", "edit"} {
		pending := build()
		c, cancel := context.WithCancel(ctx)
		if scenario == "cancel" {
			cancel()
		} else if err := os.WriteFile(file, []byte("after!"), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := pending.Verify(c)
		cancel()
		if err == nil || got.Valid() {
			t.Fatalf("published after %s", scenario)
		}
		if _, err := pending.Verify(ctx); err == nil {
			t.Fatal("retried failed publication")
		}
	}
	failed, err := BuildObservedContext(ctx, root, paths, ObservationOptions{Collect: true, MaxBytes: 1, MaxEntries: -1})
	if err == nil || failed != nil {
		t.Fatal("partial build escaped")
	}
	bypass, err := BuildObservedContext(ctx, root, paths, ObservationOptions{MaxBytes: -1, MaxEntries: -1})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := bypass.Verify(ctx); err == nil || v.Valid() {
		t.Fatal("published bypass")
	}
	if v, err := new(ObservedBuild).Verify(ctx); err == nil || v.Valid() {
		t.Fatal("published zero build")
	}
	empty, err := BuildObservedContext(ctx, root, PrepareBuildPaths(nil), ObservationOptions{Collect: true, MaxBytes: -1, MaxEntries: -1})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := empty.Verify(ctx); err != nil || !v.Valid() || v.Len() != 0 {
		t.Fatal("empty verified batch rejected")
	}
}
