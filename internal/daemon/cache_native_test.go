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

// Real tools exercise the same generic directory cache, with no adapter in
// Errand. Tool configuration here belongs to the fixture, just as on a laptop.
func TestNativeInstalledDirectories(t *testing.T) {
	if os.Getenv("ERRAND_TEST_CACHE_TOOLS") != "1" {
		t.Skip("set ERRAND_TEST_CACHE_TOOLS=1 to exercise installed tools")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tool := range []string{"npm", "pnpm", "bun", "uv"} {
		t.Run(tool, func(t *testing.T) {
			if _, err := exec.LookPath(tool); err != nil {
				t.Skipf("%s is unavailable", tool)
			}
			if tool == "npm" {
				if output, err := exec.Command("npm", "--version").CombinedOutput(); err != nil {
					t.Skipf("runner's npm is not runnable before job setup: %v\n%s", err, output)
				}
			}
			_, ts := concurrencyDaemon(t, 2, 2)
			files := map[string]string{".gitignore": "node_modules/\n.venv/\n.cache/\n", ".errandignore": ".cache/\n"}
			var install, verify []string
			directory := "node_modules"
			if tool == "uv" {
				directory = ".venv"
				files["pyproject.toml"] = "[project]\nname = 'cachefixture'\nversion = '0.1.0'\nrequires-python = '>=3.10'\ndependencies = ['tiny==1.0.0']\n[tool.uv.sources]\ntiny = { path = 'tiny-1.0.0-py3-none-any.whl' }\n"
				install = []string{"uv", "sync", "--offline", "--no-managed-python"}
				// The interpreter symlink is relocatable; entry-point scripts with
				// absolute shebangs intentionally are not rewritten by the engine.
				verify = []string{".venv/bin/python", "-c", "import tiny; assert tiny.value == 42"}
			} else {
				files["package.json"] = `{"name":"fixture","private":true,"scripts":{"test":"node verify.cjs"},"dependencies":{"tiny":"file:./tiny.tgz"}}`
				files["verify.cjs"] = `const assert=require('node:assert/strict'); assert.equal(require('tiny'),42); assert.ok(require('node:fs').lstatSync('node_modules').isDirectory());`
				files[".npmrc"] = "fund=false\naudit=false\ncache=.cache/npm\nstore-dir=.cache/pnpm\n"
				install = []string{tool, "install", "--ignore-scripts"}
				if tool != "bun" {
					install = append(install, "--offline")
				}
				verify = []string{tool, "run", "test"}
			}
			root := workspaceWith(t, files)
			if tool == "uv" {
				writeCacheTestWheel(t, root, "1.0.0", 42)
			} else {
				writeCacheTestTarball(t, root, "1.0.0", 42)
			}
			hostHome := t.TempDir()
			sentinel := filepath.Join(hostHome, ".npmrc")
			if err := os.WriteFile(sentinel, []byte("fund=false\n"), 0600); err != nil {
				t.Fatal(err)
			}
			opts := client.RunOptions{PeerURL: ts.URL, Root: root, Caches: []proto.CacheBinding{{Name: "anything", Path: directory}},
				Env: map[string]string{"HOME": hostHome, "XDG_CONFIG_HOME": hostHome, "CI": "1", "COREPACK_ENABLE_NETWORK": "0", "UV_PYTHON_DOWNLOADS": "never", "UV_CACHE_DIR": ".cache/uv", "BUN_INSTALL_CACHE_DIR": ".cache/bun", "PYTHONDONTWRITEBYTECODE": "1"}}
			if tool != "uv" {
				node, err := exec.Command("node", "-p", "process.execPath").Output()
				if err != nil {
					t.Fatal(err)
				}
				opts.Env["PATH"] = filepath.Dir(strings.TrimSpace(string(node))) + string(os.PathListSeparator) + os.Getenv("PATH")
			}
			if tool == "pnpm" {
				entry := os.Getenv("ERRAND_TEST_PNPM_EXECUTABLE")
				if entry == "" {
					home, _ := os.UserHomeDir()
					entry = filepath.Join(home, ".cache/node/corepack/v1/pnpm/10.32.1/bin/pnpm.cjs")
				}
				if _, err := os.Stat(entry); err != nil {
					t.Skip("set ERRAND_TEST_PNPM_EXECUTABLE to an installed pnpm entrypoint")
				}
				bin := filepath.Join(hostHome, "bin")
				if err := os.Mkdir(bin, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(entry, filepath.Join(bin, "pnpm")); err != nil {
					t.Fatal(err)
				}
				opts.Env["PATH"] = bin + string(os.PathListSeparator) + opts.Env["PATH"]
			}
			if tool == "npm" {
				// Resolve before isolating HOME: a version-manager shim may need
				// the real user's configuration to find the installed npm CLI.
				modules, err := exec.Command("npm", "root", "--global").Output()
				if err != nil {
					t.Fatal(err)
				}
				entry := filepath.Join(strings.TrimSpace(string(modules)), "npm/bin/npm-cli.js")
				if _, err := os.Stat(entry); err != nil {
					t.Fatal(err)
				}
				bin := filepath.Join(hostHome, "bin")
				if err := os.Mkdir(bin, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(entry, filepath.Join(bin, "npm")); err != nil {
					t.Fatal(err)
				}
				opts.Env["PATH"] = bin + string(os.PathListSeparator) + opts.Env["PATH"]
			}
			run := func(argv []string) {
				t.Helper()
				var out bytes.Buffer
				opts.Argv, opts.Stdout, opts.Stderr = argv, &out, &out
				if code := client.Run(opts); code != 0 {
					t.Fatalf("%v: %d\n%s", argv, code, &out)
				}
			}
			run(install)
			run(verify)
			runConcurrentCacheCommands(t, []client.RunOptions{opts, opts}, verify)
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
			runConcurrentCacheCommands(t, []client.RunOptions{first, second}, verify)
			if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "fund=false\n" {
				t.Fatalf("host configuration changed: %q %v", contents, err)
			}
		})
	}
}

func TestNamedCachesLeaveEnvironmentUnchanged(t *testing.T) {
	j := newJob(proto.NewULID(), t.TempDir())
	j.Spec.Env = map[string]string{"UV_CACHE_DIR": "user-value", "npm_config_store_dir": "user-store"}
	want := j.buildEnv()
	j.Spec.Selection.Caches = []proto.CacheBinding{{Name: "deps", Path: "node_modules"}, {Name: "packages", Path: ".cache/pnpm"}}
	got := j.buildEnv()
	// Environment map iteration order is immaterial.
	for _, entry := range want {
		found := false
		for _, value := range got {
			found = found || value == entry
		}
		if !found {
			t.Fatalf("environment changed: %s", entry)
		}
	}
	if len(want) != len(got) {
		t.Fatalf("cache added environment variables: %v", got)
	}
}
