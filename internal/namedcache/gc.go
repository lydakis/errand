package namedcache

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

type GCResult struct {
	DryRun         bool
	Removed        int
	FreedBytes     int64
	Protected      int
	ReclaimedTemps int // interrupted creations/evictions, separate from Removed and FreedBytes
}

// Inventory returns metadata for all owners. The daemon must filter this by
// authenticated owner before exposing it to a client.
func (s *Store) Inventory(ctx context.Context) ([]Entry, error) {
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	return s.inventory(ctx)
}

func (s *Store) inventory(ctx context.Context) ([]Entry, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var result []Entry
	for {
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := entry.Name()
			if !cacheName(name) {
				continue
			}
			r, err := s.read(name)
			if err != nil {
				return nil, fmt.Errorf("reading named cache %s: %w", name, err)
			}
			result = append(result, r.Entry)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key.hash() < result[j].Key.hash() })
	return result, nil
}

func cacheName(name string) bool {
	if len(name) != 64 || strings.ToLower(name) != name {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// GC evicts idle caches by TTL, then oldest use until below the byte target.
// Leases are always protected. Dry runs do not mutate files or metadata.
func (s *Store) GC(ctx context.Context, dryRun bool) (GCResult, error) {
	result := GCResult{DryRun: dryRun}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	defer s.unlock()
	entries, err := s.inventory(ctx)
	if err != nil {
		return result, err
	}
	var total int64
	var idle []Entry
	for _, entry := range entries {
		if entry.Bytes > math.MaxInt64-total {
			return result, fmt.Errorf("named cache byte count overflow")
		}
		total += entry.Bytes
		if entry.LeaseID != "" {
			result.Protected++
			continue
		}
		idle = append(idle, entry)
	}
	sort.Slice(idle, func(i, j int) bool {
		if idle[i].LastUsed.Equal(idle[j].LastUsed) {
			return idle[i].Key.hash() < idle[j].Key.hash()
		}
		return idle[i].LastUsed.Before(idle[j].LastUsed)
	})
	cutoff := s.now().Add(-s.ttl)
	for _, entry := range idle {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !entry.LastUsed.Before(cutoff) && total <= s.maxBytes {
			break
		}
		if !dryRun {
			name := entry.Key.hash()
			if err := s.retire(name); err != nil {
				return result, err
			}
		}
		total -= entry.Bytes
		result.Removed++
		result.FreedBytes += entry.Bytes
	}
	result.ReclaimedTemps, err = s.cleanupTemps(ctx, dryRun)
	return result, err
}

func (s *Store) retire(name string) error {
	tomb := ".gc-" + name + "-" + proto.NewULID()
	if err := s.root.Rename(name, tomb); err != nil {
		return err
	}
	if err := s.sync("."); err != nil {
		return err
	}
	return s.remove(tomb)
}

func (s *Store) remove(name string) error {
	if err := s.verifyRoot(); err != nil {
		return err
	}
	if err := s.makeRemovable(name); err != nil {
		return err
	}
	if err := s.root.RemoveAll(name); err != nil {
		return err
	}
	return s.sync(".")
}

// makeRemovable grants owner access to every directory in a retired cache.
// Cache contents have no reserved workspace paths: Git metadata must be
// removable too. Files and symlinks need no permission changes for unlinking.
func (s *Store) makeRemovable(name string) error {
	info, err := s.root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if err := s.root.Chmod(name, info.Mode().Perm()|0o700); err != nil {
		return err
	}
	dir, err := s.root.Open(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(256)
		for _, entry := range entries {
			if err := s.makeRemovable(name + "/" + entry.Name()); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Store) cleanupTemps(ctx context.Context, dryRun bool) (int, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return reclaimed, err
		}
		name := entry.Name()
		creation := strings.HasPrefix(name, ".create-") && proto.ValidULID(strings.TrimPrefix(name, ".create-"))
		tomb := strings.TrimPrefix(name, ".gc-")
		deletion := strings.HasPrefix(name, ".gc-") && len(tomb) == 91 && cacheName(tomb[:64]) && tomb[64] == '-' && proto.ValidULID(tomb[65:])
		if creation || deletion {
			if !dryRun {
				if err := s.remove(name); err != nil {
					return reclaimed, err
				}
			}
			reclaimed++
		}
	}
	return reclaimed, nil
}

// measure reads only regular-file sizes and never follows cache symlinks.
// Failure leaves the lease intact, rather than making uncertain state idle.
func (s *Store) measure(ctx context.Context, name string) (int64, error) {
	var total int64
	var walk func(string) error
	walk = func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := s.root.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if info.Size() > math.MaxInt64-total {
				return fmt.Errorf("named cache size overflow")
			}
			total += info.Size()
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		dir, err := s.root.Open(name)
		if err != nil {
			return err
		}
		defer dir.Close()
		for {
			entries, err := dir.ReadDir(256)
			for _, entry := range entries {
				if err := walk(name + "/" + entry.Name()); err != nil {
					return err
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	err := walk(name)
	return total, err
}
