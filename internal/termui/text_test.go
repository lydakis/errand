package termui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"unicode"
)

func TestShellQuoteRoundTrip(t *testing.T) {
	argv := []string{
		"plain", "", "two words", "it's", "日本語",
		"line\n$(printf EXPANDED)", "\t`printf EXPANDED`",
		"quote'\\\"\r\n", "\x1b[2J", "\u0085" + "123", "trailing\n\n",
		"\xff\n", "literal\\n\t123",
	}
	quoted := ShellQuote(argv)
	if strings.IndexFunc(quoted, unicode.IsControl) >= 0 {
		t.Fatalf("ShellQuote emitted a raw control character: %q", quoted)
	}
	want := []byte(strings.Join(argv, "\x00") + "\x00")
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is unavailable", shell)
			}
			got, err := exec.Command(path, "-c", "printf '%s\\0' "+quoted).Output()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("shell argv = %q, want %q (command %s)", got, want, quoted)
			}
		})
	}
}

func BenchmarkShellQuote(b *testing.B) {
	for _, test := range []struct {
		name string
		argv []string
	}{
		{"plain", []string{"go", "test", "./..."}},
		{"spaces", []string{"errand", "fetch", "path with spaces"}},
		{"controls", []string{"errand", "fetch", "line\n$(printf EXPANDED)"}},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				ShellQuote(test.argv)
			}
		})
	}
}
