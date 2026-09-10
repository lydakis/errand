package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise setup's PATH decision against real symlink chains without touching
// /usr/local/bin or the machine's installation.
type upgradePathSystem struct {
	*fakeSystem
	link string
}

func (s upgradePathSystem) path(p string) string {
	if p == filepath.Join(pathSymlinkDir, "errand") {
		return s.link
	}
	return p
}
func (s upgradePathSystem) Exists(p string) bool    { return (RealSystem{}).Exists(s.path(p)) }
func (s upgradePathSystem) IsSymlink(p string) bool { return (RealSystem{}).IsSymlink(s.path(p)) }
func (s upgradePathSystem) SameFile(a, b string) bool {
	return (RealSystem{}).SameFile(s.path(a), s.path(b))
}

func TestSetupRecognizesHomebrewSymlinkChain(t *testing.T) {
	root := t.TempDir()
	for _, version := range []string{"0.2.0", "0.2.1"} {
		dir := filepath.Join(root, "Cellar", "errand", version, "bin")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "errand"), []byte(version), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"bin", "opt", "ssh"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, target, path string) {
		t.Helper()
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(root, "bin", "errand")
	ssh := filepath.Join(root, "ssh", "errand")
	opt := filepath.Join(root, "opt", "errand")
	link(t, filepath.Join(root, "opt", "errand", "bin", "errand"), ssh)
	for _, version := range []string{"0.2.0", "0.2.1"} {
		t.Run(version, func(t *testing.T) {
			link(t, "../Cellar/errand/"+version+"/bin/errand", exe)
			link(t, "../Cellar/errand/"+version, opt)
			defer os.Remove(exe)
			defer os.Remove(opt)
			s := upgradePathSystem{newFake(t, "linux"), ssh}
			r := &Report{RemoteCommand: exe}
			ensureOnPath(s, r, exe, true, false)
			if r.RemoteCommand != "" || strings.Contains(stepDetail(r, "path"), "elsewhere") || len(s.writableChecks) != 0 || len(s.symlinks) != 0 {
				t.Fatalf("matching chain was not preserved: %+v", r)
			}
		})
	}
	// A genuinely stale, broken, or cyclic link still needs an explicit command.
	link(t, "../Cellar/errand/0.2.1/bin/errand", exe)
	for _, target := range []string{"../Cellar/errand/0.2.0", "../missing", "errand"} {
		t.Run(target, func(t *testing.T) {
			link(t, target, opt)
			defer os.Remove(opt)
			s := upgradePathSystem{newFake(t, "linux"), ssh}
			r := &Report{RemoteCommand: exe}
			ensureOnPath(s, r, exe, false, false)
			if r.RemoteCommand != exe || !strings.Contains(stepDetail(r, "path"), "points elsewhere") {
				t.Fatalf("unproven chain accepted: %+v", r)
			}
		})
	}
}
