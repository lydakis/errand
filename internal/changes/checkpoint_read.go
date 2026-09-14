package changes

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// One decoded revision per checkpoint/session, owned under the caller's transfer
// lock. Reuse never certifies live destination files. Every access still opens
// the guarded storage and reads the bounded regular record in full. Exact-byte
// comparison detects in-place edits even when size and timestamps are restored.
// No global cache, timestamp shortcut, or durability barrier is involved.
type checkpointReadCache struct {
	raw   []byte
	state *checkpointState
}

func (c *TransferCheckpoint) read(root *os.Root, name string) (checkpointState, error) {
	raw, err := readTransferRecordBytes(root, name)
	if err != nil {
		return checkpointState{}, err
	}
	if c.cache != nil && c.cache.state != nil && bytes.Equal(c.cache.raw, raw) {
		state := *c.cache.state
		return state, c.validateRelationship(state)
	}
	var state checkpointState
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if err := c.validateRelationship(state); err != nil {
		return state, err
	}
	if _, err := hex.DecodeString(state.InitialRoot); err != nil || len(state.InitialRoot) != 64 {
		return state, fmt.Errorf("invalid checkpoint creation digest")
	}
	if err := validateCheckpointManifest(state.Manifest); err != nil {
		return state, err
	}
	state.identity = &checkpointIdentity{}
	if state.Revision == 0 {
		if state.rootHash() != state.InitialRoot || state.LastReceipt != "" || state.LastRequest != "" {
			return state, fmt.Errorf("invalid initial checkpoint")
		}
	} else if _, err := hex.DecodeString(state.LastRequest); err != nil || len(state.LastRequest) != 64 || !filepath.IsAbs(state.LastReceipt) {
		return state, fmt.Errorf("invalid checkpoint application identity")
	}
	if c.cache == nil {
		c.cache = &checkpointReadCache{}
	}
	*c.cache = checkpointReadCache{raw: raw, state: &state}
	return state, nil
}

func (c *TransferCheckpoint) validateRelationship(state checkpointState) error {
	if state.Version != 1 || state.Owner != c.Owner || state.SourceID != c.SourceID || state.RootID != c.RootID {
		return fmt.Errorf("checkpoint does not match its recorded relationship")
	}
	return nil
}

// Derived identity is shared across immutable state copies, under the same
// transfer lock as the record cache. Public reads need not compute it.
type checkpointIdentity struct{ root string }

func (s checkpointState) rootHash() string {
	if s.identity == nil {
		return s.Manifest.RootHash()
	}
	if s.identity.root == "" {
		s.identity.root = s.Manifest.RootHash()
	}
	return s.identity.root
}
