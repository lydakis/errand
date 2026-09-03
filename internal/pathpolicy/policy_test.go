package pathpolicy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestMatcherAgreesWithGitCheckIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the differential oracle")
	}
	tests := []struct {
		name      string
		pattern   string
		paths     []string
		directory map[string]bool
	}{
		{name: "trailing dash", pattern: "[a-]", paths: []string{"a", "-", "b"}},
		{name: "leading closing bracket", pattern: "[]a]", paths: []string{"]", "a", "b"}},
		{name: "negated leading closing bracket", pattern: "[!]a]", paths: []string{"]", "a", "b"}},
		{name: "multiple ranges", pattern: "[a-c-e]", paths: []string{"a", "b", "c", "-", "e", "d"}},
		{name: "posix class and literal dash", pattern: "[[:alpha:]-]", paths: []string{"a", "Z", "-", "7"}},
		{name: "unterminated bracket", pattern: "[abc", paths: []string{"[abc", "a", "abc"}},
		{name: "escaped slash", pattern: `a\/b`, paths: []string{"a/b", `a\\/b`}},
		{name: "unicode bytes", pattern: "[é-ê]", paths: []string{"é", "ê", "ë", "e"}},
		{name: "unicode question mark", pattern: "?", paths: []string{"a", "é"}},
		{name: "escaped star", pattern: `foo\*`, paths: []string{"foo*", "foobar"}},
		{name: "escaped backslash", pattern: `f\\oo`, paths: []string{`f\oo`, "foo"}},
		{name: "mixed wildcard", pattern: "*[al]?", paths: []string{"ball", "bell", "bal"}},
		{name: "negated range", pattern: "t[!a-g]n", paths: []string{"ten", "ton"}},
		{name: "closing bracket and dash", pattern: "a[]-]b", paths: []string{"a]b", "a-b", "aab"}},
		{name: "escaped dash", pattern: `[\-_]`, paths: []string{"-", "_", "a"}},
		{name: "escaped closing bracket", pattern: `[\]]`, paths: []string{"]", "["}},
		{name: "escaped backslash class", pattern: `[\\]`, paths: []string{`\`, "a"}},
		{name: "backslash range endpoint", pattern: `[A-\\]`, paths: []string{"G", "a"}},
		{name: "backslash range start", pattern: `[\\-^]`, paths: []string{"]", "["}},
		{name: "escaped numeric range", pattern: `[\1-\3]`, paths: []string{"2", "3", "4"}},
		{name: "escaped bracket endpoint", pattern: `[[\-\]]`, paths: []string{`\`, "[", "]", "-"}},
		{name: "space range", pattern: "[ --]", paths: []string{" ", "$", "-", "0"}},
		{name: "multiple dashes", pattern: "[---]", paths: []string{"-", "a"}},
		{name: "range then dash", pattern: "[a-e-n]", paths: []string{"a", "c", "-", "j"}},
		{name: "invalid POSIX class", pattern: "[[:spaci:]]", paths: []string{"s", "1"}},
		{
			name: "trailing double star", pattern: "dir/**", paths: []string{"dir", "dir/child"},
			directory: map[string]bool{"dir": true},
		},
		{name: "star run between slashes", pattern: "a/***/b", paths: []string{"a/b", "a/x/b", "a/x/y/b"}},
		{
			name: "trailing star run", pattern: "dir/***", paths: []string{"dir", "dir/child"},
			directory: map[string]bool{"dir": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matcher, err := Compile(proto.SelectionPolicy{Ignore: []string{tt.pattern}})
			if err != nil {
				t.Fatalf("Compile(%q): %v", tt.pattern, err)
			}
			for _, name := range tt.paths {
				directory := tt.directory[name]
				want := gitCheckIgnored(t, tt.pattern, name, directory)
				if got := matcher.Ignored(name, directory); got != want {
					t.Errorf("pattern %q path %q directory=%t: Ignored = %t, git = %t", tt.pattern, name, directory, got, want)
				}
			}
		})
	}
}

func gitCheckIgnored(t *testing.T, pattern, name string, directory bool) bool {
	return gitCheckIgnoredWithCaseFold(t, pattern, name, directory, false)
}

func gitCheckIgnoredWithCaseFold(t *testing.T, pattern, name string, directory, caseFold bool) bool {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "config", "core.ignoreCase", fmt.Sprint(caseFold)).CombinedOutput(); err != nil {
		t.Fatalf("git config core.ignoreCase: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(pattern+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localName := filepath.FromSlash(name)
	if directory {
		if err := os.MkdirAll(filepath.Join(root, localName), 0o700); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, localName)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, localName), []byte("value"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "-C", root, "check-ignore", "--no-index", "--quiet", "--", name)
	err := cmd.Run()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git check-ignore pattern %q path %q: %v", pattern, name, err)
	return false
}

func TestMatcherSupportsFrozenCaseFolding(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the differential oracle")
	}
	for _, test := range []struct {
		pattern string
		paths   []string
	}{
		{pattern: "SECRET", paths: []string{"SECRET", "secret", "Secret"}},
		{pattern: "[A-Z]", paths: []string{"A", "a", "1"}},
		{pattern: "[a-z]", paths: []string{"A", "a", "1"}},
		{pattern: "[[:lower:]]", paths: []string{"A", "a"}},
		{pattern: "[[:upper:]]", paths: []string{"A", "a"}},
	} {
		matcher, err := Compile(proto.SelectionPolicy{Ignore: []string{test.pattern}, CaseFold: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range test.paths {
			want := gitCheckIgnoredWithCaseFold(t, test.pattern, name, false, true)
			if got := matcher.Ignored(name, false); got != want {
				t.Errorf("case-folded pattern %q path %q: Ignored = %t, git = %t", test.pattern, name, got, want)
			}
		}
	}
	caseSensitive, err := Compile(proto.SelectionPolicy{Ignore: []string{"SECRET"}})
	if err != nil {
		t.Fatal(err)
	}
	if caseSensitive.Ignored("secret", false) {
		t.Fatal("case-sensitive policy ignored a differently cased path")
	}
}

func TestMatcherSupportsExplicitReinclusion(t *testing.T) {
	matcher, err := Compile(proto.SelectionPolicy{Ignore: []string{
		"target/*",
		"!target/release/",
		"target/release/*",
		"!target/release/atlasctl",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path      string
		directory bool
		ignored   bool
	}{
		{path: "src/main.rs"},
		{path: "target/debug", directory: true, ignored: true},
		{path: "target/release", directory: true},
		{path: "target/release/atlasctl"},
		{path: "target/release/debug-info", ignored: true},
	} {
		if got := matcher.Ignored(test.path, test.directory); got != test.ignored {
			t.Errorf("Ignored(%q) = %t, want %t", test.path, got, test.ignored)
		}
	}
}

func TestMatcherDoesNotReincludeBelowExcludedParent(t *testing.T) {
	matcher, err := Compile(proto.SelectionPolicy{Ignore: []string{
		"build/",
		"!build/keep",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("build/keep", false) {
		t.Fatal("file below excluded parent was re-included")
	}
}

func TestMatcherParentReinclusionDoesNotOverrideChildExclusion(t *testing.T) {
	matcher, err := Compile(proto.SelectionPolicy{Ignore: []string{
		"*.secret",
		"!build/",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if matcher.Ignored("build", true) {
		t.Fatal("re-included parent directory remained ignored")
	}
	if !matcher.Ignored("build/key.secret", false) {
		t.Fatal("parent reinclusion overrode the child-specific exclusion")
	}
	if matcher.Ignored("build/readme.txt", false) {
		t.Fatal("unmatched child below re-included parent was ignored")
	}
}

func TestMatchComponentsHandlesManyDoubleStars(t *testing.T) {
	pattern := make([]string, 13)
	for i := 0; i < len(pattern)-1; i++ {
		pattern[i] = "**"
	}
	pattern[len(pattern)-1] = "never"
	name := make([]string, 48)
	for i := range name {
		name[i] = "segment"
	}
	if matchComponents(pattern, name, false) {
		t.Fatal("non-matching double-star pattern matched")
	}
}

func TestMatchSegmentBoundsWildcardAllocations(t *testing.T) {
	pattern := strings.Repeat("*a", MaxPatternBytes/2-1) + "b"
	name := strings.Repeat("a", 255)
	allocations := testing.AllocsPerRun(1, func() {
		if matchSegment(pattern, name, false) {
			t.Fatal("non-matching wildcard pattern matched")
		}
	})
	if allocations > 2 {
		t.Fatalf("worst-case segment match allocated %.0f objects, want at most 2", allocations)
	}
}

func TestMatcherUsesGitQuestionMarkWildcard(t *testing.T) {
	matcher, err := Compile(proto.SelectionPolicy{Ignore: []string{"foo?"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path    string
		ignored bool
	}{
		{path: "fooa", ignored: true},
		{path: "nested/foo1", ignored: true},
		{path: "foo?", ignored: true},
		{path: "foo", ignored: false},
		{path: "fooab", ignored: false},
	} {
		if got := matcher.Ignored(test.path, false); got != test.ignored {
			t.Errorf("Ignored(%q) = %t, want %t", test.path, got, test.ignored)
		}
	}
	literal, err := Compile(proto.SelectionPolicy{Ignore: []string{`literal\?`, "class[?]"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"literal?", "class?"} {
		if !literal.Ignored(path, false) {
			t.Errorf("escaped or bracketed question mark did not match %q", path)
		}
	}
	for _, path := range []string{"literala", "classa"} {
		if literal.Ignored(path, false) {
			t.Errorf("literal question mark pattern matched %q", path)
		}
	}
}

func TestMatcherPreservesGitCharacterClassesAndEscapedLiterals(t *testing.T) {
	matcher, err := Compile(proto.SelectionPolicy{Ignore: []string{
		"[!a].key",
		"record-[[:digit:]].json",
		`\#secret`,
		`\!important`,
		" leading",
		`trailing\ `,
		"literal+group(1)",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"b.key", "record-7.json", "#secret", "!important", " leading", "trailing ", "literal+group(1)",
	} {
		if !matcher.Ignored(path, false) {
			t.Errorf("Git-compatible pattern did not match %q", path)
		}
	}
	for _, path := range []string{"a.key", "record-x.json", "secret", "important", "leading", "trailing"} {
		if matcher.Ignored(path, false) {
			t.Errorf("Git-compatible pattern unexpectedly matched %q", path)
		}
	}
}

func TestCompileRejectsUnboundedOrMultilinePolicy(t *testing.T) {
	for _, policy := range []proto.SelectionPolicy{
		{Ignore: []string{"first\nsecond"}},
		{Ignore: make([]string, MaxPatterns+1)},
		{Prefix: "../outside"},
	} {
		if _, err := Compile(policy); err == nil {
			t.Fatalf("Compile(%+v) succeeded", policy)
		}
	}
}
