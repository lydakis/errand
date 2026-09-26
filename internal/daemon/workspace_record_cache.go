package daemon

import (
	"container/list"
	"sync"
	"unsafe"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// workspaceRecordCacheBytes matches the store's checkpoint cache budget. A
// workspace whose record does not fit also decodes its larger checkpoint on
// every push, so retaining its record would not make the push cheap.
const workspaceRecordCacheBytes = 64 << 20

// workspaceRecordCache retains decoded workspace records by ID and evicts the
// least recently used once their approximate retained size passes maxBytes.
// A hit also requires the opened file's stamp. The zero value retains nothing.
type workspaceRecordCache struct {
	mu              sync.Mutex
	maxBytes, bytes int64
	entries         map[string]*list.Element
	lru             list.List
}

type cachedWorkspaceRecord struct {
	id     string
	stamp  snapshot.ObservationStamp
	record workspaceRecord
	bytes  int64
}

// get returns a copy the caller may mutate.
func (c *workspaceRecordCache) get(id string, stamp snapshot.ObservationStamp) (workspaceRecord, bool) {
	c.mu.Lock()
	e := c.entries[id]
	if e == nil || e.Value.(cachedWorkspaceRecord).stamp != stamp {
		c.mu.Unlock()
		return workspaceRecord{}, false
	}
	c.lru.MoveToFront(e)
	r := e.Value.(cachedWorkspaceRecord).record
	c.mu.Unlock()
	// Retained records are never modified, so they can be copied unlocked.
	return cloneWorkspaceRecord(r), true
}

// put takes ownership of r and replaces any record retained for id.
func (c *workspaceRecordCache) put(id string, stamp snapshot.ObservationStamp, r workspaceRecord) {
	size := int64(unsafe.Sizeof(cachedWorkspaceRecord{})+unsafe.Sizeof(list.Element{})) + int64(len(id)) + r.retainedBytes()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[id]; e != nil {
		c.remove(e)
	}
	if size > c.maxBytes {
		return
	}
	for c.bytes > c.maxBytes-size {
		c.remove(c.lru.Back())
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	c.entries[id] = c.lru.PushFront(cachedWorkspaceRecord{id, stamp, r, size})
	c.bytes += size
}

// forget drops id's record when its workspace is removed.
func (c *workspaceRecordCache) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[id]; e != nil {
		c.remove(e)
	}
}

func (c *workspaceRecordCache) remove(e *list.Element) {
	cached := e.Value.(cachedWorkspaceRecord)
	delete(c.entries, cached.id)
	c.bytes -= cached.bytes
	c.lru.Remove(e)
}

// retainedBytes counts backing arrays and string bytes reachable from r, not
// allocator or map overhead. The creation manifest dominates.
func (r *workspaceRecord) retainedBytes() int64 {
	const stringHeader = int64(unsafe.Sizeof(""))
	n := int64(len(r.Where) + len(r.ID) + len(r.Name) + len(r.Project) + len(r.CacheProjectID) + len(r.Selection.Prefix) + len(r.CacheLeaseID) + len(r.Owner))
	n += int64(cap(r.Manifest.Entries)) * int64(unsafe.Sizeof(proto.ManifestEntry{}))
	for _, e := range r.Manifest.Entries {
		n += int64(len(e.Path) + len(e.Type) + len(e.SHA256) + len(e.Target))
	}
	n += int64(cap(r.Selection.Caches)) * int64(unsafe.Sizeof(proto.CacheBinding{}))
	for _, c := range r.Selection.Caches {
		n += int64(len(c.Name) + len(c.Path))
	}
	for _, values := range [][]string{r.JobIDs, r.Selection.Artifacts, r.Selection.Ignore, r.TreeCaches} {
		n += int64(cap(values)) * stringHeader
		for _, v := range values {
			n += int64(len(v))
		}
	}
	for name, base := range r.TreeBaselines {
		n += 2*stringHeader + int64(len(name)+len(base))
	}
	return n
}
