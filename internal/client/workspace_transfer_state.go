package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

type workspaceOrigin struct {
	Root        string              `json:"root"`
	RootID      fsidentity.Identity `json:"root_identity"`
	WorkspaceID string              `json:"workspace_id"`
	PeerURL     string              `json:"peer_url"`
	Initial     proto.Manifest      `json:"initial"`
}

func workspaceTransferDir(peer, id string) (string, error) {
	root, err := localChangeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "workspace-transfers", localChangeKey(peer, id)), nil
}
func readWorkspaceOrigin(dir string) (workspaceOrigin, error) {
	var o workspaceOrigin
	f, err := os.Open(filepath.Join(dir, "origin.json"))
	if err != nil {
		return o, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return o, err
	}
	if info.Size() > changeops.MaxBundleMetadataBytes {
		return o, fmt.Errorf("workspace origin exceeds metadata limit")
	}
	err = json.NewDecoder(f).Decode(&o)
	if err == nil && (!proto.ValidULID(o.WorkspaceID) || o.RootID.IsZero() || !filepath.IsAbs(o.Root) || o.PeerURL == "") {
		err = fmt.Errorf("invalid workspace origin")
	}
	return o, err
}
func (o workspaceOrigin) session(dir string) changeops.TransferSession {
	return changeops.TransferSession{Directory: filepath.Join(dir, "fetch"), Root: o.Root, RootID: o.RootID, Owner: localChangeKey(o.PeerURL, o.WorkspaceID), SourceID: o.WorkspaceID, MaxBytes: proto.DefaultLimits().MaxChangeBytes}
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
		o := workspaceOrigin{Root: opts.Root, RootID: rootID, WorkspaceID: id, PeerURL: opts.PeerURL, Initial: m}
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
	return acquireLocalChangeLock(localChangeTransferLockName("workspace-" + filepath.Base(dir)))
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
