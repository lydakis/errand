package snapshot

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	manifeststate "github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

// Directory listings let a watch accept file creations, removals and
// rename-over saves without enumerating the whole selection again. A stamped
// directory whose stamp changed is relisted: with policy and tracked-set
// evidence unchanged, only the names that appeared or disappeared there can
// change selection, and each is decided as full selection would decide it.

// listingEntry is one directory entry's name and type.
type listingEntry struct {
	name string
	typ  fs.FileMode
}

func listDirectory(name string) ([]listingEntry, error) {
	entries, err := os.ReadDir(name) // sorted by name
	if err != nil {
		return nil, err
	}
	listing := make([]listingEntry, len(entries))
	for i, entry := range entries {
		listing[i] = listingEntry{entry.Name(), entry.Type()}
	}
	return listing, nil
}

// membershipDelta is what relisting found: the relisted directories and the
// non-directory names created in or removed from them.
type membershipDelta struct {
	dirs    []string
	added   []string
	removed map[string]bool
}

func (d membershipDelta) changed() bool { return len(d.added) > 0 || len(d.removed) > 0 }

// changedDirectories returns stamped directories whose stamps changed. A
// stamped directory that can no longer be read is an error.
func (e *selectionEvidence) changedDirectories() ([]string, error) {
	var changed []string
	for name, old := range e.directories {
		info, err := os.Lstat(filepath.Join(e.root, filepath.FromSlash(name)))
		if err != nil {
			return nil, sourceReadError(err)
		}
		if !sameDirectoryEvidence(old, info) {
			changed = append(changed, name)
		}
	}
	return changed, nil
}

// relist re-proves directories whose stamps may have changed and returns
// evidence carrying their new stamps and listings. It returns nil when policy
// or tracked-set evidence changed, a directory cannot be re-proven, or a
// change could alter selection beyond the names themselves: a directory
// appeared, disappeared or changed type, or a selection control name did.
func (e *selectionEvidence) relist(dirs map[string]bool) (*selectionEvidence, membershipDelta) {
	var delta membershipDelta
	// A policy or index change needs full selection; skip the relisting.
	if e.verifyPolicy() != nil {
		return nil, delta
	}
	directories, listings := maps.Clone(e.directories), maps.Clone(e.listings)
	for name := range dirs {
		old, stamped := e.directories[name]
		before, listed := e.listings[name]
		if !stamped || !listed {
			return nil, delta
		}
		dir := filepath.Join(e.root, filepath.FromSlash(name))
		// Stamp before listing; the evidence check after the source work
		// rejects any membership change that follows.
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || !os.SameFile(old, info) || old.Mode() != info.Mode() {
			return nil, delta
		}
		if _, _, ok := changeStamp(info); !ok {
			return nil, delta
		}
		now, err := listDirectory(dir)
		if err != nil {
			return nil, delta
		}
		for i, j := 0, 0; i < len(before) || j < len(now); {
			var entry listingEntry
			removed := false
			switch {
			case j == len(now) || i < len(before) && before[i].name < now[j].name:
				entry, removed = before[i], true
				i++
			case i == len(before) || now[j].name < before[i].name:
				entry = now[j]
				j++
			default:
				if before[i].typ != now[j].typ {
					return nil, delta
				}
				i, j = i+1, j+1
				continue
			}
			if entry.typ.IsDir() || selectionControl(entry.name) {
				return nil, delta
			}
			rel := path.Join(name, entry.name)
			if removed {
				if delta.removed == nil {
					delta.removed = map[string]bool{}
				}
				delta.removed[rel] = true
			} else {
				delta.added = append(delta.added, rel)
			}
		}
		directories[name], listings[name] = info, now
		delta.dirs = append(delta.dirs, name)
	}
	next := *e
	next.directories, next.listings = directories, listings
	return &next, delta
}

