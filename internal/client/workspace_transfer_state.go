package client

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

// Every push reads the origin of each relationship to find those bound to its
// checkout, so the record stays small. The creation manifest it names lives in
// initial.json, which only job application and GC read.
type workspaceOrigin struct {
	Root        string              `json:"root"`
	RootID      fsidentity.Identity `json:"root_identity"`
	WorkspaceID string              `json:"workspace_id"`
	PeerURL     string              `json:"peer_url"`
	InitialRoot string              `json:"initial_root"`
}

func workspaceTransferDir(peer, id string) (string, error) {
	root, err := localChangeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "workspace-transfers", localChangeKey(peer, id)), nil
}
func readWorkspaceOrigin(dir string) (workspaceOrigin, error) {
	var record struct {
		workspaceOrigin
		Embedded *struct{} `json:"initial"` // earlier clients embedded the creation manifest
	}
	err := readWorkspaceTransferRecord(filepath.Join(dir, "origin.json"), "workspace origin", &record)
	o := record.workspaceOrigin
	switch {
	case err != nil:
	case !proto.ValidULID(o.WorkspaceID) || o.RootID.IsZero() || !filepath.IsAbs(o.Root) || o.PeerURL == "":
		err = fmt.Errorf("invalid workspace origin")
	case o.InitialRoot == "" && record.Embedded != nil:
		err = &EarlierTransferStateError{Dir: dir, WorkspaceID: o.WorkspaceID}
	case !validLocalManifestRoot(o.InitialRoot):
		err = fmt.Errorf("invalid workspace origin")
	}
	return o, err
}

// EarlierTransferStateError reports a relationship recorded by an earlier
// errand, whose origin embedded the creation manifest. It is never migrated:
// the workspace is recreated instead. Callers that resolved the workspace
// name, peer or profile fill them in so the printed command runs as shown.
type EarlierTransferStateError struct {
	Dir, WorkspaceID, Workspace string
	Peer, URL, Profile          string
}

func (e *EarlierTransferStateError) Error() string {
	peer := "--on PEER"
	if e.URL != "" {
		peer = "--url " + shellQuote(e.URL)
	} else if e.Peer != "" {
		peer = "--on " + e.Peer
	}
	create := peer
	if e.Profile != "" {
		create += " --profile " + shellQuote(e.Profile)
	}
	return fmt.Sprintf("workspace %s was created by an earlier errand, and this version cannot read its local transfer state; "+
		"recreate it from its checkout with: errand workspaces rm %s %s && errand workspaces create %s %s && rm -r %s",
		cmp.Or(e.Workspace, e.WorkspaceID), peer, e.WorkspaceID, create, cmp.Or(e.Workspace, "NAME"), shellQuote(e.Dir))
}

// initial reads the creation manifest. The root check binds it to this origin,
// so a damaged or foreign file is rejected rather than used.
func (o workspaceOrigin) initial(dir string) (proto.Manifest, error) {
	var m proto.Manifest
	if err := readWorkspaceTransferRecord(filepath.Join(dir, "initial.json"), "workspace creation manifest", &m); err != nil {
		return proto.Manifest{}, err
	}
	if m.RootHash() != o.InitialRoot {
		return proto.Manifest{}, fmt.Errorf("workspace creation manifest does not match its origin")
	}
	return m, nil
}

func readWorkspaceTransferRecord(path, what string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > changeops.MaxBundleMetadataBytes {
		return fmt.Errorf("%s exceeds metadata limit", what)
	}
	return json.NewDecoder(f).Decode(v)
}

