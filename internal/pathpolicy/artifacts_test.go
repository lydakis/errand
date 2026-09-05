package pathpolicy

import (
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestArtifactValidationAndInputIsolation(t *testing.T) {
	for _, paths := range [][]string{
		{""}, {"."}, {".."}, {"../out"}, {"/out"}, {"a/../out"}, {"./out"},
		{"out/"}, {"out\\file"}, {"out\nfile"}, {"out/*"}, {"out?"}, {"out[1]"},
		{".git"}, {"out/.GiT/config"}, {".errand-change-test"}, {"out", "out"},
		{strings.Repeat("a", MaxPatternBytes+1)}, make([]string, MaxPatterns+1),
	} {
		if err := ValidateArtifacts(paths); err == nil {
			t.Fatalf("accepted invalid artifact list %q", paths)
		}
	}
	policy := proto.SelectionPolicy{Ignore: []string{"out/"}, Artifacts: []string{"out/report", "out/build"}}
	matcher, err := Compile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !matcher.Ignored("out/report", false) {
		t.Fatal("artifact declaration widened input selection")
	}
}
