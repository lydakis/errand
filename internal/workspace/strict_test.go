package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceRejectsUnknownSettings(t *testing.T) {
	for _, tc := range []struct{ name, body, key string }{
		{"top level", "apply_on_sucess = true", "apply_on_sucess"},
		{"changes", "[changes]\napply_on_sucess = true", "changes.apply_on_sucess"},
		{"root marker", "[workspace]\nrot = true", "workspace.rot"},
		{"run", "[run]\nper = 'build'", "run.per"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, markerName)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, explicit := range []string{"", root} {
				if _, err := Discover(root, explicit); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.key) {
					t.Fatalf("explicit %q: error = %v, want file and unknown key %s", explicit, err, tc.key)
				}
			}
		})
	}
}
