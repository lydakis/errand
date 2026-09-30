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
		"notes.txt": "",
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

func TestResolveExecutableSearchesPATHWithExtensionsOnWindows(t *testing.T) {
	t.Setenv("PATHEXT", ".EXE;.CMD")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "npm.cmd"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveExecutable("npm", dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, filepath.Join(dir, "npm.cmd")) {
		t.Fatalf("resolved %s", got)
	}
	if _, err := resolveExecutable("./npm", dir, dir); err != nil {
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
