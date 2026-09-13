package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/lydakis/errand/internal/proto"
)

// Nodes are immutable after publication. Tree height is bounded before recursive
// operations, including for attacker-chosen path priorities.
const maxHeight = 128

var errTreeHeight = errors.New("manifest index exceeds height limit")

type node struct {
	entry       proto.ManifestEntry
	priority    [32]byte
	content     [32]byte
	digest      [32]byte
	left, right *node
	count       int
	height      int
}

func buildTree(ctx context.Context, entries []proto.ManifestEntry) (*node, error) {
	// Sorted entries form a Cartesian tree in O(N), then one hash pass.
	var localStack [64]*node
	stack := localStack[:0]
	var block []node
	for i, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Small blocks amortize allocation without keeping an entire cold tree
		// alive when a retained snapshot shares only a few of its entries.
		if i%256 == 0 {
			block = make([]node, min(256, len(entries)-i))
		}
		n := &block[i%256]
		*n = node{entry: entry, priority: sha256.Sum256([]byte(entry.Path))}
		for len(stack) > 0 && higher(n, stack[len(stack)-1]) {
			n.left = stack[len(stack)-1]
			n.left.recount()
			if n.left.height > maxHeight {
				return nil, errTreeHeight
			}
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			stack[len(stack)-1].right = n
		}
		stack = append(stack, n)
	}
	if len(stack) == 0 {
		return nil, ctx.Err()
	}
	root := stack[0]
	for i := len(stack) - 1; i >= 0; i-- {
		stack[i].recount()
		if stack[i].height > maxHeight {
			return nil, errTreeHeight
		}
	}
	finishTree(ctx, root)
	return root, ctx.Err()
}

// Hash independent subtrees before their ancestors. Only the new, unpublished
// nodes are mutable here. A small bounded pool respects GOMAXPROCS; small trees
// avoid goroutine setup. Updates never use this pool or rehash shared subtrees.
func finishTree(ctx context.Context, root *node) {
	workers := min(4, runtime.GOMAXPROCS(0), root.count/256)
	if workers < 2 {
		finishSubtree(ctx, root)
		return
	}
	threshold := max(128, root.count/(workers*4))
	jobs := make(chan *node, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for n := range jobs {
				finishSubtree(ctx, n)
			}
		})
	}
	var parents []*node
	var enqueue func(*node)
	enqueue = func(n *node) {
		if n == nil || ctx.Err() != nil {
			return
		}
		if n.count <= threshold {
			jobs <- n
			return
		}
		enqueue(n.left)
		enqueue(n.right)
		parents = append(parents, n)
	}
	enqueue(root)
	close(jobs)
	wg.Wait()
	for _, n := range parents {
		if ctx.Err() != nil {
			return
		}
		n.content = entryDigest(n.entry)
		n.rehash()
	}
}

func finishSubtree(ctx context.Context, n *node) {
	if n != nil && ctx.Err() == nil {
		finishSubtree(ctx, n.left)
		finishSubtree(ctx, n.right)
		n.content = entryDigest(n.entry)
		n.rehash()
	}
}

func higher(a, b *node) bool {
	c := bytes.Compare(a.priority[:], b.priority[:])
	return c < 0 || c == 0 && a.entry.Path < b.entry.Path
}
func nodeDigest(n *node) [32]byte {
	if n == nil {
		return [32]byte{}
	}
	return n.digest
}
func (n *node) recount() {
	n.count = 1
	n.height = 1
	if n.left != nil {
		n.count += n.left.count
		n.height = max(n.height, n.left.height+1)
	}
	if n.right != nil {
		n.count += n.right.count
		n.height = max(n.height, n.right.height+1)
	}
}
func (n *node) rehash() {
	n.recount()
	var data [96]byte
	l, r := nodeDigest(n.left), nodeDigest(n.right)
	copy(data[:32], l[:])
	copy(data[32:64], n.content[:])
	copy(data[64:], r[:])
	n.digest = sha256.Sum256(data[:])
}

