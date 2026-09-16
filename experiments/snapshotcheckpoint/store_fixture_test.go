//go:build darwin || linux

package snapshotcheckpoint

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

type storedObservations []observation

func (s storedObservations) Len() int             { return len(s) }
func (s storedObservations) At(i int) observation { return s[i] }

func encodeCheckpoint(ctx context.Context, w io.Writer, cp checkpoint) error {
	return encodeRecords(ctx, w, cp.Identity, storedObservations(cp.Entries))
}

// Only malformed-file and codec tests may publish raw observations. Production
// publication accepts snapshot.VerifiedObservations instead.
func writeRawCheckpoint(ctx context.Context, dir string, cp checkpoint) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var data bytes.Buffer
	data.WriteString(magic)
	if err := encodeCheckpoint(ctx, &data, cp); err != nil {
		return 0, err
	}
	sum, err := checkpointChecksum(ctx, data.Bytes())
	if err != nil {
		return 0, err
	}
	data.Write(sum[:])
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, "fixture-")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data.Bytes()); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, "checkpoint")); err != nil {
		return 0, err
	}
	return int64(data.Len()), nil
}

func verifiedFixture(t *testing.T, root string) snapshot.VerifiedObservations {
	t.Helper()
	paths, _, _, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	build, err := snapshot.BuildObservedContext(context.Background(), root, snapshot.PrepareBuildPaths(paths), snapshot.ObservationOptions{Collect: true, MaxBytes: -1, MaxEntries: -1})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := build.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return verified
}
