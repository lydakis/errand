package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

// Watch reports invalidations, not an authoritative list of files to upload.
// Selection and content verification still run before every transfer. A single
// buffered notification coalesces arbitrarily many edits while a push is busy.
type Watch struct {
	Changed      chan struct{}
	Errors       chan error
	w            *fsnotify.Watcher
	done         chan struct{}
	root         string
	identity     fs.FileInfo
	matcher      *pathpolicy.Matcher
	opts         SelectOptions
	selected     map[string]bool
	controls     map[string]bool
	directories  map[string]bool
	observedInfo map[string]fs.FileInfo
	generation   atomic.Uint64
	dirtyMu      sync.Mutex
	dirty        map[string]bool
	fullScan     bool
	resetHashes  bool
	owedFull     bool              // an expired preparation deferred its full reconciliation
	prepared     *watchPreparation // used only by serialized Prepare calls
}

func WatchFiles(root string, opts SelectOptions) (*Watch, error) {
	return watchFiles(root, opts, SelectFilesWithOptions)
}

func watchFiles(root string, opts SelectOptions, selectFiles func(string, SelectOptions) ([]string, GitInfo, proto.SelectionPolicy, error)) (*Watch, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	paths, gi, policy, err := selectFiles(root, opts)
	if err != nil {
		return nil, err
	}
	m, err := pathpolicy.Compile(policy)
	if err != nil {
		return nil, err
	}
	w, err := fsnotify.NewBufferedWatcher(4096)
	if err != nil {
		return nil, err
	}
	s := &Watch{Changed: make(chan struct{}, 1), Errors: make(chan error, 1), w: w,
		done: make(chan struct{}), root: root, matcher: m, opts: opts,
		selected: make(map[string]bool), controls: make(map[string]bool)}
	s.identity, err = os.Lstat(root)
	if err != nil {
		w.Close()
		return nil, err
	}
	for _, p := range paths {
		for p != "." {
			s.selected[p] = true
			p = filepath.ToSlash(filepath.Dir(p))
		}
	}
	// Watch Git's small control directories, including those outside a linked
	// worktree. Object writes and lock files do not invalidate source selection.
	_, explicitPolicy := os.Lstat(filepath.Join(root, ".errandignore"))
	if gi.Repository && os.IsNotExist(explicitPolicy) {
		if home, e := os.UserHomeDir(); e == nil {
			s.controls[filepath.Join(home, ".gitconfig")] = true
			xdg := os.Getenv("XDG_CONFIG_HOME")
			if xdg == "" {
				xdg = filepath.Join(home, ".config")
			}
			s.controls[filepath.Join(xdg, "git", "config")] = true
		}
		for _, name := range []string{"index", "HEAD", "config", "info/exclude"} {
			p, e := gitPath(root, name)
			if e != nil {
				w.Close()
				return nil, e
			}
			s.controls[filepath.Clean(p)] = true
		}
		worktree, _, e := gitWorktreeContext(root)
		if e != nil {
			w.Close()
			return nil, e
		}
		for dir := root; ; dir = filepath.Dir(dir) {
			s.controls[filepath.Join(dir, ".gitignore")] = true
			if dir == worktree || dir == filepath.Dir(dir) {
				break
			}
		}
		global, ok, e := gitConfigPath(root, "core.excludesFile")
		if e != nil {
			w.Close()
			return nil, e
		}
		if !ok {
			global, e = defaultGitExcludesPath()
		}
		if e != nil {
			w.Close()
			return nil, e
		}
		if global != "" {
			if !filepath.IsAbs(global) {
				global = filepath.Join(worktree, global)
			}
			s.controls[global] = true
		}
	}
	s.observedInfo = make(map[string]fs.FileInfo)
	for p := range s.controls {
		s.observedInfo[p], _ = os.Lstat(p)
	}
	if err := s.refresh(); err != nil {
		w.Close()
		return nil, err
	}
	// Selection predates registration. Reconcile once after controls are
	// watched; later changes are now queued even while directories are added.
	if err := s.reselect(); err != nil {
		w.Close()
		return nil, err
	}
	if err := s.refresh(); err != nil {
		w.Close()
		return nil, err
	}
	go s.run()
	return s, nil
}

func (s *Watch) Generation() uint64 { return s.generation.Load() }

func (s *Watch) Close() error {
	err := s.w.Close()
	<-s.done
	return err
}

// InvalidatePreparation requests full source reconciliation after freezing or
// packing finds a change before its native event arrives. It preserves queued
// dirty hints and notifications without scheduling a cycle itself, so callers
// retain control of retry backoff. It may run concurrently with event delivery.
func (s *Watch) InvalidatePreparation() {
	s.dirtyMu.Lock()
	s.fullScan = true
	s.resetHashes = true
	s.dirtyMu.Unlock()
}

func (s *Watch) invalidate() {
	s.InvalidatePreparation()
	s.notifyChange()
}

func (s *Watch) invalidatePath(name string, structural bool) {
	s.dirtyMu.Lock()
	if structural {
		s.fullScan = true
	}
	if s.dirty == nil {
		s.dirty = make(map[string]bool)
	}
	s.dirty[name] = s.dirty[name] || structural
	s.dirtyMu.Unlock()
	s.notifyChange()
}