// rootMoved reports whether the recorded workspace path no longer holds the
// directory that created this relationship, because it was deleted, moved, or
// replaced. Transfer state cannot be opened until that directory returns.
func (o workspaceOrigin) rootMoved() (bool, error) {
	identity, info, err := fsidentity.Lstat(o.Root)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || identity != o.RootID, nil
}
func (o workspaceOrigin) session(dir string) *changeops.TransferSession {
	return &changeops.TransferSession{Directory: filepath.Join(dir, "fetch"), Root: o.Root, RootID: o.RootID, Owner: localChangeKey(o.PeerURL, o.WorkspaceID), SourceID: o.WorkspaceID, MaxSourceBytes: proto.DefaultLimits().MaxWorkspaceBytes, MaxChangeBytes: proto.DefaultLimits().MaxChangeBytes}
}
func recordWorkspaceOrigin(opts RunOptions, id string, m proto.Manifest) error {
	dir, err := workspaceTransferDir(opts.PeerURL, id)
	if err != nil {
		return err
	}
	return withWorkspaceChangeLock(opts.Root, func() error {
		unlock, err := lockWorkspaceTransfer(dir)
		if err != nil {
			return err
		}
		defer unlock()
		rootID, info, err := fsidentity.Lstat(opts.Root)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace origin must be a real directory")
		}
		if err := ensurePrivateLocalDirectory(dir); err != nil {
			return err
		}
		o := workspaceOrigin{Root: opts.Root, RootID: rootID, WorkspaceID: id, PeerURL: opts.PeerURL, InitialRoot: m.RootHash()}
		tmp, err := os.MkdirTemp(dir, ".initial-")
		if err != nil {
			return err
		}
		defer changeops.RemoveTree(tmp)
		source := filepath.Join(tmp, "source")
		if err := changeops.CopyTransferSource(context.Background(), opts.Root, source, m, proto.DefaultLimits().MaxWorkspaceBytes); err != nil {
			return err
		}
		if err := o.session(dir).Initialize(context.Background(), source, m); err != nil {
			return err
		}
		// The origin marks a complete relationship, so the manifest it names
		// must be durable first. Without an origin, GC collects the directory.
		if err := writeLocalJSON(filepath.Join(dir, "initial.json"), m); err != nil {
			return err
		}
		if err := syncLocalDirectory(dir); err != nil {
			return err
		}
		if err := writeLocalJSON(filepath.Join(dir, "origin.json"), o); err != nil {
			return err
		}
		if err := syncLocalDirectory(dir); err != nil {
			return err
		}
		return syncLocalDirectory(filepath.Dir(dir))
	})
}

// Called under the existing checkout-wide apply lock, including ordinary job
// apply, so a pending workspace receipt cannot be mistaken for an orphan journal.
func recoverWorkspaceTransfers(ctx context.Context, root string) error {
	state, err := localChangeRoot()
	if err != nil {
		return err
	}
	parent := filepath.Join(state, "workspace-transfers")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := filepath.Join(parent, e.Name())
		o, err := readWorkspaceOrigin(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			continue
		} // An unknown origin must not block an unrelated checkout.
		if !sameLocalRoot(o.Root, root) {
			continue
		}
		unlock, err := lockWorkspaceTransfer(dir)
		if err != nil {
			return err
		}
		err = o.session(dir).Recover()
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func lockWorkspaceTransfer(dir string) (func(), error) {
	return acquireLocalChangeLock(workspaceTransferLockName(dir))
}

func workspaceTransferLockName(dir string) string {
	// Fetch holds a job download lock while recovering and applying workspace
	// transfers. A separate namespace prevents nested acquisitions from
	// colliding on the same stripe and waiting on a lock we already hold.
	return localChangeStripedLockName("workspace-transfer", filepath.Base(dir))
}

// Only used after a definitive creation rejection. Removing the origin first
// lets GC collect any debris if physical deletion is interrupted.
func discardWorkspaceOrigin(peer, id string) error {
	dir, err := workspaceTransferDir(peer, id)
	if err != nil {
		return err
	}
	unlock, err := lockWorkspaceTransfer(dir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(filepath.Join(dir, "origin.json")); err != nil {
		return err
	}
	if err := syncLocalDirectory(dir); err != nil {
		return err
	}
	if err := changeops.RemoveTree(dir); err != nil {
		return err
	}
	return syncLocalDirectory(filepath.Dir(dir))
}
