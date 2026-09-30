//go:build windows

package daemon

import (
	"fmt"
	"testing"
)

// A Mac client's job on a Windows runner: the command is found through
// PATHEXT, its exit code comes back, and the files it wrote are retained.
func TestJobRoundTripOnWindows(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"input.txt": "from the client\n"})
	_, status := submitChangeJob(t, d, ts.URL, root,
		[]string{"cmd", "/d", "/c", "type input.txt && echo remote> report.txt && exit /b 7"})
	result := status.Result
	if result == nil || result.StartError != "" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result = %+v", result)
	}
	if !result.ChangesOK || result.Changes == nil || fmt.Sprint(result.Changes.Paths) != "[report.txt]" {
		t.Fatalf("changes = %+v", result.Changes)
	}
}
