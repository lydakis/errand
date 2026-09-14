package changes

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/lydakis/errand/internal/proto"
)

// One decoded revision per checkpoint/session, owned under the caller's transfer
// lock. Reuse never certifies live destination files. Every access still opens
// the guarded storage and reads the bounded regular record in full. Exact-byte
// comparison detects in-place edits even when size and timestamps are restored.
// No global cache, timestamp shortcut, or durability barrier is involved.
type checkpointReadCache struct {
	record *checkpointRecord
}

func (c *TransferCheckpoint) read(root *os.Root, name string) (*checkpointRecord, error) {
	raw, err := readTransferRecordBytes(root, name)
	if err != nil {
		return nil, err
	}
	if c.cache != nil && c.cache.record != nil && bytes.Equal(c.cache.record.raw, raw) {
		return c.cache.record, c.validateRelationship(c.cache.record.state)
	}
	if record := c.Reuse.get(c.StatePath); record != nil && bytes.Equal(record.raw, raw) {
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
	c.remember(record)
	c.Reuse.put(c.StatePath, record)
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
func (c *TransferCheckpoint) checkInitialized(initial proto.Manifest) error {
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
	if err := validateCheckpointManifest(initial); err != nil {
		return err
	}
	if record.state.InitialRoot != initial.RootHash() {
		return fmt.Errorf("checkpoint creation snapshot does not match")
	}
	return verifyTransferPaths(destination, storage)
}
