package snapshotindex

import (
	"crypto/sha256"
	"hash/fnv"
	"sort"

	"github.com/lydakis/errand/internal/proto"
)

const partitionCount = 256

type partition struct {
	entries []proto.ManifestEntry
	hashes  [][32]byte
	digest  [32]byte
}
type partitioned struct {
	parts  [partitionCount]*partition
	digest [32]byte
}

func partitionFor(path string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(path))
	return h.Sum64() % partitionCount
}

// NewPartitioned uses fixed path-hash partitions. Only edited partitions are
// copied. The partition count is deliberately fixed for a reproducible trial.
func NewPartitioned(entries []proto.ManifestEntry) Index {
	p := &partitioned{}
	for _, entry := range entries {
		i := partitionFor(entry.Path)
		if p.parts[i] == nil {
			p.parts[i] = &partition{}
		}
		b := p.parts[i]
		b.entries = append(b.entries, entry)
		b.hashes = append(b.hashes, entryDigest(entry))
	}
	for _, b := range p.parts {
		if b != nil {
			b.rehash()
		}
	}
	p.rehash()
	return p
}

func (b *partition) rehash() {
	h := sha256.New()
	for _, digest := range b.hashes {
		_, _ = h.Write(digest[:])
	}
	copy(b.digest[:], h.Sum(nil))
}
func (p *partitioned) rehash() {
	h := sha256.New()
	var empty [32]byte
	for _, b := range p.parts {
		if b == nil {
			_, _ = h.Write(empty[:])
		} else {
			_, _ = h.Write(b.digest[:])
		}
	}
	copy(p.digest[:], h.Sum(nil))
}

func (p *partitioned) Update(edits []Edit) Index {
	n := *p
	var grouped [partitionCount][]Edit
	for _, edit := range sortedEdits(edits) {
		i := partitionFor(edit.Entry.Path)
		grouped[i] = append(grouped[i], edit)
	}
	for i, group := range grouped {
		if len(group) == 0 {
			continue
		}
		old := p.parts[i]
		if old == nil {
			old = &partition{}
		}
		entries := editEntries(old.entries, group)
		if len(entries) == 0 {
			n.parts[i] = nil
			continue
		}
		b := &partition{entries: entries, hashes: make([][32]byte, len(entries))}
		j := 0
		for k, entry := range entries {
			for j < len(old.entries) && old.entries[j].Path < entry.Path {
				j++
			}
			if j < len(old.entries) && old.entries[j] == entry {
				b.hashes[k] = old.hashes[j]
			} else {
				b.hashes[k] = entryDigest(entry)
			}
		}
		b.rehash()
		n.parts[i] = b
	}
	n.rehash()
	return &n
}

func (p *partitioned) Diff(next Index) []Edit {
	n := next.(*partitioned)
	if p.digest == n.digest {
		return nil
	}
	var result []Edit
	for i, before := range p.parts {
		after := n.parts[i]
		if before == after || before != nil && after != nil && before.digest == after.digest {
			continue
		}
		var a, b []proto.ManifestEntry
		if before != nil {
			a = before.entries
		}
		if after != nil {
			b = after.entries
		}
		result = append(result, diffEntries(a, b)...)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Entry.Path < result[j].Entry.Path })
	return result
}
func (p *partitioned) Manifest() proto.Manifest {
	count := 0
	for _, b := range p.parts {
		if b != nil {
			count += len(b.entries)
		}
	}
	var entries []proto.ManifestEntry
	if count > 0 {
		entries = make([]proto.ManifestEntry, 0, count)
	}
	for _, b := range p.parts {
		if b != nil {
			entries = append(entries, b.entries...)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return proto.Manifest{Entries: entries}
}
func (p *partitioned) Digest() [32]byte { return p.digest }
