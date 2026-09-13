package snapshotindex

import (
	"bytes"
	"crypto/sha256"
	"runtime"
	"sync"

	"github.com/lydakis/errand/internal/proto"
)

// tree is a deterministic Cartesian tree: path orders entries, SHA256(path)
// orders priorities. Updates copy the search path and share unchanged subtrees.
// Cryptographic priorities avoid systematic imbalance for sorted input, but are
// not a worst-case height guarantee. This prototype is not a hostile-input API.
type tree struct{ root *node }
type node struct {
	entry       proto.ManifestEntry
	priority    [32]byte
	content     [32]byte
	digest      [32]byte
	left, right *node
	count       int
}

func NewTree(entries []proto.ManifestEntry) Index {
	// Sorted entries form a Cartesian tree in O(N), then one hash pass.
	var localStack [64]*node
	stack := localStack[:0]
	var block []node
	for i, entry := range entries {
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
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			stack[len(stack)-1].right = n
		}
		stack = append(stack, n)
	}
	if len(stack) == 0 {
		return &tree{}
	}
	root := stack[0]
	for i := len(stack) - 1; i >= 0; i-- {
		stack[i].recount()
	}
	finishTree(root)
	return &tree{root: root}
}

// Hash independent subtrees before their ancestors. Only the new, unpublished
// nodes are mutable here. A small bounded pool respects GOMAXPROCS; small trees
// avoid goroutine setup. Updates never use this pool or rehash shared subtrees.
func finishTree(root *node) {
	workers := min(4, runtime.GOMAXPROCS(0), root.count/256)
	if workers < 2 {
		finishSubtree(root)
		return
	}
	threshold := max(128, root.count/(workers*4))
	jobs := make(chan *node, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for n := range jobs {
				finishSubtree(n)
			}
		})
	}
	var parents []*node
	var enqueue func(*node)
	enqueue = func(n *node) {
		if n == nil {
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
		n.content = entryDigest(n.entry)
		n.rehash()
	}
}

func finishSubtree(n *node) {
	if n != nil {
		finishSubtree(n.left)
		finishSubtree(n.right)
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
	if n.left != nil {
		n.count += n.left.count
	}
	if n.right != nil {
		n.count += n.right.count
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

func (t *tree) Update(edits []Edit) Index {
	root := t.root
	for _, edit := range edits {
		root = updateNode(root, edit)
	}
	return &tree{root: root}
}
func updateNode(n *node, edit Edit) *node {
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
			return joinNodes(n.left, n.right)
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
		result.left = updateNode(n.left, edit)
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
		result.right = updateNode(n.right, edit)
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
func joinNodes(a, b *node) *node {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if higher(a, b) {
		n := *a
		n.right = joinNodes(a.right, b)
		n.rehash()
		return &n
	}
	n := *b
	n.left = joinNodes(a, b.left)
	n.rehash()
	return &n
}

func (t *tree) Diff(next Index) []Edit {
	var result []Edit
	diffNodes(t.root, next.(*tree).root, &result)
	return result
}
func diffNodes(a, b *node, result *[]Edit) {
	if a == b || nodeDigest(a) == nodeDigest(b) {
		return
	}
	if a == nil {
		appendNodeEdits(b, false, result)
		return
	}
	if b == nil {
		appendNodeEdits(a, true, result)
		return
	}
	if a.entry.Path == b.entry.Path {
		diffNodes(a.left, b.left, result)
		if a.entry != b.entry {
			*result = append(*result, Edit{Entry: b.entry})
		}
		diffNodes(a.right, b.right, result)
		return
	}
	// The higher-priority root cannot exist in the other tree: if it did, it
	// would also be that tree's root. Split around the absent key, preserving
	// unchanged subtrees so their digests can still short-circuit comparison.
	if higher(a, b) {
		left, right := splitAbsent(b, a.entry.Path)
		diffNodes(a.left, left, result)
		*result = append(*result, Edit{Entry: a.entry, Delete: true})
		diffNodes(a.right, right, result)
	} else {
		left, right := splitAbsent(a, b.entry.Path)
		diffNodes(left, b.left, result)
		*result = append(*result, Edit{Entry: b.entry})
		diffNodes(right, b.right, result)
	}
}

// splitAbsent returns entries before/after an absent key. It copies only the
// search path. Digest computation also makes independently built equal subtrees
// skippable; diff correctness never depends on shared allocation history.
func splitAbsent(n *node, path string) (*node, *node) {
	if n == nil {
		return nil, nil
	}
	if path < n.entry.Path {
		left, right := splitAbsent(n.left, path)
		if right == n.left {
			return left, n
		}
		copy := *n
		copy.left = right
		copy.rehash()
		return left, &copy
	}
	left, right := splitAbsent(n.right, path)
	if left == n.right {
		return n, right
	}
	copy := *n
	copy.right = left
	copy.rehash()
	return &copy, right
}

func appendNodeEdits(n *node, remove bool, result *[]Edit) {
	if n == nil {
		return
	}
	appendNodeEdits(n.left, remove, result)
	*result = append(*result, Edit{Entry: n.entry, Delete: remove})
	appendNodeEdits(n.right, remove, result)
}
func appendNodes(entries []proto.ManifestEntry, n *node) []proto.ManifestEntry {
	if n == nil {
		return entries
	}
	entries = appendNodes(entries, n.left)
	entries = append(entries, n.entry)
	return appendNodes(entries, n.right)
}
func nodeEntries(n *node) []proto.ManifestEntry {
	if n == nil {
		return nil
	}
	return appendNodes(make([]proto.ManifestEntry, 0, n.count), n)
}
func (t *tree) Manifest() proto.Manifest { return proto.Manifest{Entries: nodeEntries(t.root)} }
func (t *tree) Digest() [32]byte         { return nodeDigest(t.root) }
