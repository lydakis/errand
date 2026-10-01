//go:build windows

package daemon

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// A Mac client's job on a Windows runner: the command is found through
// PATHEXT, its exit code comes back, and the files it wrote are retained.
func TestJobRoundTripOnWindows(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"input.txt": "from the client\n"})
	if err := os.Mkdir(filepath.Join(root, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../input.txt", filepath.Join(root, "links", "input")); err != nil {
		t.Fatal(err)
	}
	_, status := submitChangeJob(t, d, ts.URL, root,
		[]string{"cmd", "/d", "/c", `type links\input && echo remote> report.txt && exit /b 7`})
	result := status.Result
	if result == nil || result.StartError != "" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result = %+v", result)
	}
	if !result.ChangesOK || result.Changes == nil || fmt.Sprint(result.Changes.Paths) != "[report.txt]" {
		t.Fatalf("changes = %+v", result.Changes)
	}
}

func TestJobExplicitExecutableWithRestrictedPATHEXTOnWindows(t *testing.T) {
	t.Setenv("PATHEXT", ".EXE")
	executable, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Fatal(err)
	}
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	manifest := proto.Manifest{}
	id := proto.NewULID()
	response := rawSubmitSpec(t, ts.URL, id, root, proto.Spec{
		Argv:         []string{executable, "/d", "/c", "exit /b 7"},
		Env:          map[string]string{"PATHEXT": ".CMD"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(),
	}, manifest)
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("submit = %s: %s", response.Status, body)
	}
	response.Body.Close()
	result := waitTerminal(t, ts.URL, id).Result
	if result == nil || !result.Started || result.StartError != "" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result = %+v", result)
	}
}
