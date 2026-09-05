package changes

import (
	"context"
	"github.com/lydakis/errand/internal/proto"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheExclusionWinsOverArtifactAncestors(t *testing.T) {
	root, job := t.TempDir(), t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), root, job, proto.Manifest{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"out/report.txt", "out/cache/object"} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := proto.SelectionPolicy{Artifacts: []string{"out"}, Ignore: []string{"out/"}, Caches: []proto.CacheBinding{{Name: "compiler", Path: "out/cache"}}}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), root, job, proto.Manifest{}, policy, 1<<20)
	if err != nil || !collected {
		t.Fatalf("collect: %v %v", collected, err)
	}
	for _, entry := range bundle.RemoteManifest.Entries {
		if entry.Path == "out/cache" || entry.Path == "out/cache/object" {
			t.Fatalf("cache retained: %+v", entry)
		}
	}
	if len(bundle.RemoteManifest.Entries) != 2 {
		t.Fatalf("report missing: %+v", bundle.RemoteManifest)
	}
}

func TestCacheRetentionPreservesDistinctPathCase(t *testing.T) {
	root, job := t.TempDir(), t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), root, job, proto.Manifest{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Build"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Build", "report.txt"), []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Lstat(filepath.Join(root, "build"))
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatal(statErr)
	}
	policy := proto.SelectionPolicy{Caches: []proto.CacheBinding{{Name: "compiler", Path: "build"}}}
	bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), root, job, proto.Manifest{}, policy, 1<<20)
	if statErr == nil {
		if err == nil {
			t.Fatal("accepted ambiguous cache casing")
		}
		return
	}
	if err != nil || !collected {
		t.Fatalf("collect: %v %v", collected, err)
	}
	for _, entry := range bundle.RemoteManifest.Entries {
		if entry.Path == "Build/report.txt" {
			return
		}
	}
	t.Fatalf("report excluded: %+v", bundle.RemoteManifest)
}
