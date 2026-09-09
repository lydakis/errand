package changes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

// TransferSession integrates staging, receipts, and source checkpoints for one
// direction. Directory is private receiver storage outside Root. The caller
// holds the destination's transfer lock across every operation (including GC).
// This lock does not serialize commands executing in the working tree.
type TransferSession struct {
	Directory, Root, Owner, SourceID string
	RootID                           fsidentity.Identity
	MaxBytes                         int64
}

type TransferAttempt struct {
	ID          string          `json:"id"`
	Revision    uint64          `json:"revision"`
	BundleRoot  string          `json:"bundle_root"`
	SourceRoot  string          `json:"source_root"`
	CreatedAt   time.Time       `json:"created_at"`
	Applying    bool            `json:"applying,omitempty"`
	Done        bool            `json:"done,omitempty"`
	Selected    map[string]bool `json:"selected,omitempty"`
	Materialize bool            `json:"materialize,omitempty"`
}

func (s TransferSession) Checkpoint() TransferCheckpoint {
	return TransferCheckpoint{Root: s.Root, RootID: s.RootID, Owner: s.Owner, SourceID: s.SourceID, StatePath: filepath.Join(s.Directory, "checkpoint.json")}
}
func (s TransferSession) Blobs() TransferBlobStore {
	return TransferBlobStore{Directory: filepath.Join(s.Directory, "blobs"), MaxBytes: s.MaxBytes}
}
func (s TransferSession) Initialize(ctx context.Context, source string, initial proto.Manifest) error {
	if _, err := s.Checkpoint().Read(); err == nil {
		_, err = s.Checkpoint().Initialize(initial)
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, dir := range []string{s.Directory, filepath.Join(s.Directory, "blobs"), filepath.Join(s.Directory, "attempts")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	if err := s.Blobs().Retain(ctx, source, initial); err != nil {
		return err
	}
	_, err := s.Checkpoint().Initialize(initial)
	return err
}

// Stage takes a complete, immutable source snapshot and records a delta against
// the last accepted source. Reusing an ID is allowed only for that same snapshot.
func (s TransferSession) Stage(ctx context.Context, id, source string, manifest proto.Manifest) (string, proto.ChangeBundle, error) {
	if !proto.ValidULID(id) {
		return "", proto.ChangeBundle{}, fmt.Errorf("invalid transfer ID")
	}
	if err := s.Recover(); err != nil {
		return "", proto.ChangeBundle{}, err
	}
	dest := filepath.Join(s.Directory, "attempts", id)
	if a, err := s.Attempt(id); err == nil {
		if a.SourceRoot != manifest.RootHash() {
			return "", proto.ChangeBundle{}, fmt.Errorf("transfer ID reused with another snapshot")
		}
		b, err := ReadTransferBundle(dest)
		return dest, b, err
	} else if !os.IsNotExist(err) {
		return "", proto.ChangeBundle{}, err
	}
	entries, err := os.ReadDir(filepath.Join(s.Directory, "attempts"))
	if err != nil {
		return "", proto.ChangeBundle{}, err
	}
	if len(entries) >= 4096 {
		return "", proto.ChangeBundle{}, fmt.Errorf("transfer staging is full; run gc changes")
	}
	stagedBytes, err := TransferStorageBytes(filepath.Join(s.Directory, "attempts"))
	if err != nil {
		return "", proto.ChangeBundle{}, err
	}
	if stagedBytes > s.MaxBytes {
		return "", proto.ChangeBundle{}, fmt.Errorf("transfer staging budget exceeded; run gc changes")
	}
	v, err := s.Checkpoint().Read()
	if err != nil {
		return "", proto.ChangeBundle{}, err
	}
	b, err := workspaceDelta(ctx, v.Manifest, manifest, s.MaxBytes)
	if err != nil {
		return "", b, err
	}
	tmp, err := os.MkdirTemp(filepath.Join(s.Directory, "attempts"), ".stage-")
	if err != nil {
		return "", b, err
	}
	defer RemoveTree(tmp)
	if err := s.Blobs().MaterializeBase(ctx, tmp, v.Manifest, s.MaxBytes); err != nil {
		return "", b, err
	}
	access, err := makeManifestAccessibleContext(ctx, source, b.RemoteManifest)
	if err != nil {
		return "", b, err
	}
	packErr := commitBundleWithPhysicalModesContext(ctx, filepath.Join(tmp, "change-base"), source, tmp, b, nil, access.physical)
	if err := errors.Join(packErr, access.restore()); err != nil {
		return "", b, err
	}
	if err := extractTransferBundle(tmp, b, s.MaxBytes); err != nil {
		return "", b, err
	}
	if err := RemoveTree(filepath.Join(tmp, "change-base")); err != nil {
		return "", b, err
	}
	if err := RemoveTree(filepath.Join(tmp, BundleDirectory)); err != nil {
		return "", b, err
	}
	a := TransferAttempt{ID: id, Revision: v.Revision, SourceRoot: manifest.RootHash(), BundleRoot: b.RootHash(), CreatedAt: time.Now().UTC()}
	if err := writeTransferJSON(filepath.Join(tmp, "attempt.json"), a); err != nil {
		return "", b, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", b, err
	}
	if err := syncDirectory(filepath.Dir(dest)); err != nil {
		return "", b, err
	}
	return dest, b, nil
}
func extractTransferBundle(dir string, b proto.ChangeBundle, max int64) error {
	for _, name := range []string{"base", "remote"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			return err
		}
	}
	base, err := OpenBaseArchive(dir)
	if err != nil {
		return err
	}
	err = ExtractBase(base, filepath.Join(dir, "base"), b, max)
	err = errors.Join(err, base.Close())
	if err != nil {
		return err
	}
	remote, err := OpenRemoteArchive(dir)
	if err != nil {
		return err
	}
	err = ExtractRemote(remote, filepath.Join(dir, "remote"), b, max)
	err = errors.Join(err, remote.Close())
	if err != nil {
		return err
	}
	if err := SyncTransferSource(filepath.Join(dir, "base"), b.BaseManifest); err != nil {
		return err
	}
	if err := SyncTransferSource(filepath.Join(dir, "remote"), b.RemoteManifest); err != nil {
		return err
	}
	return writeTransferJSON(filepath.Join(dir, "bundle.json"), b)
}
func ReadTransferBundle(dir string) (proto.ChangeBundle, error) {
	var b proto.ChangeBundle
	if err := readTransferJSON(filepath.Join(dir, "bundle.json"), &b); err != nil {
		return b, err
	}
	return b, VerifyExtracted(dir, b)
}
func (s TransferSession) Attempt(id string) (TransferAttempt, error) {
	var a TransferAttempt
	if !proto.ValidULID(id) {
		return a, fmt.Errorf("invalid transfer ID")
	}
	err := readTransferJSON(filepath.Join(s.Directory, "attempts", id, "attempt.json"), &a)
	if err == nil && (a.ID != id || a.SourceRoot == "" || a.BundleRoot == "" || a.CreatedAt.IsZero() || a.Done && !a.Applying) {
		err = fmt.Errorf("invalid transfer attempt")
	}
	return a, err
}
func (s TransferSession) Apply(id string, selected map[string]bool, materialize bool) (ApplyResult, error) {
	a, err := s.Attempt(id)
	if err != nil {
		return ApplyResult{}, err
	}
	dir := filepath.Join(s.Directory, "attempts", id)
	b, err := ReadTransferBundle(dir)
	if err != nil {
		return ApplyResult{}, err
	}
	if b.RootHash() != a.BundleRoot {
		return ApplyResult{}, fmt.Errorf("staged bundle changed after publication")
	}
	if a.Applying && !a.Done {
		v, err := s.Checkpoint().Read()
		if err != nil {
			return ApplyResult{}, err
		}
		if v.Revision != a.Revision {
			if _, err := s.Checkpoint().Advance(a.Revision, filepath.Join(dir, "receipt.json"), b); err != nil {
				return ApplyResult{}, err
			}
		}
	}
	if !a.Applying {
		v, err := s.Checkpoint().Read()
		if err != nil {
			return ApplyResult{}, err
		}
		if v.Revision != a.Revision || v.Manifest.RootHash() != b.BaselineRoot {
			return ApplyResult{}, ErrCheckpointChanged
		}
		if _, err := acceptedSourceManifest(v.Manifest, b, transferOutcome{Refused: true}); err != nil {
			return ApplyResult{}, err
		}
		for p, enabled := range selected {
			if enabled {
				if _, ok := slices.BinarySearch(b.Paths, p); !ok {
					return ApplyResult{}, fmt.Errorf("selected path is not a change root")
				}
			}
		}
		// Source bodies must be durable before any receiver mutation can be accepted.
		if err := s.Blobs().Retain(context.Background(), filepath.Join(dir, "remote"), b.RemoteManifest); err != nil {
			return ApplyResult{}, err
		}
		a.Applying, a.Selected, a.Materialize = true, selected, materialize
		if err := writeTransferJSON(filepath.Join(dir, "attempt.json"), a); err != nil {
			return ApplyResult{}, err
		}
	} else if a.Materialize != materialize || !sameTransferSelection(b, a.Selected, selected) {
		return ApplyResult{}, fmt.Errorf("transfer retry options differ from original application")
	}
	receipt := filepath.Join(dir, "receipt.json")
	target := TransferTarget{Root: s.Root, RootID: s.RootID, Owner: s.Owner, StatePath: receipt}
	result, applyErr := target.Apply(dir, b, a.Selected, ApplyOptions{MaterializeConflicts: a.Materialize})
	var conflict *MergeConflictError
	if applyErr != nil && !errors.As(applyErr, &conflict) {
		return result, applyErr
	}
	if !a.Done {
		if _, err := s.Checkpoint().Advance(a.Revision, receipt, b); err != nil {
			return result, err
		}
		a.Done = true
		if err := writeTransferJSON(filepath.Join(dir, "attempt.json"), a); err != nil {
			return result, err
		}
	}
	return result, applyErr
}
func sameTransferSelection(b proto.ChangeBundle, a, c map[string]bool) bool {
	for _, p := range b.Paths {
		if (a == nil || a[p]) != (c == nil || c[p]) {
			return false
		}
	}
	return true
}
func (s TransferSession) Recover() error {
	entries, err := os.ReadDir(filepath.Join(s.Directory, "attempts"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !proto.ValidULID(e.Name()) {
			continue
		}
		a, err := s.Attempt(e.Name())
		if err != nil {
			return err
		}
		if a.Applying && !a.Done {
			_, err := s.Apply(a.ID, a.Selected, a.Materialize)
			var conflict *MergeConflictError
			if err != nil && !errors.As(err, &conflict) {
				return err
			}
		}
	}
	return nil
}

// SourceManifest reconstructs a job's complete observed source from its immutable
// creation snapshot and retained delta. It never uses the destination's contents.
func SourceManifest(initial proto.Manifest, b proto.ChangeBundle) (proto.Manifest, error) {
	if b.BaselineRoot != initial.RootHash() {
		return proto.Manifest{}, fmt.Errorf("result does not match creation snapshot")
	}
	if err := validateBundle(b); err != nil {
		return proto.Manifest{}, err
	}
	states := map[string]string{}
	for _, p := range b.Paths {
		states[p] = "source"
	}
	return acceptedSourceManifest(initial, b, transferOutcome{States: states})
}

func readTransferJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > MaxBundleMetadataBytes {
		return fmt.Errorf("transfer metadata exceeds limit")
	}
	return json.NewDecoder(f).Decode(v)
}
func writeTransferJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > MaxBundleMetadataBytes {
		return fmt.Errorf("transfer metadata exceeds limit")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".record-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// ObservedSource preserves previously accepted paths outside this job's retention
// policy. Omitting an artifact declaration is not evidence that its file vanished.
func ObservedSource(initial proto.Manifest, b proto.ChangeBundle, previous proto.Manifest, policy proto.SelectionPolicy) (proto.Manifest, error) {
	current, err := SourceManifest(initial, b)
	if err != nil {
		return current, err
	}
	selector, err := newRetentionSelector(initial, policy)
	if err != nil {
		return current, err
	}
	entries := map[string]proto.ManifestEntry{}
	old := map[string]proto.ManifestEntry{}
	for _, e := range current.Entries {
		entries[e.Path] = e
	}
	for _, e := range previous.Entries {
		old[e.Path] = e
	}
	for _, e := range previous.Entries {
		observed, _ := selector.selectKind(e.Path, e.Type == proto.EntryDir)
		for p := path.Dir(e.Path); p != "."; p = path.Dir(p) {
			_, descend := selector.selectKind(p, true)
			if !descend {
				observed = false
			}
		}
		if observed {
			continue
		}
		replaced := false
		for _, p := range b.Paths {
			if !bundleHasMetadataPath(b, p) && (p == e.Path || strings.HasPrefix(e.Path, p+"/")) {
				replaced = true
				break
			}
		}
		if replaced {
			continue
		}
		parents := []proto.ManifestEntry{}
		for p := path.Dir(e.Path); p != "."; p = path.Dir(p) {
			if existing, ok := entries[p]; ok {
				if existing.Type != proto.EntryDir {
					replaced = true
				}
				break
			}
			parent, ok := old[p]
			if !ok {
				return current, fmt.Errorf("missing checkpoint parent")
			}
			parents = append(parents, parent)
		}
		if replaced {
			continue
		}
		if _, exists := entries[e.Path]; !exists {
			entries[e.Path] = e
		}
		for _, parent := range parents {
			entries[parent.Path] = parent
		}
	}
	current.Entries = nil
	for _, e := range entries {
		current.Entries = append(current.Entries, e)
	}
	sort.Slice(current.Entries, func(i, j int) bool { return current.Entries[i].Path < current.Entries[j].Path })
	return current, validateCheckpointManifest(current)
}
