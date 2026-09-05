// Package namedcache manages mutable runner-local caches and durable leases.
// It does not authorize callers or attach caches to job workspaces.
package namedcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/lydakis/errand/internal/proto"
)

var (
	ErrBusy          = errors.New("named cache is leased")
	ErrLeaseMismatch = errors.New("named cache lease does not belong to this job")
)

// Key separates authenticated owners, stable project identities, and names.
// Project must be a stable identity, not a display label or snapshot hash.
type Key struct {
	Owner   string `json:"owner"`
	Project string `json:"project"`
	Name    string `json:"name"`
}

func (k Key) validate() error {
	if k.Owner == "" || k.Project == "" || len(k.Owner) > 512 || len(k.Project) > 512 || !utf8.ValidString(k.Owner) || !utf8.ValidString(k.Project) || strings.ContainsAny(k.Owner+k.Project, "\x00\r\n") {
		return fmt.Errorf("named cache requires bounded owner and project identities")
	}
	if k.Name == "" || len(k.Name) > 64 || k.Name == "." || k.Name == ".." {
		return fmt.Errorf("invalid named cache name %q", k.Name)
	}
	for _, c := range k.Name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return fmt.Errorf("invalid named cache name %q", k.Name)
		}
	}
	return nil
}

func (k Key) hash() string {
	raw, _ := json.Marshal(k)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// Entry reports durable state. Bytes is measured at the last release, so an
// active cache's current size is unknown. LeaseID remains set across restart.
type Entry struct {
	Key      Key       `json:"key"`
	LeaseID  string    `json:"lease_id,omitempty"`
	LastUsed time.Time `json:"last_used"`
	Bytes    int64     `json:"bytes"`
}

type record struct {
	Version int `json:"version"`
	Entry
}

// Store exclusively owns a directory for its lifetime. Its size budget is an
// eviction target for GC, not a disk quota on running jobs.
type Store struct {
	root     *os.Root
	dir      string
	identity os.FileInfo
	lockFile *os.File
	mu       chan struct{}
	closed   bool
	maxBytes int64
	ttl      time.Duration
	now      func() time.Time
}

func Open(dir string, maxBytes int64, ttl time.Duration) (*Store, error) {
	if maxBytes < 0 || ttl <= 0 {
		return nil, fmt.Errorf("named cache budget must be nonnegative and TTL positive")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("named cache root must be a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("named cache root must be private (mode 0700)")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, fmt.Errorf("named cache root changed while opening")
	}
	file, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		root.Close()
		return nil, err
	}
	lockInfo, err := file.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() {
		file.Close()
		root.Close()
		return nil, fmt.Errorf("named cache lock must be a regular file")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		root.Close()
		return nil, fmt.Errorf("named cache store is already open: %w", err)
	}
	s := &Store{root: root, dir: dir, identity: info, lockFile: file, mu: make(chan struct{}, 1), maxBytes: maxBytes, ttl: ttl, now: time.Now}
	s.mu <- struct{}{}
	return s, nil
}

func (s *Store) lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.mu:
	}
	if err := ctx.Err(); err != nil {
		s.unlock()
		return err
	}
	if s.closed {
		s.unlock()
		return os.ErrClosed
	}
	if err := s.verifyRoot(); err != nil {
		s.unlock()
		return err
	}
	return nil
}

func (s *Store) unlock() { s.mu <- struct{}{} }

func (s *Store) verifyRoot() error {
	info, err := os.Lstat(s.dir)
	if err != nil || !info.IsDir() || !os.SameFile(s.identity, info) {
		return fmt.Errorf("named cache root changed")
	}
	return nil
}

// Close does not release job leases. Recovery must first settle the associated
// process scopes, then explicitly release their leases.
func (s *Store) Close() error {
	<-s.mu
	defer s.unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.lockFile.Close(), s.root.Close())
}

