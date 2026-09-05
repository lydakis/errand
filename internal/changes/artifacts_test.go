package changes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestArtifactRetentionSelectsIgnoredOutputs(t *testing.T) {
	for _, artifact := range []string{"ignored/reports", "ignored/reports/result.txt"} {
		t.Run(artifact, func(t *testing.T) {
			root, job := t.TempDir(), t.TempDir()
			if err := CaptureWorkspaceBaseContext(context.Background(), root, job, proto.Manifest{}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"ordinary.txt", "ignored/reports/result.txt", "ignored/other.txt", "ignored/reports/.git/config"} {
				full := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte("result"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Ignore rules may use the enclosing Git worktree's coordinates;
			// artifact declarations always use the submitted workspace's root.
			policy := proto.SelectionPolicy{Prefix: "packages/api", Ignore: []string{"/packages/api/ignored/"}, Artifacts: []string{artifact}}
			bundle, collected, err := CollectWorkspaceChangesContext(context.Background(), root, job, proto.Manifest{}, policy, 1<<20)
			if err != nil || !collected {
				t.Fatalf("collect: %v %v", collected, err)
			}
			var names []string
			for _, entry := range bundle.RemoteManifest.Entries {
				names = append(names, entry.Path)
			}
			if !slices.Equal(names, []string{"ignored", "ignored/reports", "ignored/reports/result.txt", "ordinary.txt"}) {
				t.Fatalf("retained: %v", names)
			}
			staged := extractTestBundle(t, job, bundle)
			output := filepath.Join(t.TempDir(), "export")
			if err := ExportRemote(staged, output, artifact, bundle); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(output, "ignored/reports/result.txt")); err != nil || string(got) != "result" {
				t.Fatalf("export: %q %v", got, err)
			}
			if _, _, err := CollectWorkspaceChangesContext(context.Background(), root, job, proto.Manifest{}, policy, 5); !errors.Is(err, ErrByteLimitExceeded) {
				t.Fatalf("artifact ignored byte limit: %v", err)
			}
		})
	}
}

func TestMissingArtifactDoesNotRetainIgnoredAncestors(t *testing.T) {
	root, job := t.TempDir(), t.TempDir()
	if err := CaptureWorkspaceBaseContext(context.Background(), root, job, proto.Manifest{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "ignored"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, collected, err := CollectWorkspaceChangesContext(context.Background(), root, job, proto.Manifest{}, proto.SelectionPolicy{Ignore: []string{"ignored/"}, Artifacts: []string{"ignored/missing"}}, 1<<20)
	if err != nil || collected {
		t.Fatalf("missing artifact: %v %v", collected, err)
	}
}
