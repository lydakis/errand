package changes

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

type TransferGCResult struct {
	Failures   []string `json:"failures,omitempty"`
	Removed    int      `json:"removed"`
	Protected  int      `json:"protected"`
	FreedBytes int64    `json:"freed_bytes"`
}

// TransferStorageBytes counts private storage without following symlinks or
// changing modes. Read failures are reported rather than silently undercounted.
func TransferStorageBytes(dir string) (int64, error) {
	var bytes int64
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if os.IsNotExist(err) && path == dir {
			return nil
		}
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			info, err := e.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
		}
		return nil
	})
	return bytes, err
}

// GC never recovers or collects an in-flight application. Extra pins include
// the creation snapshot when older immutable job results must be reconstructed.
func (s TransferSession) GC(ctx context.Context, cutoff time.Time, dryRun bool, extra []proto.Manifest) (TransferGCResult, error) {
	var result TransferGCResult
	v, err := s.Checkpoint().Read()
	if err != nil {
		return result, err
	}
	pins := append([]proto.Manifest{v.Manifest}, extra...)
	parent := filepath.Join(s.Directory, "attempts")
	entries, err := os.ReadDir(parent)
	if err != nil {
		return result, err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		dir := filepath.Join(parent, e.Name())
		removing := strings.HasPrefix(e.Name(), ".removing-")
		if removing {
			// Eligibility was established before the durable rename. Never read
			// metadata from a partially removed attempt.
		} else if strings.HasPrefix(e.Name(), ".stage-") {
			info, err := e.Info()
			if err != nil {
				return result, err
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
		} else {
			if !proto.ValidULID(e.Name()) {
				return result, fmt.Errorf("invalid transfer staging entry")
			}
			a, err := s.Attempt(e.Name())
			if err != nil {
				return result, err
			}
			if a.Applying && (!a.Done || a.Revision+1 == v.Revision) {
				b, err := ReadTransferBundle(dir)
				if err != nil {
					return result, err
				}
				if !a.Done {
					pins = append(pins, b.RemoteManifest)
				}
				result.Protected++
				continue
			}
			if !a.CreatedAt.Before(cutoff) {
				continue
			}
		}
		size, err := TransferStorageBytes(dir)
		if err != nil {
			return result, err
		}
		if !dryRun {
			if !removing {
				garbage := filepath.Join(parent, ".removing-"+e.Name())
				if err := os.Rename(dir, garbage); err != nil {
					return result, err
				}
				dir = garbage
			}
			// Also retry this barrier for tombstones left by a failed fsync.
			if err := syncDirectory(parent); err != nil {
				return result, err
			}
			if err := RemoveTree(dir); err != nil {
				return result, err
			}
		}
		result.Removed++
		result.FreedBytes += size
	}
	pruned, err := s.Blobs().Prune(ctx, pins, dryRun)
	if err != nil {
		return result, err
	}
	result.FreedBytes += pruned.FreedBytes
	if !dryRun {
		err = syncDirectory(parent)
	}
	return result, err
}
