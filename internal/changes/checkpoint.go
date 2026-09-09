package changes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

var ErrCheckpointChanged = errors.New("transfer checkpoint changed; prepare a new transfer")

// TransferCheckpoint stores the accepted source manifest for one direction.
// SourceID is a stable endpoint identity supplied by the caller, never an alias.
// Root identifies the receiving workspace. The reverse direction uses a separate
// checkpoint. The caller serializes checkpoint and apply operations, checks the
// expected revision before applying, and binds receipts to this relationship.
// StatePath and receipts are private, retained state outside the receiving tree.
// This stores metadata only: callers must retain the source blobs referenced by
// the manifest for constructing subsequent three-way merge bundles.
type TransferCheckpoint struct {
	Root      string
	RootID    fsidentity.Identity
	Owner     string
	SourceID  string
	StatePath string
}

type CheckpointVersion struct {
	Revision uint64         `json:"revision"`
	Manifest proto.Manifest `json:"manifest"`
}

type checkpointState struct {
	Version     int                 `json:"version"`
	Owner       string              `json:"owner"`
	SourceID    string              `json:"source_identity"`
	RootID      fsidentity.Identity `json:"destination_identity"`
	InitialRoot string              `json:"initial_root"`
	CheckpointVersion
	LastReceipt string `json:"last_receipt,omitempty"`
	LastRequest string `json:"last_request,omitempty"`
}

// Stored in the receipt before checkpoint publication so a crash cannot allow
// the same application to be consumed at a different revision or relationship.
type checkpointReceiptBinding struct {
	StatePath   string `json:"state_path"`
	SourceID    string `json:"source_identity"`
	InitialRoot string `json:"initial_root"`
	Revision    uint64 `json:"revision"`
}

// Initialize records the complete shared creation snapshot. Repeating it with
// that snapshot returns current progress; it never resets an advanced checkpoint.
func (c TransferCheckpoint) Initialize(base proto.Manifest) (CheckpointVersion, error) {
	if err := validateCheckpointManifest(base); err != nil {
		return CheckpointVersion{}, err
	}
	destination, storage, name, err := c.open(c.StatePath)
	if err != nil {
		return CheckpointVersion{}, err
	}
	defer destination.Close()
	defer storage.Close()
	state, err := c.read(storage.root, name)
	if err == nil {
		if state.InitialRoot != base.RootHash() {
			return CheckpointVersion{}, fmt.Errorf("checkpoint creation snapshot does not match")
		}
		return state.CheckpointVersion, verifyTransferPaths(destination, storage)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return CheckpointVersion{}, err
	}
	state = checkpointState{Version: 1, Owner: c.Owner, SourceID: c.SourceID, RootID: c.RootID,
		InitialRoot: base.RootHash(), CheckpointVersion: CheckpointVersion{Manifest: base}}
	if err := c.save(destination, storage, name, state); err != nil {
		return CheckpointVersion{}, err
	}
	return state.CheckpointVersion, nil
}

func (c TransferCheckpoint) Read() (CheckpointVersion, error) {
	destination, storage, name, err := c.open(c.StatePath)
	if err != nil {
		return CheckpointVersion{}, err
	}
	defer destination.Close()
	defer storage.Close()
	state, err := c.read(storage.root, name)
	if err != nil {
		return CheckpointVersion{}, err
	}
	return state.CheckpointVersion, verifyTransferPaths(destination, storage)
}

