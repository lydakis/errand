package changes

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestTransferStagePreparedFromBlobs(t *testing.T) {
	for _, failure := range []string{"none", "missing", "corrupt", "quota"} {
		t.Run(failure, func(t *testing.T) {
			root, b, staged := applyFixture(t, "before\n", "after!\n")
			target := transferTarget(t, root)
			s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxSourceBytes: 1 << 20, MaxChangeBytes: 1 << 20}
			if err := s.Initialize(t.Context(), filepath.Join(staged, "base"), b.BaseManifest); err != nil {
				t.Fatal(err)
			}
			if err := s.Blobs().Retain(t.Context(), filepath.Join(staged, "remote"), b.RemoteManifest); err != nil {
				t.Fatal(err)
			}
			p, err := PrepareTransferSource(t.Context(), b.BaseManifest, b.RemoteManifest, s.MaxChangeBytes)
			if err != nil {
				t.Fatal(err)
			}
			// Prove the new path does not depend on the former frozen source tree.
			if err := RemoveTree(staged); err != nil {
				t.Fatal(err)
			}
			blob := filepath.Join(s.Blobs().Directory, b.RemoteManifest.Entries[0].SHA256)
			switch failure {
			case "missing":
				if err := os.Remove(blob); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(blob, []byte("broken\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "quota":
				s.MaxSourceBytes = 1
			}
			id := proto.NewULID()
			dir, delta, err := s.StagePreparedFromBlobs(t.Context(), id, p)
			if failure != "none" {
				if err == nil {
					t.Fatal("invalid blob-backed stage accepted")
				}
				if failure == "quota" && !errors.Is(err, ErrByteLimitExceeded) {
					t.Fatalf("quota: %v", err)
				}
				if _, err := s.Attempt(id); !os.IsNotExist(err) {
					t.Fatalf("failed stage published: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyExtracted(dir, delta); err != nil {
				t.Fatal(err)
			}
			if retry, _, err := s.StagePreparedFromBlobs(t.Context(), id, p); err != nil || retry != dir {
				t.Fatalf("retry: %q, %v", retry, err)
			}
			if err := os.WriteFile(filepath.Join(dir, "remote", "artifact"), []byte("edited\n"), 0600); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(blob)
			if err != nil || string(body) != "after!\n" {
				t.Fatalf("stage edit mutated blob: %q, %v", body, err)
			}
		})
	}
}
