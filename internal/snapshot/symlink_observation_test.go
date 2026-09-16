//go:build darwin || linux

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReusedSymlinkKeepsTargetBoundToStamp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink("before", link); err != nil {
		t.Fatal(err)
	}
	first, err := BuildObservedContext(ctx, root, PrepareBuildPaths([]string{"link"}), ObservationOptions{Collect: true, MaxBytes: -1, MaxEntries: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Verify(ctx, root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	build := &ObservedBuild{prior: first.Observations, collect: true}
	prior, err := build.lookup("link", info)
	if err != nil || prior == nil {
		t.Fatalf("lookup: %v %v", prior, err)
	}
	// Replace the link after its observation matched, before reading its target.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("after", link); err != nil {
		t.Fatal(err)
	}
	target, err := readSymlinkTarget(link, prior)
	if err != nil || target != "before" {
		t.Fatalf("mixed new target with old observation: %q %v", target, err)
	}
	// A cache miss must still read the current target.
	target, err = readSymlinkTarget(link, nil)
	if err != nil || target != "after" {
		t.Fatalf("fresh target: %q %v", target, err)
	}
}
