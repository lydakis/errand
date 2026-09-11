package namedcache

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"syscall"

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
	s.operations.RLock()
	defer s.operations.RUnlock()
	result := GCResult{DryRun: dryRun}
	// Shared stores are measured only by GC. No recursive scan delays job
	// completion, and scanning does not prevent new jobs from acquiring them.
	entries, err := s.Inventory(ctx)
	if err != nil {
		return result, err
	}
	measured := make(map[string]Entry)
	damaged := make(map[string]Entry)
	var cleanupErrors error
	for _, entry := range entries {
		if !entry.Tree || entry.Protected() {
			continue
		}
		// Missing payloads are disposable, but only selected known generation
		// names are reclaimed. Holder and ownership metadata remain authoritative.
		reclaimed, cleanupErr := s.collectUnusedTrees(ctx, entry.Key, dryRun)
		result.ReclaimedTemps += reclaimed
		if cleanupErr != nil && ctx.Err() != nil {
			return result, ctx.Err()
		}
		cleanupErrors = errors.Join(cleanupErrors, cleanupErr)
		var size int64
		err := s.validateData(entry.Key.hash())
		if err == nil {
			size, err = s.measureConcurrent(ctx, entry.Key.hash()+"/data")
		}
		if err == nil {
			trees := entry.Key.hash() + "/trees"
			if _, statErr := s.root.Lstat(trees); !os.IsNotExist(statErr) {
				var snapshots int64
				if dryRun && reclaimed > 0 {
					snapshots, err = s.measureRetainedTrees(ctx, entry.Key.hash())
				} else {
					snapshots, err = s.measureConcurrent(ctx, trees)
				}
				if err == nil && snapshots > math.MaxInt64-size {
					err = fmt.Errorf("named cache byte count overflow")
				}
				if err == nil {
					size += snapshots
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOMEM) || errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EIO) {
				continue // Resource pressure is not evidence of damaged data.
			}
			// Cache trees are disposable. Retire unreadable/replaced roots
			// only if a locked recheck still proves this entry is idle.
			// One damaged store must not block collection of healthy entries.
			damaged[entry.Key.hash()] = entry
			continue
		}
		entry.Bytes, entry.BytesUnknown = size, false
		measured[entry.Key.hash()] = entry
	}
	if err := s.lock(ctx); err != nil {
		return result, err
	}
	var tombs []string
	err = func() error {
		defer s.unlock()
		entries, err = s.inventory(ctx)
		if err != nil {
			return err
		}
		var total int64
		var idle []Entry
		for _, entry := range entries {
			if sample, ok := measured[entry.Key.hash()]; ok && !entry.Protected() && sample.LastUsed.Equal(entry.LastUsed) {
				entry.Bytes, entry.BytesUnknown = sample.Bytes, false
				if !dryRun {
					r, err := s.read(entry.Key.hash())
					if err != nil {
						return err
					}
					r.Bytes, r.BytesUnknown = entry.Bytes, false
					if err := s.write(entry.Key.hash(), r); err != nil {
						return err
					}
				}
			}
			if entry.Bytes > math.MaxInt64-total {
				return fmt.Errorf("named cache byte count overflow")
			}
			total += entry.Bytes
			if entry.Protected() {
				result.Protected++
				continue
			}
			if sample, ok := damaged[entry.Key.hash()]; ok && sample.LastUsed.Equal(entry.LastUsed) {
				if !dryRun {
					tomb, err := s.detach(entry.Key.hash())
					if err != nil {
						return err
					}
					tombs = append(tombs, tomb)
				}
				total -= entry.Bytes
				result.Removed++
				result.FreedBytes += entry.Bytes
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
				return err
			}
			if !entry.LastUsed.Before(cutoff) && total <= s.maxBytes {
				break
			}
			if !dryRun {
				tomb, err := s.detach(entry.Key.hash())
				if err != nil {
					return err
				}
				tombs = append(tombs, tomb)
			}
			total -= entry.Bytes
			result.Removed++
			result.FreedBytes += entry.Bytes
		}
		temps, err := s.temporaryTrees(ctx)
		if err != nil {
			return err
		}
		result.ReclaimedTemps += len(temps)
		if !dryRun {
			for _, name := range temps {
				s.markRetiring(name)
				tombs = append(tombs, name)
			}
		}
		return nil
	}()
	// Deletion can take seconds for a large tree. Metadata operations on other
	// caches, including acquisitions and releases, remain available throughout.
	for _, tomb := range tombs {
		err = errors.Join(err, s.removeRetired(tomb))
	}
	return result, errors.Join(err, cleanupErrors)
}

func (s *Store) markRetiring(name string) {
	if s.retiring == nil {
		s.retiring = make(map[string]bool)
	}
	s.retiring[name] = true
}

func (s *Store) detach(name string) (string, error) {
	tomb := ".gc-" + name + "-" + proto.NewULID()
	if err := s.root.Rename(name, tomb); err != nil {
		return "", err
	}
	if err := s.sync("."); err != nil {
		return "", err
	}
	s.markRetiring(tomb)
	return tomb, nil
}

func (s *Store) removeRetired(name string) error {
	err := s.remove(name)
	<-s.mu
	delete(s.retiring, name)
	s.unlock()
	return err
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

func (s *Store) temporaryTrees(ctx context.Context) ([]string, error) {
	dir, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if s.retiring[name] {
			continue
		}
		creation := strings.HasPrefix(name, ".create-") && proto.ValidULID(strings.TrimPrefix(name, ".create-"))
		tomb := strings.TrimPrefix(name, ".gc-")
		deletion := strings.HasPrefix(name, ".gc-") && len(tomb) == 91 && cacheName(tomb[:64]) && tomb[64] == '-' && proto.ValidULID(tomb[65:])
		if creation || deletion {
			names = append(names, name)
		}
	}
	return names, nil
}

// measure reads only regular-file sizes and never follows cache symlinks.
// Failure leaves the lease intact, rather than making uncertain state idle.
func (s *Store) measure(ctx context.Context, name string) (int64, error) {
	return s.measureTree(ctx, name, false)
}

func (s *Store) measureConcurrent(ctx context.Context, name string) (int64, error) {
	return s.measureTree(ctx, name, true)
}

func (s *Store) measureTree(ctx context.Context, name string, concurrent bool) (int64, error) {
	var total int64
	var walk func(string) error
	walk = func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := s.root.Lstat(name)
		if concurrent && (os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)) {
			return nil
		}
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
		if concurrent && (os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)) {
			return nil
		}
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
			if concurrent && (os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)) {
				return nil
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
