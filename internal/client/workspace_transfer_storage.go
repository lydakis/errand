package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

func workspaceTransferStats(ctx context.Context) (proto.StorageCategory, error) {
	var stats proto.StorageCategory
	err := visitWorkspaceTransfers(ctx, false, func(dir string, o workspaceOrigin) error {
		bytes, err := changeops.TreeSizeContext(ctx, dir)
		if err != nil {
			return err
		}
		stats.Items++
		stats.Bytes += bytes
		return nil
	})
	return stats, err
}
func workspaceTransferGC(cutoff time.Time, dryRun bool) (ChangeGCResult, error) {
	var result ChangeGCResult
	err := visitWorkspaceTransfers(context.Background(), dryRun, func(dir string, o workspaceOrigin) error {
		if o.Root == "" {
			info, err := os.Stat(dir)
			if err != nil {
				return err
			}
			if !info.ModTime().Before(cutoff) {
				return nil
			}
			size, err := changeops.TransferStorageBytes(dir)
			if err != nil {
				return err
			}
			if !dryRun {
				if err := changeops.RemoveTree(dir); err != nil {
					return err
				}
			}
			result.Removed++
			result.Selected++
			result.FreedBytes += size
			return nil
		}
		gc, err := o.session(dir).GC(context.Background(), cutoff, dryRun, []proto.Manifest{o.Initial})
		result.Removed += gc.Removed
		result.Selected += gc.Removed + gc.Protected
		result.Protected += gc.Protected
		result.FreedBytes += gc.FreedBytes
		if err != nil {
			result.Failed++
			return fmt.Errorf("collecting workspace %s transfers: %w", o.WorkspaceID, err)
		}
		children, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range children {
			if !strings.HasPrefix(e.Name(), ".source-") && !strings.HasPrefix(e.Name(), ".initial-") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			size, err := changeops.TransferStorageBytes(path)
			if err != nil {
				return err
			}
			if !dryRun {
				if err := changeops.RemoveTree(path); err != nil {
					return err
				}
			}
			result.Removed++
			result.Selected++
			result.FreedBytes += size
		}
		// Frozen uploads referenced by an uncertain request stay protected.
		var pending pendingPush
		raw, readErr := os.ReadFile(filepath.Join(dir, "push.json"))
		if readErr == nil {
			if err := json.Unmarshal(raw, &pending); err != nil {
				return err
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		entries, err := os.ReadDir(filepath.Join(dir, "push-sources"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name() == pending.Request.ID {
				result.Protected++
				continue
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
			path := filepath.Join(dir, "push-sources", e.Name())
			size, err := changeops.TransferStorageBytes(path)
			if err != nil {
				return err
			}
			if !dryRun {
				if err := changeops.RemoveTree(path); err != nil {
					return err
				}
			}
			result.Removed++
			result.Selected++
			result.FreedBytes += size
		}
		return nil
	})
	return result, err
}
func visitWorkspaceTransfers(ctx context.Context, dryRun bool, visit func(string, workspaceOrigin) error) error {
	root, err := localChangeRoot()
	if err != nil {
		return err
	}
	parent := filepath.Join(root, "workspace-transfers")
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
		var unlock func()
		if dryRun {
			var acquired bool
			unlock, acquired, err = tryAcquireExistingLocalChangeLock(localChangeTransferLockName("workspace-" + e.Name()))
			if err != nil {
				return err
			}
			if !acquired {
				continue
			}
		} else {
			unlock, err = lockWorkspaceTransfer(dir)
			if err != nil {
				return err
			}
		}
		o, err := readWorkspaceOrigin(dir)
		if err == nil || os.IsNotExist(err) {
			err = visit(dir, o)
		}
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func RemoteTransferGC(peer string, olderThan time.Duration, dryRun bool) (changeops.TransferGCResult, error) {
	var result changeops.TransferGCResult
	if olderThan < time.Second {
		return result, fmt.Errorf("change retention must be at least 1s")
	}
	seconds := int64(olderThan / time.Second)
	if olderThan%time.Second != 0 {
		seconds++
	}
	err := postJSONResultContext(context.Background(), strings.TrimSuffix(peer, "/")+"/v0/gc/changes", proto.TransferGCRequest{OlderThanSeconds: seconds, DryRun: dryRun}, "workspace transfer GC", &result)
	return result, err
}
