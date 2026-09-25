package daemon

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/namedcache"
	"github.com/lydakis/errand/internal/proto"
)

func TestStorageEndpointReportsCacheAndOwnedJobBytes(t *testing.T) {
	d, ts := testDaemon(t)
	insertContent(t, d.cache, "cached")
	d.cfg.ChangeStorage = func(context.Context) (proto.ChangeStorageStats, error) {
		return proto.ChangeStorageStats{StorageCategory: proto.StorageCategory{Items: 2, Bytes: 123}, StoreID: "opaque"}, nil
	}

	jobID := proto.NewULID()
	jobDir := filepath.Join(d.jobsDir(), jobID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "receipt"), []byte("job-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.jobs[jobID] = &Job{ID: jobID, Dir: jobDir}
	d.mu.Unlock()

	resp, err := http.Get(ts.URL + "/v0/storage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stats proto.StorageStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || stats.Cache == nil || stats.Cache.Blobs != 1 ||
		stats.Cache.Bytes != int64(len("cached")) || stats.Jobs.Items != 1 ||
		stats.Jobs.Bytes != int64(len("job-data")) || stats.Changes == nil || stats.Changes.Bytes != 123 || stats.Changes.Items != 2 {
		t.Fatalf("storage response = %s %+v", resp.Status, stats)
	}
}

func TestStorageTreeBytesKeepsKnownBytesWhenInteriorFileDisappears(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept"), []byte("known"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vanished"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}

	bytes, err := storageTreeBytesWithInfo(t.Context(), root, func(entry fs.DirEntry) (fs.FileInfo, error) {
		if entry.Name() == "vanished" {
			return nil, os.ErrNotExist
		}
		return entry.Info()
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes != int64(len("known")) {
		t.Fatalf("storage bytes = %d, want %d", bytes, len("known"))
	}
}

func TestStorageEndpointIncludesOwnedFailedGCTombstones(t *testing.T) {
	d, ts := testDaemon(t)
	jobID := proto.NewULID()
	tombstone := filepath.Join(d.jobsDir(), ".gc-"+jobID+"-"+proto.NewULID())
	if err := os.MkdirAll(tombstone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tombstone, "receipt"), []byte("stranded"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.collected[jobID] = collectedRecord{CollectedAt: d.admissionNow(time.Now())}
	d.mu.Unlock()

	resp, err := http.Get(ts.URL + "/v0/storage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stats proto.StorageStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || stats.Jobs.Items != 1 ||
		stats.Jobs.Bytes != int64(len("stranded")) {
		t.Fatalf("storage response = %s %+v", resp.Status, stats)
	}
}

func TestStorageEndpointMeasuresIdleTreeCachesOnly(t *testing.T) {
	d, ts := testDaemon(t)
	idle := namedcache.Key{Owner: "insecure-test", Project: strings.Repeat("a", 32), Name: "idle"}
	held := namedcache.Key{Owner: "insecure-test", Project: strings.Repeat("a", 32), Name: "held"}
	idleJob, heldJob := proto.NewULID(), proto.NewULID()
	data, err := d.namedCaches.AcquireTree(t.Context(), idle, idleJob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "object"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.namedCaches.ReleaseTree(t.Context(), idle, idleJob); err != nil {
		t.Fatal(err)
	}
	if _, err := d.namedCaches.AcquireTree(t.Context(), held, heldJob); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/v0/storage?verbose=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stats proto.StorageStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	named := stats.NamedCaches
	if resp.StatusCode != http.StatusOK || named == nil || named.Items != 2 || named.Bytes != int64(len("cached")) ||
		named.Unmeasured != 1 || named.Protected != 1 || stats.Details == nil {
		t.Fatalf("storage response = %s %+v", resp.Status, named)
	}
	for _, detail := range stats.Details.NamedCaches {
		if detail.BytesUnknown != (detail.Name == "held") {
			t.Fatalf("named cache details = %+v", stats.Details.NamedCaches)
		}
	}
}
