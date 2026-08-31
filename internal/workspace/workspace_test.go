package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

func TestDiscoverUsesNearestMarkedAncestor(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "src", "package-b")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), []byte("[workspace]\nroot = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := filepath.EvalSymlinks(root)
	if got.Root != wantRoot || got.Workdir != "src/package-b" || got.Project != "src" ||
		got.Source != filepath.Join(wantRoot, markerName) {
		t.Fatalf("Discover() = %+v", got)
	}
}

func TestDiscoverUsesGitWorkspaceNameBelowRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "src", "package-b")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), []byte("[workspace]\nroot = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := filepath.EvalSymlinks(root)
	if got.Project != filepath.Base(wantRoot) || got.Workdir != "src/package-b" {
		t.Fatalf("Discover() = %+v", got)
	}
}

func TestDiscoverMarkerWithoutRootDoesNotSelectAncestor(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "package")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), []byte("[run]\nbackend = 'host'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := filepath.EvalSymlinks(cwd)
	if got.Root != wantRoot || got.Workdir != "" || got.Project != filepath.Base(wantRoot) ||
		got.Source != "current directory" {
		t.Fatalf("Discover() = %+v", got)
	}
}

func TestDiscoverFailsClosedOnMalformedMarker(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "package")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(cwd, ""); err == nil || !strings.Contains(err.Error(), markerName) {
		t.Fatalf("Discover() error = %v", err)
	}
}

func TestDiscoverExplicitRootMustContainCurrentDirectory(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	if _, err := Discover(cwd, root); err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("Discover() error = %v", err)
	}
}

func TestDiscoverExplicitRelativeRoot(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "src", "package")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(cwd, filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := filepath.EvalSymlinks(root)
	if got.Root != wantRoot || got.Workdir != "src/package" || got.Project != "src" ||
		got.Source != "--workspace-root" {
		t.Fatalf("Discover() = %+v", got)
	}
}

func TestDiscoverIgnoresMarkerInWorldWritableAncestor(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), []byte("[workspace]\nroot = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	got, err := Discover(cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := filepath.EvalSymlinks(cwd)
	if got.Root != wantRoot || got.Source != "current directory" {
		t.Fatalf("Discover() trusted world-writable ancestor: %+v", got)
	}
}

func TestDiscoveredRootStillRequiresSnapshotPolicy(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), []byte("[workspace]\nroot = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := Discover(cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshot.SelectFiles(selected.Root); err == nil || !strings.Contains(err.Error(), "no .errandignore") {
		t.Fatalf("marked non-Git root bypassed snapshot policy: %v", err)
	}
}

func TestDiscoverLoadsOutputsFromSelectedMarker(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "src", "package")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[workspace]\nroot = true\n\n[[outputs]]\npath = 'dist/app'\ncollect = 'always'\napply = 'auto'\n"
	if err := os.WriteFile(filepath.Join(root, markerName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := Discover(cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Outputs) != 1 || selected.Outputs[0].Path != "dist/app" ||
		selected.Outputs[0].Collect != "always" || selected.Outputs[0].Apply != "auto" {
		t.Fatalf("selected outputs = %+v", selected.Outputs)
	}
}

func TestDiscoverLoadsOutputsFromCurrentProjectConfigWithoutRootMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, markerName), []byte("[[outputs]]\npath = 'report.json'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := Discover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := canonicalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Root != canonicalRoot || len(selected.Outputs) != 1 || selected.Outputs[0].Path != "report.json" {
		t.Fatalf("Discover() = %+v", selected)
	}
}

func TestDiscoverExplicitRootLoadsItsProjectConfig(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "src")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), []byte("[[outputs]]\npath = 'dist'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	selected, err := Discover(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Outputs) != 1 || selected.Outputs[0].Path != "dist" {
		t.Fatalf("selected outputs = %+v", selected.Outputs)
	}
}
