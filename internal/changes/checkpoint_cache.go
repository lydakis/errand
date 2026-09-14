package changes

import (
	"container/list"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"
)

// CheckpointCache retains immutable decoded records across transfer requests.
// It is owned by a receiver, never by a package global. The zero value disables
// retention. Cache hits still require a fresh guarded read and exact bytes.
// This mutex protects only retention; callers still hold the destination's
// transfer lock across revision checks, application, publication, and GC.
type CheckpointCache struct {
	mu              sync.Mutex
	maxBytes, bytes int64
	maxRecords      int
	records         map[string]*list.Element
	lru             list.List
}

type cachedCheckpoint struct {
	path   string
	record *checkpointRecord
	bytes  int64
}

// NewCheckpointCache bounds retained decoded entries and raw record buffers.
// Active requests can retain their own records after eviction. Budgets account
// for backing capacities and decoded strings, not allocator/runtime overhead.
func NewCheckpointCache(maxRecords int, maxBytes int64) *CheckpointCache {
	return &CheckpointCache{maxRecords: maxRecords, maxBytes: maxBytes}
}
func (c *CheckpointCache) get(path string) *checkpointRecord {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.records[filepath.Clean(path)]; e != nil {
		c.lru.MoveToFront(e)
		return e.Value.(cachedCheckpoint).record
	}
	return nil
}
func (c *CheckpointCache) put(path string, record *checkpointRecord) {
	if c == nil {
		return
	}
	path = filepath.Clean(path)
	size := record.retainedBytes() + int64(len(path))
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.records[path]; e != nil {
		c.remove(e)
	}
	if c.maxRecords <= 0 || size > c.maxBytes {
		return
	}
	for c.lru.Len() >= c.maxRecords || c.bytes > c.maxBytes-size {
		c.remove(c.lru.Back())
	}
	if c.records == nil {
		c.records = make(map[string]*list.Element)
	}
	c.records[path] = c.lru.PushFront(cachedCheckpoint{path, record, size})
	c.bytes += size
}
func (c *CheckpointCache) forget(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.records[filepath.Clean(path)]; e != nil {
		c.remove(e)
	}
}

// ForgetDirectory releases records when their owning workspace is removed.
// Ordinary transfer GC does not remove the checkpoint, which remains pinned.
func (c *CheckpointCache) ForgetDirectory(dir string) {
	if c == nil {
		return
	}
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, e := range c.records {
		if strings.HasPrefix(name, prefix) {
			c.remove(e)
		}
	}
}
func (c *CheckpointCache) remove(e *list.Element) {
	record := e.Value.(cachedCheckpoint)
	delete(c.records, record.path)
	c.bytes -= record.bytes
	c.lru.Remove(e)
}
func (r *checkpointRecord) retainedBytes() int64 {
	s := &r.state
	n := int64(unsafe.Sizeof(*r)) + int64(cap(r.raw)) + int64(cap(s.Manifest.Entries))*int64(unsafe.Sizeof(s.Manifest.Entries[0]))
	n += int64(len(s.Owner) + len(s.SourceID) + len(s.InitialRoot) + len(s.LastReceipt) + len(s.LastRequest) + 64)
	for _, e := range s.Manifest.Entries {
		n += int64(len(e.Path) + len(e.Type) + len(e.SHA256) + len(e.Target))
	}
	return n
}