// Advance consumes one completed receipt at expectedRevision. Installed paths
// outside conflicts advance to source values, never merged destination values.
// Conflicted or unselected paths keep their previous base. Even a refusal
// consumes a revision, recording the attempt without changing the manifest.
// Repeating the latest update is idempotent; older updates are refused after
// further progress, even when the manifest happens to have the same digest.
func (c TransferCheckpoint) Advance(expectedRevision uint64, receiptPath string, bundle proto.ChangeBundle) (CheckpointVersion, error) {
	if err := validateBundle(bundle); err != nil {
		return CheckpointVersion{}, err
	}
	destination, storage, name, err := c.open(c.StatePath)
	if err != nil {
		return CheckpointVersion{}, err
	}
	defer destination.Close()
	defer storage.Close()
	state, err := c.read(storage.root, name)
	if err != nil {
		return CheckpointVersion{}, err
	}
	receiver, receipts, receiptName, err := c.open(receiptPath)
	if err != nil {
		return CheckpointVersion{}, err
	}
	defer receiver.Close()
	defer receipts.Close()
	receipt, err := readTransferState(receipts.root, receiptName)
	if err != nil {
		return CheckpointVersion{}, err
	}
	if receipt.Version != 1 || receipt.Owner != c.Owner || receipt.RootID != c.RootID || receipt.BundleRoot != bundle.RootHash() {
		return CheckpointVersion{}, fmt.Errorf("receipt does not belong to this checkpoint destination and bundle")
	}
	if receipt.Outcome == nil || receipt.Pending != "" || receipt.CleanupID != nil {
		return CheckpointVersion{}, fmt.Errorf("finish transfer application and recovery before advancing its checkpoint")
	}
	for i, root := range receipt.Paths {
		if _, ok := slices.BinarySearch(bundle.Paths, root); !ok || i > 0 && receipt.Paths[i-1] >= root {
			return CheckpointVersion{}, fmt.Errorf("invalid receipt selection")
		}
	}
	if err := receipt.validateOutcome(bundle); err != nil {
		return CheckpointVersion{}, err
	}
	if err := verifyTransferPaths(receiver, receipts); err != nil {
		return CheckpointVersion{}, err
	}
	binding := checkpointReceiptBinding{StatePath: filepath.Join(storage.path, name),
		SourceID: c.SourceID, InitialRoot: state.InitialRoot, Revision: expectedRevision}
	if receipt.Checkpoint != nil && *receipt.Checkpoint != binding {
		return CheckpointVersion{}, ErrCheckpointChanged
	}
	unbound := receipt.Checkpoint == nil
	receipt.Checkpoint = &binding
	raw, err := json.Marshal(receipt)
	if err != nil {
		return CheckpointVersion{}, err
	}
	digest := sha256.Sum256(raw)
	request := hex.EncodeToString(digest[:])
	canonicalReceipt := filepath.Join(receipts.path, receiptName)
	if expectedRevision < math.MaxUint64 && state.Revision == expectedRevision+1 && state.LastReceipt == canonicalReceipt && state.LastRequest == request {
		return state.CheckpointVersion, verifyTransferPaths(destination, storage, receipts)
	}
	if state.Revision != expectedRevision || expectedRevision == math.MaxUint64 || state.Manifest.RootHash() != bundle.BaselineRoot {
		return CheckpointVersion{}, ErrCheckpointChanged
	}
	next, err := acceptedSourceManifest(state.Manifest, bundle, *receipt.Outcome)
	if err != nil {
		return CheckpointVersion{}, err
	}
	state.Manifest = next
	state.Revision++
	state.LastReceipt, state.LastRequest = canonicalReceipt, request
	if unbound {
		if err := writeVerifiedTransferRecord(receiver, receipts, receiptName, receipt); err != nil {
			return CheckpointVersion{}, err
		}
	}
	if err := c.save(destination, storage, name, state); err != nil {
		return CheckpointVersion{}, err
	}
	return state.CheckpointVersion, verifyTransferPaths(destination, storage, receipts)
}

