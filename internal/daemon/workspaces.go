package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

var errWorkspaceBusy = errors.New("workspace is in use by another job")
var errWorkspaceExists = errors.New("workspace already exists")

type workspaceRecord struct {
	proto.Workspace
	CacheLeaseID string              `json:"cache_lease_id,omitempty"`
	Owner        string              `json:"owner"`
	Identity     fsidentity.Identity `json:"identity"`
}

type workspaceStore struct {
	gateMu  sync.Mutex
	gates   map[string]*workspaceGate
	mu      sync.Mutex
	uploads map[string]*workspaceUpload // protected by mu; independent of command gates
	dir     string
	root    *os.Root
}

func openWorkspaces(dir string) (*workspaceStore, error) {
	if err := ensureChildDirectoryDurable(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("workspace store must be a real directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	s := &workspaceStore{dir: dir, root: root}
	// Unpublished uploads and removal tombstones never contain running workspaces.
	entries, err := os.ReadDir(dir)
	if err != nil {
		root.Close()
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".create-") || strings.HasPrefix(e.Name(), ".removed-") || strings.HasPrefix(e.Name(), ".push-upload-") {
			if err := removeOwnedTree(filepath.Join(dir, e.Name())); err != nil {
				root.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *workspaceStore) read(id string) (workspaceRecord, error) {
	var r workspaceRecord
	if !proto.ValidULID(id) {
		return r, os.ErrNotExist
	}
	info, err := s.root.Lstat(id)
	if err != nil {
		return r, err
	}
	if !info.IsDir() {
		return r, fmt.Errorf("invalid workspace directory")
	}
	f, err := s.root.Open(id + "/workspace.json")
	if err != nil {
		return r, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+maxSpecBytes+1))
	if err != nil {
		return r, err
	}
	if len(raw) > maxManifestBytes+maxSpecBytes {
		return r, fmt.Errorf("workspace metadata is too large")
	}
	if err := decodeStrictJSON(raw, &r); err != nil {
		return r, err
	}
	if r.ID != id || r.Owner == "" || r.Identity.IsZero() || proto.ValidateWorkspaceName(r.Name) != nil {
		return r, fmt.Errorf("invalid workspace identity")
	}
	seen := make(map[string]bool)
	for _, jobID := range r.JobIDs {
		if !proto.ValidULID(jobID) || seen[jobID] {
			return r, fmt.Errorf("invalid workspace job leases")
		}
		seen[jobID] = true
	}
	if len(r.JobIDs) > 0 && !proto.ValidULID(r.CacheLeaseID) || len(r.JobIDs) == 0 && r.CacheLeaseID != "" {
		return r, fmt.Errorf("invalid workspace cache lease")
	}
	if err := archive.Validate(r.Manifest); err != nil {
		return r, err
	}
	return r, nil
}

// Callers hold mu across reading a record and publishing its replacement.
func (s *workspaceStore) records() ([]workspaceRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var result []workspaceRecord
	var readErr error
	for _, e := range entries {
		if !proto.ValidULID(e.Name()) {
			continue
		}
		r, err := s.read(e.Name())
		if err != nil {
			readErr = errors.Join(readErr, fmt.Errorf("workspace %s: %w", e.Name(), err))
			continue
		}
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, readErr
}

func (s *workspaceStore) lookup(owner, name string) (workspaceRecord, error) {
	if proto.ValidULID(name) {
		r, err := s.read(name)
		if err == nil && r.Owner != owner {
			err = os.ErrNotExist
		}
		return r, err
	}
	rows, err := s.records()
	for _, r := range rows {
		if r.Owner == owner && r.Name == name {
			return r, nil
		}
	}
	if err != nil {
		return workspaceRecord{}, err
	}
	return workspaceRecord{}, os.ErrNotExist
}

func (s *workspaceStore) write(r workspaceRecord) error {
	return replaceJSONDurable(filepath.Join(s.dir, r.ID, "workspace.json"), r)
}

func (s *workspaceStore) publish(dir string, r workspaceRecord) error {
	// Finish the potentially large metadata write before serializing publication.
	if err := replaceJSONDurable(filepath.Join(dir, "workspace.json"), r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.lookup(r.Owner, r.Name); err == nil {
		return fmt.Errorf("%w: name %s", errWorkspaceExists, r.Name)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := s.root.Lstat(r.ID); !os.IsNotExist(err) {
		return fmt.Errorf("%w: id %s", errWorkspaceExists, r.ID)
	}
	if err := os.Rename(dir, filepath.Join(s.dir, r.ID)); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func (d *Daemon) workspaceOwner(id Identity) string {
	if d.cfg.InsecureNoAuth {
		return "insecure-test"
	}
	return id.Owner()
}

func workspaceDataIdentity(path string, want fsidentity.Identity) error {
	id, info, err := fsidentity.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || id != want {
		return fmt.Errorf("persistent workspace directory identity changed")
	}
	return nil
}

// Serialize binding changes and last-member settlement per workspace. The
// global inventory mutex never covers cache I/O or process cleanup.
type workspaceGate struct {
	token chan struct{}
	users int
}

func (s *workspaceStore) lockWorkspace(id string) func() {
	unlock, _ := s.lockWorkspaceContext(context.Background(), id)
	return unlock
}

func (s *workspaceStore) lockWorkspaceContext(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.gateMu.Lock()
	if s.gates == nil {
		s.gates = make(map[string]*workspaceGate)
	}
	gate := s.gates[id]
	if gate == nil {
		gate = &workspaceGate{token: make(chan struct{}, 1)}
		s.gates[id] = gate
	}
	gate.users++
	s.gateMu.Unlock()
	release := func() {
		s.gateMu.Lock()
		gate.users--
		if gate.users == 0 {
			delete(s.gates, id)
		}
		s.gateMu.Unlock()
	}
	select {
	case gate.token <- struct{}{}:
		return func() { <-gate.token; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}
func (r workspaceRecord) holds(jobID string) bool { return slices.Contains(r.JobIDs, jobID) }
