package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestWorkspaceRecordCacheFollowsReplacementAndIsolatesCallers(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"a": "one\n", "b": "two\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "cached")
	if err != nil {
		t.Fatal(err)
	}
	store := d.workspaces
	first, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating a returned record must not leak into later reads.
	first.Manifest.Entries[0].Path = "mutated"
	first.JobIDs = append(first.JobIDs, "mutated")
	second, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Manifest.Entries[0].Path == "mutated" || len(second.JobIDs) != 0 {
		t.Fatalf("cached record was mutated through a returned copy: %+v", second)
	}

	// A durable replacement is observed immediately.
	second.Name = "renamed"
	if err := store.write(second); err != nil {
		t.Fatal(err)
	}
	third, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if third.Name != "renamed" {
		t.Fatalf("read returned a stale cached record: %q", third.Name)
	}
	if !reflect.DeepEqual(third.Manifest, second.Manifest) {
		t.Fatal("replacement changed the manifest")
	}

	// An in-place overwrite changes the stamp and is decoded, not served from cache.
	if err := os.WriteFile(filepath.Join(store.dir, ws.ID, "workspace.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(ws.ID); err == nil {
		t.Fatal("corrupt workspace record was served from cache")
	}
}

func TestWorkspaceRecordCacheForgetsRemovedWorkspace(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"a": "one\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "removed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.workspaces.read(ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := retainedWorkspaceRecord(&d.workspaces.recordCache, ws.ID); !ok {
		t.Fatal("read did not retain the record")
	}
	if err := client.RemoveWorkspace(ts.URL, ws.Name); err != nil {
		t.Fatal(err)
	}
	c := &d.workspaces.recordCache
	if _, ok := retainedWorkspaceRecord(c, ws.ID); ok {
		t.Fatal("removed workspace's record is still retained")
	}
	checkWorkspaceRecordCacheAccounting(t, c)
	if c.bytes != 0 {
		t.Fatalf("removal left %d retained bytes", c.bytes)
	}
}

func TestWorkspaceRecordCacheBoundsRetainedSize(t *testing.T) {
	c := &workspaceRecordCache{maxBytes: 1 << 20}
	stamp := snapshot.ObservationStamp{Inode: 1}

	// A record larger than the whole budget is not retained, and does not leave
	// an older record for the same workspace behind.
	c.put("big", stamp, syntheticWorkspaceRecord("big", 10))
	if _, ok := retainedWorkspaceRecord(c, "big"); !ok {
		t.Fatal("small record was not retained")
	}
	huge := syntheticWorkspaceRecord("big", 20000)
	if huge.retainedBytes() <= c.maxBytes {
		t.Fatalf("test record is only %d bytes", huge.retainedBytes())
	}
	c.put("big", stamp, huge)
	if _, ok := c.get("big", stamp); ok {
		t.Fatal("record larger than the budget was retained")
	}
	checkWorkspaceRecordCacheAccounting(t, c)
	if c.bytes != 0 {
		t.Fatalf("oversized record left %d retained bytes", c.bytes)
	}

	// Many records totalling several budgets evict the least recently used,
	// while a record read between insertions stays.
	const records = 40
	var total int64
	for i := range records {
		id := fmt.Sprintf("w%02d", i)
		r := syntheticWorkspaceRecord(id, 1000)
		total += r.retainedBytes()
		c.put(id, stamp, r)
		checkWorkspaceRecordCacheAccounting(t, c)
		if _, ok := c.get("w00", stamp); !ok {
			t.Fatalf("recently used record was evicted after %d insertions", i+1)
		}
	}
	if total <= 4*c.maxBytes {
		t.Fatalf("test records total only %d bytes", total)
	}
	if _, ok := retainedWorkspaceRecord(c, "w01"); ok {
		t.Fatal("least recently used record was not evicted")
	}
	if _, ok := retainedWorkspaceRecord(c, fmt.Sprintf("w%02d", records-1)); !ok {
		t.Fatal("newest record was evicted")
	}
	if n := c.lru.Len(); n < 2 || n >= records {
		t.Fatalf("retained %d of %d records", n, records)
	}
}

func TestWorkspaceRecordCacheHitsAcrossRepeatedPushes(t *testing.T) {
	d, ts := testDaemon(t)
	c := &d.workspaces.recordCache
	type pushed struct {
		root string
		ws   proto.Workspace
	}
	var workspaces []pushed
	for _, name := range []string{"hot-a", "hot-b", "hot-c"} {
		root := workspaceWith(t, map[string]string{"value": "initial\n", "other": "fixed\n"})
		ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, name)
		if err != nil {
			t.Fatal(err)
		}
		workspaces = append(workspaces, pushed{root, ws})
	}
	retained := make(map[string]*proto.ManifestEntry)
	for round := range 3 {
		for _, w := range workspaces {
			if err := os.WriteFile(filepath.Join(w.root, "value"), []byte(fmt.Sprintf("round %d\n", round)), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := client.PushChanges(client.PushOptions{PeerURL: ts.URL, Root: w.root, Workspace: w.ws.Name}); err != nil {
				t.Fatal(err)
			}
			cached, ok := retainedWorkspaceRecord(c, w.ws.ID)
			if !ok {
				t.Fatalf("round %d: push did not retain %s", round, w.ws.Name)
			}
			c.mu.Lock()
			front := c.lru.Front().Value.(cachedWorkspaceRecord).id
			c.mu.Unlock()
			if front != w.ws.ID {
				t.Fatalf("round %d: push to %s did not read its record", round, w.ws.Name)
			}
			// A miss stores a fresh copy, so the same backing array proves a hit.
			entries := &cached.record.Manifest.Entries[0]
			if round == 0 {
				retained[w.ws.ID] = entries
			} else if retained[w.ws.ID] != entries {
				t.Fatalf("round %d: push to %s decoded its record again", round, w.ws.Name)
			}
		}
	}
	checkWorkspaceRecordCacheAccounting(t, c)
}

func retainedWorkspaceRecord(c *workspaceRecordCache, id string) (cachedWorkspaceRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[id]
	if e == nil {
		return cachedWorkspaceRecord{}, false
	}
	return e.Value.(cachedWorkspaceRecord), true
}

func checkWorkspaceRecordCacheAccounting(t *testing.T, c *workspaceRecordCache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var sum int64
	for e := c.lru.Front(); e != nil; e = e.Next() {
		cached := e.Value.(cachedWorkspaceRecord)
		if c.entries[cached.id] != e {
			t.Fatalf("index does not point at %s", cached.id)
		}
		sum += cached.bytes
	}
	if len(c.entries) != c.lru.Len() || sum != c.bytes || c.bytes > c.maxBytes {
		t.Fatalf("cache accounting: %d indexed, %d listed, %d bytes counted, %d summed, budget %d", len(c.entries), c.lru.Len(), c.bytes, sum, c.maxBytes)
	}
}

func syntheticWorkspaceRecord(id string, files int) workspaceRecord {
	r := workspaceRecord{Workspace: proto.Workspace{ID: id, Name: id}, Owner: "owner"}
	for i := range files {
		r.Manifest.Entries = append(r.Manifest.Entries, proto.ManifestEntry{Path: fmt.Sprintf("dir/file-%06d.go", i), Type: "file", Mode: 0o644, SHA256: fmt.Sprintf("%064x", i)})
	}
	return r
}

// The creation base is retained with its record: hits share it, and a replaced
// workspace.json gets a base copied from its own manifest.
func TestWorkspaceRecordCacheRetainsCreationBaseWithRecord(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"a": "one\n", "b": "two\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "creation")
	if err != nil {
		t.Fatal(err)
	}
	store := d.workspaces
	first, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.creation == nil || second.creation != first.creation || !reflect.DeepEqual(first.creation.Manifest(), first.Manifest) {
		t.Fatal("cache hit did not share the creation base of its record")
	}
	second.Manifest.Entries[len(second.Manifest.Entries)-1].SHA256 = strings.Repeat("0", 64)
	if err := store.write(second); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		next, err := store.read(ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		if next.creation == first.creation || !reflect.DeepEqual(next.creation.Manifest(), second.Manifest) {
			t.Fatal("replaced record kept the previous creation base")
		}
	}
	record := syntheticWorkspaceRecord("sized", 100)
	withBase := record
	withBase.creation = changes.NewSourceBase(record.Manifest)
	if withBase.retainedBytes() <= record.retainedBytes() {
		t.Fatal("retained size does not count the creation base's entries")
	}
	checkWorkspaceRecordCacheAccounting(t, &store.recordCache)
}
