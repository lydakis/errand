package daemon

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestNativeCargoBuildTree(t *testing.T) {
	if os.Getenv("ERRAND_TEST_CACHE_TOOLS") != "1" {
		t.Skip("set ERRAND_TEST_CACHE_TOOLS=1 to exercise installed tools")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo unavailable")
	}
	_, ts := concurrencyDaemon(t, 2, 2)
	files := map[string]string{".gitignore": "target/\n"}
	directory, source := "target", "src/main.rs"
	files["Cargo.toml"] = "[package]\nname = 'tree-fixture'\nversion = '0.1.0'\nedition = '2021'\n"
	files[source] = "fn main() { println!(\"42\"); }\n"
	build := []string{"cargo", "build", "--offline"}
	run := []string{"./target/debug/tree-fixture"}
	root := workspaceWith(t, files)
	opts := client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "compiled", Path: directory}}}
	execute := func(argv []string) string {
		t.Helper()
		var output bytes.Buffer
		opts.Argv, opts.Stdout, opts.Stderr = argv, &output, &output
		if code := client.Run(opts); code != 0 {
			t.Fatalf("%v: %d\n%s", argv, code, &output)
		}
		return output.String()
	}
	execute(build)
	if out := execute(run); !strings.Contains(out, "42\n") {
		t.Fatal(out)
	}
	// A new source snapshot must build from its own relocated tree, using
	// the native invalidation rules, with no hidden configure/build command.
	if err := os.WriteFile(filepath.Join(root, source), []byte(strings.ReplaceAll(files[source], "42", "99")), 0600); err != nil {
		t.Fatal(err)
	}
	execute(build)
	if out := execute(run); !strings.Contains(out, "99\n") {
		t.Fatal(out)
	}
	// Independent persistent workspaces materialize the same saved output.
	a, err := client.CreateWorkspace(opts, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := client.CreateWorkspace(opts, "b")
	if err != nil {
		t.Fatal(err)
	}
	first, second := opts, opts
	first.Workspace, second.Workspace = a.Name, b.Name
	runConcurrentCacheCommands(t, []client.RunOptions{first, second}, run)
}
