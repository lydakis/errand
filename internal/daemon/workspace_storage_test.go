package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceStorageDoesNotFollowSharedCacheLinks(t *testing.T) {
	root, cache := t.TempDir(), t.TempDir()
	for name, content := range map[string]string{"data/file": "abc", "change-base/file": "old", "workspace.json": "{}"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cache, "large"), make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cache, filepath.Join(root, "data", "target")); err != nil {
		t.Fatal(err)
	}
	usage, err := workspaceStorageBytes(t.Context(), root, workspaceRecord{})
	if err != nil || usage.WorkingBytes != 3 || usage.BaseBytes != 3 || usage.MetadataBytes != 2 || usage.Bytes != 8 {
		t.Fatalf("workspace storage counted external cache: %+v %v", usage, err)
	}
}
