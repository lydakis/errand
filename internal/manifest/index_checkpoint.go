package manifest

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/lydakis/errand/internal/proto"
)

// IndexCheckpoint is experimental, versioned derived state. It carries no source
// authority. Records are postorder, and entry positions refer to sorted metadata.
// RestoreIndex validates the metadata, topology and hashes before retaining it.
type IndexCheckpoint struct {
	Version  int
	Fallback bool
	Nodes    []IndexRecord
}

type IndexRecord struct {
	Entry, Left, Right int
	Content, Digest    [32]byte
}

func (s *Snapshot) CheckpointIndex(ctx context.Context) (IndexCheckpoint, error) {
	if err := s.PrepareUpdates(ctx); err != nil {
		return IndexCheckpoint{}, err
	}
	image := IndexCheckpoint{Version: 1, Fallback: s.noIndex.Load()}
	root := s.index.Load()
	if root == nil {
		return image, ctx.Err()
	}
	image.Nodes = make([]IndexRecord, 0, s.count)
	var visit func(*node, int) int
	visit = func(n *node, offset int) int {
		if n == nil || ctx.Err() != nil {
			return -1
		}
		position := offset
		if n.left != nil {
			position += n.left.count
		}
		left := visit(n.left, offset)
		right := visit(n.right, position+1)
		image.Nodes = append(image.Nodes, IndexRecord{position, left, right, n.content, n.digest})
		return len(image.Nodes) - 1
	}
	visit(root, 0)
	return image, ctx.Err()
}

// RestoreIndex keeps the selected representation, including the height fallback.
// Checksums in an enclosing cache do not replace these semantic checks. Validation
// deliberately includes hashing; benchmarks must count that cost, not assume that
// deserializing derived hashes is sufficient to trust Merkle equality shortcuts.
func RestoreIndex(ctx context.Context, m proto.Manifest, image IndexCheckpoint) (*Snapshot, error) {
	if image.Version != 1 {
		return nil, fmt.Errorf("unsupported index checkpoint")
	}
	s, err := New(ctx, m)
	if err != nil {
		return nil, err
	}
	if len(image.Nodes) == 0 {
		if len(m.Entries) >= minIndexedEntries && !image.Fallback {
			return nil, fmt.Errorf("missing index checkpoint nodes")
		}
		s.noIndex.Store(image.Fallback)
		return s, ctx.Err()
	}
	if image.Fallback || len(image.Nodes) != s.count {
		return nil, fmt.Errorf("invalid index checkpoint size")
	}
	nodes := make([]node, s.count)
	used := make([]bool, s.count)
	low, high := make([]int, s.count), make([]int, s.count)
	for i, record := range image.Nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if record.Entry < 0 || record.Entry >= s.count {
			return nil, fmt.Errorf("invalid index entry")
		}
		n := &nodes[i]
		n.entry = s.entries[record.Entry]
		n.priority = sha256.Sum256([]byte(n.entry.Path))
		n.content = entryDigest(n.entry)
		low[i], high[i] = record.Entry, record.Entry
		for side, child := range []int{record.Left, record.Right} {
			if child == -1 {
				continue
			}
			if child < 0 || child >= i || used[child] {
				return nil, fmt.Errorf("invalid index topology")
			}
			used[child] = true
			if higher(&nodes[child], n) {
				return nil, fmt.Errorf("invalid index priority")
			}
			if side == 0 {
				if high[child] != record.Entry-1 {
					return nil, fmt.Errorf("invalid left index range")
				}
				n.left, low[i] = &nodes[child], low[child]
			} else {
				if low[child] != record.Entry+1 {
					return nil, fmt.Errorf("invalid right index range")
				}
				n.right, high[i] = &nodes[child], high[child]
			}
		}
		n.rehash()
		if n.height > maxHeight || n.content != record.Content || n.digest != record.Digest {
			return nil, fmt.Errorf("invalid index hash or height")
		}
	}
	last := s.count - 1
	if nodes[last].count != s.count || low[last] != 0 || high[last] != last {
		return nil, fmt.Errorf("disconnected index checkpoint")
	}
	s.index.Store(&nodes[last])
	return s, ctx.Err()
}
