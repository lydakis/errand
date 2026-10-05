package relpath

import (
	"path"
	"testing"
)

func FuzzIsClean(f *testing.F) {
	for _, p := range []string{"", ".", "..", "/", "//", "a", "a/", "/a", "./a", "a/.", "a/..", "../a", "../../a", "a/../b", "/..", "/../a", "a//b", "...", "a/.../b", ".a", "a..", "../..", "../.", ".git/x"} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		clean := path.Clean(p) == p
		if got := IsClean(p); got != clean {
			t.Fatalf("IsClean(%q) = %t, path.Clean gives %q", p, got, path.Clean(p))
		}
		if clean {
			if got, want := Dir(p), path.Dir(p); got != want {
				t.Fatalf("Dir(%q) = %q, want %q", p, got, want)
			}
		}
	})
}

// Every path of up to six bytes over the bytes Clean treats specially.
func TestIsCleanMatchesPathCleanExhaustively(t *testing.T) {
	alphabet := []byte{'/', '.', 'a'}
	var visit func([]byte)
	visit = func(p []byte) {
		s := string(p)
		clean := path.Clean(s) == s
		if IsClean(s) != clean {
			t.Fatalf("IsClean(%q) = %t, path.Clean gives %q", s, !clean, path.Clean(s))
		}
		if clean && Dir(s) != path.Dir(s) {
			t.Fatalf("Dir(%q) = %q, want %q", s, Dir(s), path.Dir(s))
		}
		if len(p) == 6 {
			return
		}
		for _, b := range alphabet {
			visit(append(p, b))
		}
	}
	visit(nil)
}
