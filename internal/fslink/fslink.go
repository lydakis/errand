// Package fslink preserves Windows' file/directory symlink distinction when a
// link is recreated in private staging before its referent is present.
package fslink

import (
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

type Lookup func(string) (proto.ManifestEntry, bool)

// ManifestLookup borrows sorted metadata without allocating an index. Archive
// callers may supply unsorted manifests; only those need a sorted copy.
func ManifestLookup(m proto.Manifest) Lookup {
	entries := m.Entries
	compare := func(a, b proto.ManifestEntry) int { return strings.Compare(a.Path, b.Path) }
	if !slices.IsSortedFunc(entries, compare) {
		entries = slices.Clone(entries)
		slices.SortFunc(entries, compare)
	}
	return func(name string) (proto.ManifestEntry, bool) {
		i := sort.Search(len(entries), func(i int) bool { return entries[i].Path >= name })
		if i < len(entries) && entries[i].Path == name {
			return entries[i], true
		}
		// A manifest may omit explicit directory entries.
		prefix := name + "/"
		i = sort.Search(len(entries), func(i int) bool { return entries[i].Path >= prefix })
		if i < len(entries) && strings.HasPrefix(entries[i].Path, prefix) {
			return proto.ManifestEntry{Path: name, Type: proto.EntryDir}, true
		}
		return proto.ManifestEntry{}, false
	}
}

// IsDirectory follows manifest links, including links in target ancestors.
// Missing referents and cycles use the file-link default. No filesystem reads
// or traversal through untrusted links are needed.
func IsDirectory(e proto.ManifestEntry, lookup Lookup) bool {
	name := path.Clean(path.Join(path.Dir(e.Path), e.Target))
	for hops := 0; hops < 40; hops++ {
		if name == "." {
			return true
		}
		if name == ".." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
			return false
		}
		if entry, ok := lookup(name); ok {
			switch entry.Type {
			case proto.EntryDir:
				return true
			case proto.EntryFile:
				return false
			case proto.EntrySymlink:
				name = path.Clean(path.Join(path.Dir(name), entry.Target))
				continue
			}
		}
		resolved := false
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if entry, ok := lookup(parent); ok && entry.Type == proto.EntrySymlink {
				name = path.Clean(path.Join(path.Dir(parent), entry.Target, strings.TrimPrefix(name, parent+"/")))
				resolved = true
				break
			}
		}
		if !resolved {
			return false
		}
	}
	return false
}
