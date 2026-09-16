package snapshot

import (
	"context"
	"io/fs"
	"os"

	"github.com/lydakis/errand/internal/proto"
)

// Builder reuses hashes only when native identity, size, mode, mtime and ctime
// are unchanged. Build stats every selected path; Watch.Prepare can refresh a
// hinted subset while the packer independently verifies every shipped body.
// A Builder belongs to one serialized watch session and is not concurrent-safe.
type Builder struct {
	hashes map[string]fileHash
	next   map[string]fileHash
}
type fileHash struct {
	info           fs.FileInfo
	seconds, nanos int64
	hash           string
}

func (b *Builder) Build(root string, paths []string) (proto.Manifest, error) {
	b.next = make(map[string]fileHash, len(paths))
	m, err := buildBoundedContext(context.Background(), root, paths, -1, -1, b)
	if err == nil {
		b.hashes = b.next
	}
	b.next = nil
	return m, err
}

func (b *Builder) hash(ctx context.Context, path string, info fs.FileInfo) (string, error) {
	if b == nil {
		return hashFileSizedContext(ctx, path, info.Size(), info.Mode())
	}
	seconds, nanos, supported := changeStamp(info)
	old, ok := b.hashes[path]
	if supported && ok && os.SameFile(old.info, info) && old.info.Mode() == info.Mode() && old.info.Size() == info.Size() && old.info.ModTime().Equal(info.ModTime()) && old.seconds == seconds && old.nanos == nanos {
		b.next[path] = old
		return old.hash, nil
	}
	hash, err := hashFileSizedContext(ctx, path, info.Size(), info.Mode())
	if err != nil {
		return "", err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return "", sourceReadError(err)
	}
	s, ns, _ := changeStamp(after)
	if !os.SameFile(info, after) || info.Mode() != after.Mode() || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || supported && (s != seconds || ns != nanos) {
		return "", sourceChangedf("snapshot: %s changed while hashing", path)
	}
	if supported {
		b.next[path] = fileHash{info, seconds, nanos, hash}
	}
	return hash, nil
}