func updateNode(ctx context.Context, n *node, edit Edit) *node {
	if ctx.Err() != nil {
		return n
	}
	if n == nil {
		if edit.Delete {
			return nil
		}
		n = &node{entry: edit.Entry, priority: sha256.Sum256([]byte(edit.Entry.Path)), content: entryDigest(edit.Entry)}
		n.rehash()
		return n
	}
	if edit.Entry.Path == n.entry.Path {
		if edit.Delete {
			return joinNodes(ctx, n.left, n.right)
		}
		if edit.Entry == n.entry {
			return n
		}
		result := *n
		result.entry, result.content = edit.Entry, entryDigest(edit.Entry)
		result.rehash()
		return &result
	}
	result := *n
	if edit.Entry.Path < n.entry.Path {
		result.left = updateNode(ctx, n.left, edit)
		if result.left == n.left {
			return n
		}
		if result.left != nil && higher(result.left, &result) {
			top := *result.left
			result.left = top.right
			result.rehash()
			top.right = &result
			top.rehash()
			return &top
		}
	} else {
		result.right = updateNode(ctx, n.right, edit)
		if result.right == n.right {
			return n
		}
		if result.right != nil && higher(result.right, &result) {
			top := *result.right
			result.right = top.left
			result.rehash()
			top.left = &result
			top.rehash()
			return &top
		}
	}
	result.rehash()
	return &result
}
func joinNodes(ctx context.Context, a, b *node) *node {
	if ctx.Err() != nil {
		return a
	}
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if higher(a, b) {
		n := *a
		n.right = joinNodes(ctx, a.right, b)
		n.rehash()
		return &n
	}
	n := *b
	n.left = joinNodes(ctx, a, b.left)
	n.rehash()
	return &n
}

func diffNodes(ctx context.Context, a, b *node, result *[]Edit) {
	if ctx.Err() != nil {
		return
	}
	if a == b || nodeDigest(a) == nodeDigest(b) {
		return
	}
	if a == nil {
		appendNodeEdits(ctx, b, false, result)
		return
	}
	if b == nil {
		appendNodeEdits(ctx, a, true, result)
		return
	}
	if a.entry.Path == b.entry.Path {
		diffNodes(ctx, a.left, b.left, result)
		if a.entry != b.entry {
			*result = append(*result, Edit{Entry: b.entry})
		}
		diffNodes(ctx, a.right, b.right, result)
		return
	}
	// The higher-priority root cannot exist in the other tree: if it did, it
	// would also be that tree's root. Split around the absent key, preserving
	// unchanged subtrees so their digests can still short-circuit comparison.
	if higher(a, b) {
		left, right := splitAbsent(ctx, b, a.entry.Path)
		diffNodes(ctx, a.left, left, result)
		*result = append(*result, Edit{Entry: a.entry, Delete: true})
		diffNodes(ctx, a.right, right, result)
	} else {
		left, right := splitAbsent(ctx, a, b.entry.Path)
		diffNodes(ctx, left, b.left, result)
		*result = append(*result, Edit{Entry: b.entry})
		diffNodes(ctx, right, b.right, result)
	}
}

// splitAbsent returns entries before/after an absent key. It copies only the
// search path. Digest computation also makes independently built equal subtrees
// skippable; diff correctness never depends on shared allocation history.
func splitAbsent(ctx context.Context, n *node, path string) (*node, *node) {
	if ctx.Err() != nil {
		return n, nil
	}
	if n == nil {
		return nil, nil
	}
	if path < n.entry.Path {
		left, right := splitAbsent(ctx, n.left, path)
		if right == n.left {
			return left, n
		}
		copy := *n
		copy.left = right
		copy.rehash()
		return left, &copy
	}
	left, right := splitAbsent(ctx, n.right, path)
	if left == n.right {
		return n, right
	}
	copy := *n
	copy.right = left
	copy.rehash()
	return &copy, right
}

func appendNodeEdits(ctx context.Context, n *node, remove bool, result *[]Edit) {
	if ctx.Err() != nil {
		return
	}
	if n == nil {
		return
	}
	appendNodeEdits(ctx, n.left, remove, result)
	*result = append(*result, Edit{Entry: n.entry, Delete: remove})
	appendNodeEdits(ctx, n.right, remove, result)
}

// Replacements preserve path priorities and tree shape. Visit the union of
// edited search paths once, so batches do not repeatedly clone and hash their
// shared ancestors. Insertions/deletions use the structural update algorithm.
func replaceNodes(ctx context.Context, n *node, edits []Edit) *node {
	if len(edits) == 0 || n == nil || ctx.Err() != nil {
		return n
	}
	i, found := slices.BinarySearchFunc(edits, n.entry.Path, func(e Edit, p string) int { return strings.Compare(e.Entry.Path, p) })
	left := replaceNodes(ctx, n.left, edits[:i])
	rightStart := i
	if found {
		rightStart++
	}
	right := replaceNodes(ctx, n.right, edits[rightStart:])
	entry := n.entry
	if found {
		entry = edits[i].Entry
	}
	if left == n.left && right == n.right && entry == n.entry {
		return n
	}
	next := *n
	next.left, next.right = left, right
	if entry != n.entry {
		next.entry = entry
		next.content = entryDigest(entry)
	}
	next.rehash()
	return &next
}
