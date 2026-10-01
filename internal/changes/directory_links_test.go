package changes

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestTransferDirectorySymlinksOnWindows(t *testing.T) {
	for _, tc := range []struct {
		link  string
		blobs bool
	}{{"alias", false}, {"nested/alias", false}, {"alias", true}, {"nested/alias", true}} {
		link := tc.link
		t.Run(fmt.Sprintf("%s/blobs=%v", link, tc.blobs), func(t *testing.T) {
			source, destination := t.TempDir(), t.TempDir()
			for _, root := range []string{source, destination} {
				if err := os.Mkdir(filepath.Join(root, "target"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
					t.Fatal(err)
				}
				for _, file := range []string{"target/value", "file"} {
					if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(file)), []byte(file), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			paths := []string{"file", "nested", "target", "target/value"}
			baseline, err := snapshot.Build(source, paths)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := applyWorkspaceIdentity(destination)
			if err != nil {
				t.Fatal(err)
			}
			session := TransferSession{Directory: t.TempDir(), Root: destination, RootID: identity, Owner: "owner", SourceID: "sender", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
			if err := session.Initialize(t.Context(), source, baseline); err != nil {
				t.Fatal(err)
			}
			for i, target := range []string{"target", "file", "target"} {
				name := filepath.Join(source, filepath.FromSlash(link))
				if i != 0 {
					if err := os.Remove(name); err != nil {
						t.Fatal(err)
					}
				}
				if filepath.Dir(link) != "." {
					target = "../" + target
				}
				if err := os.Symlink(target, name); err != nil {
					t.Fatal(err)
				}
				current, err := snapshot.Build(source, append(paths, link))
				if err != nil {
					t.Fatal(err)
				}
				id := proto.NewULID()
				if tc.blobs {
					if err := session.Blobs().Retain(t.Context(), source, current); err != nil {
						t.Fatal(err)
					}
					prepared, err := PrepareTransferSource(t.Context(), baseline, current, 1<<20)
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := session.StagePreparedFromBlobs(t.Context(), id, prepared); err != nil {
						t.Fatal(err)
					}
				} else if _, _, err := session.Stage(t.Context(), id, source, current); err != nil {
					t.Fatal(err)
				}
				if _, err := session.Apply(id, nil, false); err != nil {
					t.Fatal(err)
				}
				baseline = current
				read, want := link, "file"
				if i != 1 {
					read += "/value"
					want = "target/value"
				}
				if body, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(read))); err != nil || string(body) != want {
					t.Fatalf("transition %d: installed link = %q, %v; want %q", i, body, err, want)
				}
			}
		})
	}
}
