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
	stamp ObservationStamp
	hash  string
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
	stamp, stampErr := Fingerprint(info)
	supported := stampErr == nil
	old, ok := b.hashes[path]
	if supported && ok && sameObservation(old.stamp, stamp) {
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
	afterStamp, afterErr := Fingerprint(after)
	var unchanged bool
	if supported {
		unchanged = afterErr == nil && sameObservation(stamp, afterStamp)
	} else {
		unchanged = os.SameFile(info, after) && info.Mode() == after.Mode() && info.Size() == after.Size() && info.ModTime().Equal(after.ModTime())
	}
	if !unchanged {
		return "", sourceChangedf("snapshot: %s changed while hashing", path)
	}
	if supported {
		b.next[path] = fileHash{stamp, hash}
	}
	return hash, nil
}