// Acquire persists the lease before exposing a writable path. A busy cache is
// refused immediately, including one leased by the same job ID.
func (s *Store) Acquire(ctx context.Context, key Key, jobID string) (string, error) {
	if err := key.validate(); err != nil {
		return "", err
	}
	if !proto.ValidULID(jobID) {
		return "", fmt.Errorf("named cache requires a job ID")
	}
	if err := s.lock(ctx); err != nil {
		return "", err
	}
	defer s.unlock()
	name := key.hash()
	r, err := s.read(name)
	if os.IsNotExist(err) {
		// Only a wholly absent entry is new. Missing metadata in an existing
		// directory must not make an unresolved lease reusable.
		if _, existsErr := s.root.Lstat(name); !os.IsNotExist(existsErr) {
			return "", fmt.Errorf("incomplete named cache %s: %w", name, err)
		}
		r = record{Version: 1, Entry: Entry{Key: key, LeaseID: jobID, LastUsed: s.now()}}
		if err := s.create(name, r); err != nil {
			return "", err
		}
	} else {
		if err != nil {
			return "", err
		}
		if r.LeaseID != "" {
			return "", ErrBusy
		}
		if err := s.validateData(name); err != nil {
			return "", err
		}
		r.LeaseID, r.LastUsed = jobID, s.now()
		if err := s.write(name, r); err != nil {
			return "", err
		}
	}
	return filepath.Join(s.dir, name, "data"), nil
}

// Release is allowed only after the job's entire process scope is stopped.
// Measurement failure leaves the lease protected. A persistence error can
// occur after rename; Inventory provides readback before a caller retries.
func (s *Store) Release(ctx context.Context, key Key, jobID string) error {
	if err := key.validate(); err != nil {
		return err
	}
	if !proto.ValidULID(jobID) {
		return ErrLeaseMismatch
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	name := key.hash()
	r, err := s.read(name)
	if err != nil {
		return err
	}
	if r.LeaseID != jobID {
		return ErrLeaseMismatch
	}
	if err := s.validateData(name); err != nil {
		return err
	}
	bytes, err := s.measure(ctx, name+"/data")
	if err != nil {
		return err
	}
	r.Bytes, r.LeaseID, r.LastUsed = bytes, "", s.now()
	return s.write(name, r)
}

// Discard retires an unusable cache after its lease holder's process cleanup.
// It is also the recovery path when the job removed its cache directory.
func (s *Store) Discard(ctx context.Context, key Key, jobID string) error {
	if err := key.validate(); err != nil {
		return err
	}
	if !proto.ValidULID(jobID) {
		return ErrLeaseMismatch
	}
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	name := key.hash()
	r, err := s.read(name)
	if err != nil {
		return err
	}
	if r.LeaseID != jobID {
		return ErrLeaseMismatch
	}
	return s.retire(name)
}

func (s *Store) read(name string) (record, error) {
	var r record
	info, err := s.root.Lstat(name)
	if err != nil {
		return r, err
	}
	if !info.IsDir() {
		return r, fmt.Errorf("named cache entry is not a directory: %s", name)
	}
	f, err := s.root.OpenFile(name+"/record.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return r, err
	}
	defer f.Close()
	metadata, err := f.Stat()
	if err != nil {
		return r, err
	}
	if !metadata.Mode().IsRegular() || metadata.Size() > 16<<10 {
		return r, fmt.Errorf("invalid named cache metadata file")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return r, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return r, fmt.Errorf("invalid named cache metadata")
	}
	if r.Version != 1 || r.Key.validate() != nil || r.Key.hash() != name || r.LastUsed.IsZero() || r.Bytes < 0 || r.LeaseID != "" && !proto.ValidULID(r.LeaseID) {
		return r, fmt.Errorf("invalid named cache metadata: %s", name)
	}
	return r, nil
}

func (s *Store) validateData(name string) error {
	info, err := s.root.Lstat(name + "/data")
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("named cache data is not a directory: %s", name)
	}
	return nil
}

func (s *Store) create(name string, r record) error {
	tmp := ".create-" + proto.NewULID()
	if err := s.root.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	defer s.root.RemoveAll(tmp)
	if err := s.root.Mkdir(tmp+"/data", 0o700); err != nil {
		return err
	}
	if err := s.write(tmp, r); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, name); err != nil {
		return err
	}
	return s.sync(".")
}

func (s *Store) write(name string, r record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := name + "/.record-" + proto.NewULID()
	f, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer s.root.Remove(tmp)
	_, writeErr := f.Write(raw)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, name+"/record.json"); err != nil {
		return err
	}
	return s.sync(name)
}

func (s *Store) sync(name string) error {
	dir, err := s.root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
