package changes

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestCheckpointCacheIndependentRequests(t *testing.T) {
	root, bundle, staged := applyFixture(t, "original\n", "source\n")
	target := transferTarget(t, root)
	template := checkpointFor(t, target)
	template.Reuse = NewCheckpointCache(4, 1<<20)
	request := func() *TransferCheckpoint {
		return &TransferCheckpoint{Root: template.Root, RootID: template.RootID, Owner: template.Owner,
			SourceID: template.SourceID, StatePath: template.StatePath, Reuse: template.Reuse}
	}
	if _, err := request().Initialize(bundle.BaseManifest); err != nil {
		t.Fatal(err)
	}
	// Separate read-only handles can share decoded metadata and memoized identity.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 4 {
				c := request()
				r, err := c.readVersion()
				if err != nil {
					t.Error(err)
					return
				}
				if r.rootHash() != bundle.BaseManifest.RootHash() {
					t.Error("wrong source identity")
				}
				v := r.export()
				v.Manifest.Entries[0].SHA256 = "caller mutation"
			}
		})
	}
	wg.Wait()
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	// Publication through an uncached handle must also invalidate reuse by bytes.
	writer := request()
	writer.Reuse = nil
	want, err := writer.Advance(0, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	got, err := request().Read()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("stale revision: %+v, %v", got, err)
	}
	bad := request()
	bad.SourceID = "different sender"
	if _, err := bad.Read(); err == nil {
		t.Fatal("shared cache skipped relationship check")
	}
	// Same-size corruption with restored times must fail on a new request too.
	raw, err := os.ReadFile(template.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(template.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":9`), 1)
	if err := os.WriteFile(template.StatePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(template.StatePath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := request().Read(); err == nil {
		t.Fatal("shared cache accepted corrupt record")
	}
}

func TestCheckpointCacheBoundsAndRemoval(t *testing.T) {
	// Accounting/eviction is a resource contract; active readers remain valid.
	r := &checkpointRecord{raw: []byte("record"), state: checkpointState{CheckpointVersion: CheckpointVersion{Manifest: proto.Manifest{}}}}
	size := r.retainedBytes() + int64(len("/one/checkpoint"))
	for _, tc := range []struct {
		name    string
		records int
		bytes   int64
	}{
		{"record limit", 1, 1 << 20}, {"byte limit", 4, size}, {"disabled", 0, 1 << 20}, {"oversize", 4, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCheckpointCache(tc.records, tc.bytes)
			c.put("/one/checkpoint", r)
			c.put("/two/checkpoint", r)
			if len(c.records) > tc.records || c.bytes > tc.bytes {
				t.Fatal("retention exceeded budget")
			}
			if c.get("/one/checkpoint") != nil {
				t.Fatal("old entry retained past bound")
			}
			c.ForgetDirectory("/two")
			if len(c.records) != 0 || c.bytes != 0 {
				t.Fatal("removed workspace retained")
			}
			if r.rootHash() != (proto.Manifest{}).RootHash() {
				t.Fatal("eviction damaged active reader")
			}
		})
	}
	c := NewCheckpointCache(4, 1<<20)
	c.put("/workspace/checkpoint", r)
	c.put("/workspace-other/checkpoint", r)
	c.ForgetDirectory("/workspace")
	if c.get("/workspace-other/checkpoint") == nil {
		t.Fatal("removed a different workspace")
	}
}

func TestTransferSessionInitializeExistingValidation(t *testing.T) {
	root, b, staged := applyFixture(t, "original\n", "source\n")
	target := transferTarget(t, root)
	s := TransferSession{Directory: t.TempDir(), Root: root, RootID: target.RootID, Owner: "owner", SourceID: "sender", MaxSourceBytes: 1 << 20}
	// A failed initial retention must never publish a checkpoint.
	missing := filepath.Join(staged, "missing")
	if err := s.Initialize(t.Context(), missing, b.BaseManifest); err == nil {
		t.Fatal("missing initial bodies accepted")
	}
	if _, err := s.Checkpoint().Read(); !os.IsNotExist(err) {
		t.Fatalf("checkpoint published before source durability: %v", err)
	}
	if err := s.Initialize(t.Context(), filepath.Join(staged, "base"), b.BaseManifest); err != nil {
		t.Fatal(err)
	}
	for _, initial := range []proto.Manifest{b.RemoteManifest, {Entries: []proto.ManifestEntry{{Path: "../escape", Type: proto.EntryDir}}}} {
		if err := s.Initialize(t.Context(), missing, initial); err == nil {
			t.Fatal("invalid creation snapshot accepted")
		}
	}
	if err := s.Initialize(t.Context(), missing, b.BaseManifest); err != nil {
		t.Fatalf("existing checkpoint required source again: %v", err)
	}
}
