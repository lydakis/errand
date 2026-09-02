package pathpolicy

import (
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

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
	if matchComponents(pattern, name) {
		t.Fatal("non-matching double-star pattern matched")
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