func acceptedSourceManifest(base proto.Manifest, bundle proto.ChangeBundle, outcome transferOutcome) (proto.Manifest, error) {
	// BaselineRoot alone is a declaration; also compare the actual merge bases.
	for _, root := range bundle.Paths {
		left, right := subtreeManifest(base, root), subtreeManifest(bundle.BaseManifest, root)
		if bundleHasMetadataPath(bundle, root) {
			left, right = exactManifestEntry(base, root), exactManifestEntry(bundle.BaseManifest, root)
		}
		if !slices.Equal(left.Entries, right.Entries) {
			return proto.Manifest{}, fmt.Errorf("bundle base for %q does not match checkpoint", root)
		}
	}
	if outcome.Refused || len(outcome.States) == 0 {
		return base, nil
	}
	content, metadata := map[string]bool{}, map[string]bool{}
	// States identify roots actually installed, including partially conflicted
	// directories. Their hashes are destination values and are never copied here.
	for root := range outcome.States {
		if bundleHasMetadataPath(bundle, root) {
			metadata[root] = true
		} else {
			content[root] = true
		}
	}
	conflicts := make(map[string]bool, len(outcome.Conflicts))
	for _, conflict := range outcome.Conflicts {
		conflicts[conflict] = true
	}
	replaced := func(name string) bool {
		accepted := metadata[name]
		for current := name; current != "."; current = path.Dir(current) {
			// A separately selected metadata conflict affects only that entry.
			// Installed child roots still contribute their accepted source values.
			if conflicts[current] && (current == name || !bundleHasMetadataPath(bundle, current)) {
				return false
			}
			accepted = accepted || content[current]
		}
		return accepted
	}
	var next proto.Manifest
	for _, entry := range base.Entries {
		if !replaced(entry.Path) {
			next.Entries = append(next.Entries, entry)
		}
	}
	for _, entry := range bundle.RemoteManifest.Entries {
		if replaced(entry.Path) {
			next.Entries = append(next.Entries, entry)
		}
	}
	sort.Slice(next.Entries, func(i, j int) bool { return next.Entries[i].Path < next.Entries[j].Path })
	return next, validateCheckpointManifest(next)
}

func (c TransferCheckpoint) open(statePath string) (*applyDestination, *applyDestination, string, error) {
	if c.Owner == "" || c.SourceID == "" || c.RootID.IsZero() || !filepath.IsAbs(statePath) {
		return nil, nil, "", fmt.Errorf("checkpoint requires owner, source, destination identity, and absolute state path")
	}
	destination, err := openApplyDestinationWithIdentity(c.Root, c.RootID)
	if err != nil {
		return nil, nil, "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(statePath))
	if err != nil {
		destination.Close()
		return nil, nil, "", err
	}
	storage, err := openApplyDestination(parent)
	if err != nil {
		destination.Close()
		return nil, nil, "", err
	}
	storage.requestedPath = filepath.Dir(statePath)
	if err := storage.verifyPath(); err != nil {
		storage.Close()
		destination.Close()
		return nil, nil, "", err
	}
	if err := transferStorageOutsideWorkspace(storage.root, c.RootID); err != nil {
		storage.Close()
		destination.Close()
		return nil, nil, "", err
	}
	return destination, storage, filepath.Base(statePath), nil
}

func (c TransferCheckpoint) read(root *os.Root, name string) (checkpointState, error) {
	var state checkpointState
	if err := readTransferRecord(root, name, &state); err != nil {
		return state, err
	}
	if state.Version != 1 || state.Owner != c.Owner || state.SourceID != c.SourceID || state.RootID != c.RootID {
		return state, fmt.Errorf("checkpoint does not match its recorded relationship")
	}
	if _, err := hex.DecodeString(state.InitialRoot); err != nil || len(state.InitialRoot) != 64 {
		return state, fmt.Errorf("invalid checkpoint creation digest")
	}
	if state.Revision == 0 {
		if state.Manifest.RootHash() != state.InitialRoot || state.LastReceipt != "" || state.LastRequest != "" {
			return state, fmt.Errorf("invalid initial checkpoint")
		}
	} else if _, err := hex.DecodeString(state.LastRequest); err != nil || len(state.LastRequest) != 64 || !filepath.IsAbs(state.LastReceipt) {
		return state, fmt.Errorf("invalid checkpoint application identity")
	}
	return state, validateCheckpointManifest(state.Manifest)
}

func (c TransferCheckpoint) save(destination, storage *applyDestination, name string, state checkpointState) error {
	return writeVerifiedTransferRecord(destination, storage, name, state)
}

func validateCheckpointManifest(manifest proto.Manifest) error {
	if err := archive.Validate(manifest); err != nil {
		return err
	}
	for i, entry := range manifest.Entries {
		if err := validatePath(entry.Path); err != nil {
			return err
		}
		if pathUsesApplyTransaction(entry.Path) || i > 0 && manifest.Entries[i-1].Path >= entry.Path {
			return fmt.Errorf("invalid checkpoint manifest path %q", entry.Path)
		}
	}
	return nil
}