func (s *Watch) notifyChange() {
	s.generation.Add(1)
	select {
	case s.Changed <- struct{}{}:
	default:
	}
}

func (s *Watch) relevant(p string, directory bool) bool {
	if s.controls[p] {
		return true
	}
	rel, err := filepath.Rel(s.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return true
	}
	if pathpolicy.InCache(rel, s.opts.Caches) || isLocalChangeTransactionPath(rel) {
		return false
	}
	if pathContainsGitMetadata(rel) {
		return false
	}
	// Ignore files can affect selection even when they are themselves excluded.
	if filepath.Base(rel) == ".errandignore" || filepath.Base(rel) == ".gitignore" {
		return true
	}
	return s.selected[rel] || !s.matcher.Ignored(rel, directory)
}

func (s *Watch) refresh() error {
	info, err := os.Lstat(s.root)
	if err != nil || !os.SameFile(s.identity, info) {
		return fmt.Errorf("watched checkout was removed or replaced")
	}
	watching := make(map[string]bool)
	wanted := make(map[string]bool)
	observed := make(map[string]fs.FileInfo)
	for p := range s.controls {
		observed[p], _ = os.Lstat(p)
	}
	for _, p := range s.w.WatchList() {
		watching[p] = true
	}
	add := func(p string) error {
		wanted[p] = true
		if watching[p] {
			return nil
		}
		if err := s.w.Add(p); err != nil {
			return fmt.Errorf("watching %s: %w", p, err)
		}
		watching[p] = true
		return nil
	}
	for p := range s.controls {
		// Watch the closest existing ancestor so creating a missing info/ or
		// global ignore directory is observed as well.
		for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
			if fi, e := os.Stat(dir); e == nil && fi.IsDir() {
				if err := add(dir); err != nil {
					return err
				}
				break
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	err = filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		} // an editor may remove its temporary file
		if err != nil {
			return err
		}
		if s.relevant(p, d.IsDir()) {
			observed[p], _ = d.Info()
		}
		if !d.IsDir() {
			return nil
		}
		if !s.relevant(p, true) {
			return filepath.SkipDir
		}
		return add(p)
	})
	if err != nil {
		return err
	}
	for p := range watching {
		if !wanted[p] {
			if err := s.w.Remove(p); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
				return err
			}
		}
	}
	s.directories, s.observedInfo = wanted, observed
	return nil
}

func (s *Watch) reselect() error {
	paths, _, policy, err := SelectFilesWithOptions(s.root, s.opts)
	if err != nil {
		return err
	}
	matcher, err := pathpolicy.Compile(policy)
	if err != nil {
		return err
	}
	s.matcher = matcher
	s.selected = make(map[string]bool)
	for _, p := range paths {
		for p != "." {
			s.selected[p] = true
			p = filepath.ToSlash(filepath.Dir(p))
		}
	}
	return nil
}

func (s *Watch) run() {
	defer close(s.done)
	fail := func(err error) { s.Errors <- err }
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var pendingRefresh bool
	var selectionChanged bool

	for {
		select {
		case <-timer.C:
			pendingRefresh = false
			if selectionChanged {
				if err := s.reselect(); err != nil {
					fail(err)
					return
				}
				selectionChanged = false
			}
			if err := s.refresh(); err != nil {
				fail(err)
				return
			}
			s.invalidate() // also covers files created before their directory watch
		case err, ok := <-s.w.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				if err := s.reselect(); err != nil {
					fail(err)
					return
				}
				if err := s.refresh(); err != nil {
					fail(err)
					return
				}
				s.invalidate() // never infer no changes from a lossy event stream
				continue
			}
			fail(err)
			return
		case event, ok := <-s.w.Events:
			if !ok {
				return
			}
			info, err := os.Lstat(event.Name)
			dir := err == nil && info.IsDir()
			if s.relevant(event.Name, dir) {
				old := s.observedInfo[event.Name]
				if info == nil {
					delete(s.observedInfo, event.Name)
				} else {
					s.observedInfo[event.Name] = info
				}
				// Reads can emit attribute notifications on macOS (notably Git's index).
				// Access-time-only events do not change selection. Writes, inode
				// replacement and real mode changes still force reconciliation.
				if event.Op == fsnotify.Chmod && old != nil && info != nil && os.SameFile(old, info) && old.Mode() == info.Mode() && old.Size() == info.Size() && old.ModTime().Equal(info.ModTime()) {
					continue
				}
			}
			control := s.controls[event.Name] || filepath.Base(event.Name) == ".gitignore" || filepath.Base(event.Name) == ".errandignore"
			if !s.relevant(event.Name, dir) {
				// A missing policy directory may have just been created.
				ancestor := false
				for p := range s.controls {
					if strings.HasPrefix(p, event.Name+string(filepath.Separator)) {
						ancestor = true
						break
					}
				}
				if !ancestor {
					continue
				}
			}
			if control || event.Has(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) && (dir || s.directories[event.Name]) {
				selectionChanged = selectionChanged || control
				if !pendingRefresh {
					timer.Reset(20 * time.Millisecond)
					pendingRefresh = true
				}
			}
			s.invalidatePath(event.Name, control || dir || event.Has(fsnotify.Create|fsnotify.Remove|fsnotify.Rename))
		}
	}
}
