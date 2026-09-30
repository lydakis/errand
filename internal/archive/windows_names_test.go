package archive

import "testing"

func TestWindowsPathRejectsNamesWindowsWouldReadDifferently(t *testing.T) {
	for _, bad := range []string{
		`a\..\..\escape`, "C:x", "file.txt:stream", "dir/CON", "aux.c", "src/nul.txt",
		"COM1", "lpt9.log", "trailing.", "trailing ", "x/GIT~1/config", "git~12",
		"con .txt", "tab\tname", "what?", "star*", `quote"`, "pipe|", "lt<", "gt>",
	} {
		if err := checkWindowsPath(bad); err == nil {
			t.Errorf("checkWindowsPath(%q) accepted it", bad)
		}
	}
	for _, good := range []string{
		"src/main.go", ".github/workflows/ci.yml", "console.log", "auxiliary/x", "COM10",
		"com", "git~", "git~x", "LPT", "a b/c d.txt", "~/tilde", "..hidden", "CONTRIBUTING.md",
	} {
		if err := checkWindowsPath(good); err != nil {
			t.Errorf("checkWindowsPath(%q) = %v", good, err)
		}
	}
}
