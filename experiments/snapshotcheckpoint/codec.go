//go:build darwin || linux

package snapshotcheckpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
)

// Separate gob messages bound codec work between cancellation checks. This
// remains a full replacement of ordered observations, not an update journal.
const codecBatch = 256

type checkpointHeader struct {
	Identity identity
	Count    int
}

func encodeCheckpoint(ctx context.Context, w io.Writer, cp checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoder := gob.NewEncoder(w)
	if err := encoder.Encode(checkpointHeader{cp.Identity, len(cp.Entries)}); err != nil {
		return err
	}
	for start := 0; start < len(cp.Entries); start += codecBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := encoder.Encode(cp.Entries[start:min(start+codecBatch, len(cp.Entries))]); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func decodeCheckpoint(ctx context.Context, r io.Reader) (checkpoint, error) {
	decoder := gob.NewDecoder(contextReader{ctx, r})
	var header checkpointHeader
	if err := decoder.Decode(&header); err != nil {
		return checkpoint{}, err
	}
	if header.Count < 0 || header.Count > maxEntries {
		return checkpoint{}, fmt.Errorf("checkpoint entry limit")
	}
	cp := checkpoint{Identity: header.Identity, Entries: make([]observation, 0, header.Count)}
	for len(cp.Entries) < header.Count {
		if err := ctx.Err(); err != nil {
			return checkpoint{}, err
		}
		var batch []observation
		if err := decoder.Decode(&batch); err != nil {
			return checkpoint{}, err
		}
		if len(batch) != min(codecBatch, header.Count-len(cp.Entries)) {
			return checkpoint{}, fmt.Errorf("invalid checkpoint batch")
		}
		cp.Entries = append(cp.Entries, batch...)
	}
	var extra checkpointHeader
	err := decoder.Decode(&extra)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return checkpoint{}, ctxErr
	}
	if !errors.Is(err, io.EOF) {
		return checkpoint{}, fmt.Errorf("trailing checkpoint data")
	}
	return cp, ctx.Err()
}

// Cap individual reads even when a consumer asks for an entire gob message.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p[:min(len(p), 64<<10)])
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func checkpointChecksum(ctx context.Context, data []byte) ([sha256.Size]byte, error) {
	h := sha256.New()
	_, err := io.Copy(h, contextReader{ctx, bytes.NewReader(data)})
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, err
}
