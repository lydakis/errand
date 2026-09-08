package changes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyCumulativeResultWithPreviouslyAppliedContent(t *testing.T) {
	base := "first\nunchanged\nunchanged\nunchanged\nunchanged\nlast\n"
	previous := "FIRST\nunchanged\nunchanged\nunchanged\nunchanged\nlast\n"
	latest := "FIRST\nunchanged\nunchanged\nunchanged\nunchanged\nLAST\n"
	for _, remote := range []string{previous, latest} {
		local, bundle, staged := applyFixture(t, base, remote)
		if err := os.WriteFile(filepath.Join(local, "artifact"), []byte(previous), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := Apply(staged, local, bundle, nil, "test-owner", NewApplyTransaction(), ApplyOptions{})
		if err != nil {
			t.Fatalf("already-applied change caused a conflict: %v", err)
		}
		if err := CommitApply(local, result.Transaction); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(local, "artifact"))
		if err != nil || string(got) != remote {
			t.Fatalf("cumulative apply: %q %v", got, err)
		}
	}
}
