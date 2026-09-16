//go:build darwin || linux

package snapshotcheckpoint

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Cancels after successful I/O: exercise cancellation during work rather than
// only rejecting a context that was already cancelled at entry.
type cancellingIO struct {
	*bytes.Buffer
	cancel context.CancelFunc
}

func (b cancellingIO) Write(p []byte) (int, error) {
	n, err := b.Buffer.Write(p)
	b.cancel()
	return n, err
}
func (b cancellingIO) Read(p []byte) (int, error) {
	n, err := b.Buffer.Read(p)
	b.cancel()
	return n, err
}

func TestCheckpointCodecCancellationAndBounds(t *testing.T) {
	cp := checkpoint{Entries: make([]observation, codecBatch+1)}
	var data bytes.Buffer
	if err := encodeCheckpoint(context.Background(), &data, cp); err != nil {
		t.Fatal(err)
	}
	got, err := decodeCheckpoint(context.Background(), bytes.NewReader(data.Bytes()))
	if err != nil || len(got.Entries) != len(cp.Entries) {
		t.Fatalf("batch round trip: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := encodeCheckpoint(ctx, cancellingIO{&bytes.Buffer{}, cancel}, cp); !errors.Is(err, context.Canceled) {
		t.Fatalf("encoding ignored cancellation: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	if _, err := decodeCheckpoint(ctx, cancellingIO{bytes.NewBuffer(data.Bytes()), cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("decoding ignored cancellation: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	if _, err := io.Copy(io.Discard, contextReader{ctx, cancellingIO{bytes.NewBufferString("body"), cancel}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("body I/O ignored cancellation: %v", err)
	}
	for _, count := range []int{-1, maxEntries + 1} {
		var oversized bytes.Buffer
		if err := gob.NewEncoder(&oversized).Encode(checkpointHeader{Count: count}); err != nil {
			t.Fatal(err)
		}
		if _, err := decodeCheckpoint(context.Background(), &oversized); err == nil {
			t.Fatal("accepted invalid entry count")
		}
	}
	if _, err := decodeCheckpoint(context.Background(), bytes.NewReader(data.Bytes()[:data.Len()-1])); err == nil {
		t.Fatal("accepted truncated batch")
	}
}

func TestCancelledCacheIOPreservesGeneration(t *testing.T) {
	dir := t.TempDir()
	cp := checkpoint{}
	if _, err := writeCheckpoint(context.Background(), dir, cp); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, status, _, err := readCheckpoint(ctx, dir, cp.Identity, true); !errors.Is(err, context.Canceled) || status != "" {
		t.Fatalf("cancelled load became a cache miss: %q %v", status, err)
	}
	if _, err := writeCheckpoint(ctx, dir, cp); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "checkpoint"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("cancelled write changed generation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".checkpoint.tmp")); !os.IsNotExist(err) {
		t.Fatalf("cancelled write left temporary file: %v", err)
	}
}
