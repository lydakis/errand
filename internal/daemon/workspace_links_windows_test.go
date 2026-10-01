//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// Directory links reach a Windows job as directory links, including a link
// through another link that is created after it, and one in a subdirectory.
// The Windows client isn't supported yet, so this goes through a job rather
// than a workspace, whose client keeps POSIX-checked local state.
func TestJobDirectoryLinksOnWindows(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"target/value": "body"})
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ name, target string }{{"z", "target"}, {"a", "z"}, {"nested/alias", "../target"}} {
		if err := os.Symlink(link.target, filepath.Join(root, filepath.FromSlash(link.name))); err != nil {
			t.Fatal(err)
		}
	}
	_, status := submitChangeJob(t, d, ts.URL, root,
		[]string{"cmd", "/d", "/c", `type a\value >nul && type nested\alias\value >nul && exit /b 7`})
	result := status.Result
	if result == nil || result.StartError != "" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result = %+v", result)
	}
}
