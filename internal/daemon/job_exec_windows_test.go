//go:build windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindExecutableAppliesPATHEXTOnWindows(t *testing.T) {
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	dir := t.TempDir()
	for _, name := range []string{"tool.exe", "shim.cmd", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{
		"tool":      "tool.exe",
		"tool.exe":  "tool.exe",
		"shim":      "shim.cmd",
		"notes.txt": "notes.txt",
		"notes":     "",
		"missing":   "",
	} {
		got, ok := findExecutable(filepath.Join(dir, name))
		if want == "" {
			if ok {
				t.Errorf("findExecutable(%s) = %s, want none", name, got)
			}
			continue
		}
		if !ok || !strings.EqualFold(got, filepath.Join(dir, want)) {
			t.Errorf("findExecutable(%s) = %s, %v, want %s", name, got, ok, want)
		}
	}
}

func TestExplicitExecutableIgnoresPATHEXTOnWindows(t *testing.T) {
	dir := t.TempDir()
	// The competing script must never shadow an explicitly named executable.
	for _, name := range []string{"tool.exe", "tool.exe.cmd", "fallback.exe.cmd"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	const pathext = ".CMD"
	for _, tc := range []struct{ name, want string }{
		{name: "tool.exe", want: "tool.exe"},
		{name: "TOOL.EXE", want: "tool.exe"},
		{name: "fallback.exe", want: "fallback.exe.cmd"},
		{name: "directory.exe"},
		{name: "missing.exe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{tc.name, "./" + tc.name, filepath.Join(dir, tc.name)} {
				got, err := resolveExecutable(name, dir, dir, pathext)
				if tc.want == "" {
					if err == nil {
						t.Fatalf("resolve(%q) = %q; want not found", name, got)
					}
					continue
				}
				if err != nil || !strings.EqualFold(got, filepath.Join(dir, tc.want)) {
					t.Fatalf("resolve(%q) = %q, %v; want %q", name, got, err, tc.want)
				}
			}
			got := placementTool(tc.name, []string{"PATH=" + dir, "PATHEXT=" + pathext})
			want := ""
			if tc.want != "" {
				want = filepath.Join(dir, tc.want)
			}
			if !strings.EqualFold(got, want) {
				t.Fatalf("placement(%q) = %q; want %q", tc.name, got, want)
			}
		})
	}
}

func TestResolveExecutableSearchesPATHWithExtensionsOnWindows(t *testing.T) {
	t.Setenv("PATHEXT", ".EXE;.CMD")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "npm.cmd"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveExecutable("npm", dir, t.TempDir(), os.Getenv("PATHEXT"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, filepath.Join(dir, "npm.cmd")) {
		t.Fatalf("resolved %s", got)
	}
	if _, err := resolveExecutable("./npm", dir, dir, os.Getenv("PATHEXT")); err != nil {
		t.Fatalf("relative path with forward slash: %v", err)
	}
}

func TestEnvValueIgnoresNameCaseOnWindows(t *testing.T) {
	env := []string{"Path=C:\\one", "SYSTEMROOT=C:\\Windows"}
	if got := envValue(env, "PATH"); got != `C:\one` {
		t.Fatalf("PATH = %q", got)
	}
	if got := envValue(env, "SystemRoot"); got != `C:\Windows` {
		t.Fatalf("SystemRoot = %q", got)
	}
}

func TestExecutableLookupUsesJobPATHEXTOnWindows(t *testing.T) {
	t.Setenv("PATHEXT", ".EXE")
	dir := t.TempDir()
	for _, name := range []string{"tool.exe", "tool.cmd"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ extensions, want string }{
		{extensions: ".CMD", want: "tool.cmd"},
		{extensions: ".CMD;.EXE", want: "tool.cmd"},
		{extensions: ".EXE;.CMD", want: "tool.exe"},
	} {
		t.Run(tc.extensions, func(t *testing.T) {
			j := &Job{}
			j.Spec.Env = map[string]string{"Path": dir, "PathExt": tc.extensions}
			env := j.buildEnv()
			got, err := resolveExecutable("tool", envValue(env, "PATH"), dir, envValue(env, "PATHEXT"))
			want := filepath.Join(dir, tc.want)
			if err != nil || !strings.EqualFold(got, want) {
				t.Fatalf("resolve = %q, %v; want %q", got, err, want)
			}
			if got := placementTool("tool", env); !strings.EqualFold(got, want) {
				t.Fatalf("placement = %q; want %q", got, want)
			}
		})
	}
}

func TestCheckCommandLineRefusesBatchMetacharactersOnWindows(t *testing.T) {
	if err := checkCommandLine(`C:\bin\npm.cmd`, []string{"run", "build", `C:\Program Files (x86)\x`}); err != nil {
		t.Fatalf("plain batch arguments refused: %v", err)
	}
	for _, arg := range []string{"a&b", "a|b", "%PATH%", `say "hi"`, "a^b", "x>y"} {
		if err := checkCommandLine(`C:\bin\npm.CMD`, []string{arg}); err == nil {
			t.Errorf("batch argument %q accepted", arg)
		}
		if err := checkCommandLine(`C:\bin\cargo.exe`, []string{arg}); err != nil {
			t.Errorf("exe argument %q refused: %v", arg, err)
		}
	}
}
