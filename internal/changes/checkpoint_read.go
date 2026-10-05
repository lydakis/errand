package changes

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/lydakis/errand/internal/proto"
)

// One decoded revision per checkpoint/session, owned under the caller's transfer
// lock. Reuse never certifies live destination files. Every access still opens
// the guarded storage and reads the bounded regular record in full. Exact-byte
// comparison detects in-place edits even when size and timestamps are restored.
// The comparison runs as the record is read, so a match keeps no second copy.
// No global cache, timestamp shortcut, or durability barrier is involved.
type checkpointReadCache struct {
	record *checkpointRecord
}

func (c *TransferCheckpoint) read(root *os.Root, name string) (*checkpointRecord, error) {
	var known *checkpointRecord
	if c.cache != nil {
		known = c.cache.record
	}
	if known == nil {
		known = c.Reuse.get(c.StatePath)
	}
	var knownRaw []byte
	if known != nil {
		knownRaw = known.raw
	}
	raw, same, err := readTransferRecordMatching(root, name, knownRaw)
	if err != nil {
		return nil, err
	}
	if same {
		if err := c.validateRelationship(known.state); err != nil {
			return nil, err
		}
		c.remember(known)
		return known, nil
	}
	if record := c.Reuse.get(c.StatePath); record != nil && record != known && bytes.Equal(record.raw, raw) {
		if err := c.validateRelationship(record.state); err != nil {
			return nil, err
		}
		c.remember(record)
		return record, nil
	}
	var state checkpointState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	record, err := c.validatedRecord(raw, state)
	if err != nil {
		return nil, err
	}
	c.remember(record)
	c.Reuse.put(c.StatePath, record)
	return record, nil
}

// A matching read compares the record in chunks of this size, from buffers
// pooled so that a read that matches allocates nothing in proportion to it.
const recordReadChunk = 64 << 10

var recordReadBuffers = sync.Pool{New: func() any { b := make([]byte, recordReadChunk); return &b }}

// readTransferRecordMatching reads name as readTransferRecordBytes does: a
// regular file, in full, within the same limit. It compares the bytes with
// known while reading. When the file holds exactly known, it returns true and
// keeps no copy. Otherwise it returns the file's bytes: the prefix that matched
// (equal to known's, so taken from it) and the rest as read.
func readTransferRecordMatching(root *os.Root, name string, known []byte) ([]byte, bool, error) {
	if known == nil {
		raw, err := readTransferRecordBytes(root, name)
		return raw, false, err
	}
	f, err := openTransferRecord(root, name)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	r := io.LimitReader(f, MaxBundleMetadataBytes+1)
	pooled := recordReadBuffers.Get().(*[]byte)
	defer recordReadBuffers.Put(pooled)
	buf := *pooled
	for matched := 0; ; {
		n, err := r.Read(buf)
		if n > 0 && (n > len(known)-matched || !bytes.Equal(buf[:n], known[matched:matched+n])) {
			if err != nil && err != io.EOF {
				return nil, false, err
			}
			rest, err := io.ReadAll(r)
			if err != nil {
				return nil, false, err
			}
			if matched+n+len(rest) > MaxBundleMetadataBytes {
				return nil, false, fmt.Errorf("transfer state exceeds size limit")
			}
			raw := make([]byte, 0, matched+n+len(rest))
			return append(append(append(raw, known[:matched]...), buf[:n]...), rest...), false, nil
		}
		matched += n
		if err == io.EOF {
			if matched == len(known) {
				return nil, true, nil
			}
			return bytes.Clone(known[:matched]), false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
}

// validatedRecord applies every check read performs on a decoded record.
func (c *TransferCheckpoint) validatedRecord(raw []byte, state checkpointState) (*checkpointRecord, error) {
	if err := c.validateRelationship(state); err != nil {
		return nil, err
	}
	if _, err := hex.DecodeString(state.InitialRoot); err != nil || len(state.InitialRoot) != 64 {
		return nil, fmt.Errorf("invalid checkpoint creation digest")
	}
	if err := validateCheckpointManifest(state.Manifest); err != nil {
		return nil, err
	}
	record := &checkpointRecord{state: state, raw: raw}
	if state.Revision == 0 {
		if record.rootHash() != state.InitialRoot || state.LastReceipt != "" || state.LastRequest != "" {
			return nil, fmt.Errorf("invalid initial checkpoint")
		}
	} else if _, err := hex.DecodeString(state.LastRequest); err != nil || len(state.LastRequest) != 64 || !filepath.IsAbs(state.LastReceipt) {
		return nil, fmt.Errorf("invalid checkpoint application identity")
	}
	return record, nil
}
func (c *TransferCheckpoint) remember(record *checkpointRecord) {
	if c.cache == nil {
		c.cache = &checkpointReadCache{}
	}
	c.cache.record = record
}

func (c *TransferCheckpoint) validateRelationship(state checkpointState) error {
	if state.Version != 1 || state.Owner != c.Owner || state.SourceID != c.SourceID || state.RootID != c.RootID {
		return fmt.Errorf("checkpoint does not match its recorded relationship")
	}
	return nil
}

// checkpointRecord owns decoded metadata. Session callers use operations rather
// than borrowing its manifest slice. Derived identity is safe to share across
// independent request handles; wire state never contains memoization fields.
type checkpointRecord struct {
	raw   []byte
	state checkpointState
	once  sync.Once
	root  string
}

func (r *checkpointRecord) rootHash() string {
	r.once.Do(func() { r.root = r.state.Manifest.RootHash() })
	return r.root
}

// sourceBase borrows the manifest validatedRecord checked and shares the
// record's identity, so every handle that reads these exact bytes shares one hash.
func (r *checkpointRecord) sourceBase() *SourceBase {
	return &SourceBase{manifest: r.state.Manifest, validate: func() error { return nil }, rootHash: r.rootHash}
}
func (r *checkpointRecord) export() CheckpointVersion { return r.state.export() }
func (r *checkpointRecord) revision() uint64          { return r.state.Revision }
func (r *checkpointRecord) delta(ctx context.Context, source proto.Manifest, limit int64) (proto.ChangeBundle, error) {
	return workspaceDelta(ctx, r.state.Manifest, source, limit)
}
func (r *checkpointRecord) validateBase(ctx context.Context, bundle proto.ChangeBundle) error {
	return validateSourceMergeBases(ctx, r.state.Manifest, bundle)
}

// An existing session only needs to validate the creation relationship. Keep
// the storage guard through that check without reopening and exporting it.
// The creation snapshot's validation and identity are reused when retained.
func (c *TransferCheckpoint) checkInitialized(initial *SourceBase) error {
	destination, storage, name, err := c.open(c.StatePath)
	if err != nil {
		return err
	}
	defer destination.Close()
	defer storage.Close()
	record, err := c.read(storage.root, name)
	if err != nil {
		return err
	}
	if err := initial.validate(); err != nil {
		return err
	}
	if record.state.InitialRoot != initial.rootHash() {
		return fmt.Errorf("checkpoint creation snapshot does not match")
	}
	return verifyTransferPaths(destination, storage)
}