// selectionControl reports names whose presence changes how selection treats
// other paths: ignore files, the explicit policy and Git metadata.
func selectionControl(name string) bool {
	for _, control := range []string{".errandignore", ".gitignore", ".git"} {
		if strings.EqualFold(name, control) {
			return true
		}
	}
	return false
}

// resolve turns relisted membership into manifest work. It adds created
// files full selection would include, and selected files in relisted
// directories whose observations changed, to refresh, and returns the prior
// entries that were removed. It fails when the result could differ from
// full selection.
func (d membershipDelta) resolve(e *selectionEvidence, prior *manifeststate.Snapshot, refresh map[string]bool, observed func(rel string) bool) ([]string, error) {
	var deleted []string
	for rel := range d.removed {
		if _, ok := prior.Lookup(rel); ok {
			deleted = append(deleted, rel)
		}
	}
	// Check the relisted directories' other selected files: a replacement
	// whose event was lost still changes its directory's stamp.
	for _, dir := range d.dirs {
		for _, entry := range e.listings[dir] {
			rel := path.Join(dir, entry.name)
			if found, ok := prior.Lookup(rel); ok && found.Type != proto.EntryDir && !refresh[rel] && !observed(rel) {
				refresh[rel] = true
			}
		}
	}
	added, err := e.selectAdded(d.added)
	if err != nil {
		return nil, err
	}
	for _, rel := range added {
		refresh[rel] = true
	}
	if e.git != nil && !keepsDirectories(prior, e.listings, deleted, added) {
		return nil, errors.New("snapshot: a removal emptied a Git-selected directory")
	}
	return deleted, nil
}

// selectAdded returns the created non-directory names full selection would
// include. Their directories are already proven selected (explicit) or
// stamped as unexcluded (Git).
func (e *selectionEvidence) selectAdded(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if e.git != nil {
		return gitSelectAdded(e.root, names, e.opts.Caches)
	}
	var selected []string
	for _, rel := range names {
		// The same exclusions walk applies to a non-directory entry.
		if pathpolicy.InCache(rel, e.opts.Caches) || isLocalChangeTransactionPath(rel) ||
			pathContainsGitMetadata(rel) || e.matcher.Ignored(rel, false) {
			continue
		}
		selected = append(selected, rel)
	}
	return selected, nil
}

// maxGitPathspecs bounds one Git query; more creations use full selection.
const maxGitPathspecs = 256

// gitSelectAdded asks Git which created names are tracked or untracked and
// unignored, then applies gitListFiles' and the caller's other exclusions.
func gitSelectAdded(root string, names []string, caches []proto.CacheBinding) ([]string, error) {
	if len(names) > maxGitPathspecs {
		return nil, errors.New("snapshot: too many created files for narrowed Git selection")
	}
	args := append([]string{"--literal-pathspecs", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--"}, names...)
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, err
	}
	var selected []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" || pathContainsGitMetadata(p) || isLocalChangeTransactionPath(p) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
			selected = append(selected, p)
		}
	}
	return withoutCachePaths(root, selected, caches)
}

// keepsDirectories reports whether removals leave each affected directory
// with a selected child. Git selection derives directories from the files
// below them, so a directory emptied of selected files would leave the
// manifest; explicit selection lists directories themselves.
func keepsDirectories(prior *manifeststate.Snapshot, listings map[string][]listingEntry, deleted, added []string) bool {
	gone := make(map[string]bool, len(deleted))
	for _, rel := range deleted {
		gone[rel] = true
	}
	kept := map[string]bool{".": true}
	for _, rel := range added {
		kept[path.Dir(rel)] = true
	}
	for _, rel := range deleted {
		dir := path.Dir(rel)
		for _, entry := range listings[dir] {
			if kept[dir] {
				break
			}
			child := path.Join(dir, entry.name)
			if _, ok := prior.Lookup(child); ok && !gone[child] {
				kept[dir] = true
			}
		}
		if !kept[dir] {
			return false
		}
	}
	return true
}
