package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/namedcache"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestCacheBindingFailureSettlesPartialLeases(t *testing.T) {
	d, server := testDaemon(t)
	project := strings.Repeat("a", 32)
	held := namedcache.Key{Owner: "insecure-test", Project: project, Name: "busy"}
	heldJob := proto.NewULID()
	if _, err := d.namedCaches.Acquire(context.Background(), held, heldJob); err != nil {
		t.Fatal(err)
	}
	id := proto.NewULID()
	spec := proto.Spec{Argv: []string{"true"}, ManifestRoot: (proto.Manifest{}).RootHash(), NoSnapshot: true, Limits: proto.DefaultLimits(), CacheProjectID: project, Selection: proto.SelectionPolicy{Caches: []proto.CacheBinding{{Name: "free", Path: "out"}, {Name: "busy", Path: "busy"}}}}
	resp := rawSubmitSpec(t, server.URL, id, t.TempDir(), spec, proto.Manifest{})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("submit: %s %s", resp.Status, body)
	}
	resp.Body.Close()
	result := waitTerminal(t, server.URL, id).Result
	if result.Started || !strings.Contains(result.StartError, "leased") || !result.CleanupOK {
		t.Fatalf("result: %+v", result)
	}
	entries, err := d.namedCaches.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Key.Name == "free" && entry.LeaseID != "" {
			t.Fatal("partial lease leaked")
		}
		if entry.Key.Name == "busy" && entry.LeaseID != heldJob {
			t.Fatal("other lease changed")
		}
	}
}

func TestCacheBindingParentCasing(t *testing.T) {
	for _, second := range []string{"Build/two", "build/two"} {
		t.Run(second, func(t *testing.T) {
			d, server := testDaemon(t)
			id := proto.NewULID()
			spec := proto.Spec{
				Argv:         []string{"/bin/sh", "-c", "printf report > report.txt"},
				ManifestRoot: (proto.Manifest{}).RootHash(), NoSnapshot: true,
				Limits: proto.DefaultLimits(), CacheProjectID: strings.Repeat("a", 32),
				Selection: proto.SelectionPolicy{Caches: []proto.CacheBinding{
					{Name: "one", Path: "Build/one"}, {Name: "two", Path: second},
				}},
			}
			resp := rawSubmitSpec(t, server.URL, id, t.TempDir(), spec, proto.Manifest{})
			if second == "build/two" {
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "casing") {
					t.Fatalf("ambiguous bindings must fail admission: %s %s %v", resp.Status, body, err)
				}
				entries, err := d.namedCaches.Inventory(context.Background())
				if err != nil || len(entries) != 0 {
					t.Fatalf("rejected bindings acquired caches: %v %v", entries, err)
				}
				return
			}
			if resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("submit: %s %s", resp.Status, body)
			}
			resp.Body.Close()
			result := waitTerminal(t, server.URL, id).Result
			if !result.Started || result.ExitCode == nil || *result.ExitCode != 0 || !result.ChangesOK {
				t.Fatalf("valid bindings must run and retain results: %+v", result)
			}
			if !result.CleanupOK {
				t.Fatalf("cleanup: %+v", result)
			}
			entries, err := d.namedCaches.Inventory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.LeaseID != "" {
					t.Fatalf("lease left after settlement: %+v", entry)
				}
			}
		})
	}
}

func TestNamedCacheRestartRecovery(t *testing.T) {
	for _, unresolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre-start", true: "unreadable-scope"}[unresolved], func(t *testing.T) {
			state := t.TempDir()
			cfg := Config{StateDir: state, InsecureNoAuth: true}
			d, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			id := proto.NewULID()
			dir := filepath.Join(d.jobsDir(), id)
			if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0700); err != nil {
				t.Fatal(err)
			}
			j := newJob(id, dir)
			j.Spec = proto.Spec{Argv: []string{"true"}, ManifestRoot: (proto.Manifest{}).RootHash(), Limits: proto.DefaultLimits(), CacheProjectID: strings.Repeat("b", 32), Selection: proto.SelectionPolicy{Caches: []proto.CacheBinding{{Name: "build", Path: "target"}}}}
			if err := j.writeJSON("spec.json", proto.NewReceiptSpec(j.Spec)); err != nil {
				t.Fatal(err)
			}
			if err := j.writeJSON("admission.json", proto.Admission{Method: "insecure-test", Time: time.Now()}); err != nil {
				t.Fatal(err)
			}
			key := namedcache.Key{Owner: "insecure-test", Project: j.Spec.CacheProjectID, Name: "build"}
			data, err := d.namedCaches.Acquire(context.Background(), key, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(data, "state"), []byte("cache"), 0600); err != nil {
				t.Fatal(err)
			}
			if unresolved {
				if err := j.writeJSON("scope.json", scopeRecord{Token: "unreadable"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			d, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			entries, err := d.namedCaches.Inventory(context.Background())
			if err != nil || len(entries) != 1 {
				t.Fatalf("inventory: %v %v", entries, err)
			}
			if (entries[0].LeaseID != "") != unresolved {
				t.Fatalf("recovery: %+v", entries[0])
			}
			if !unresolved && entries[0].Bytes != 5 {
				t.Fatal(entries[0])
			}
		})
	}
}

func TestNamedCacheStatsFilterOwner(t *testing.T) {
	d, err := New(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i, owner := range []string{"tailnet-user:1", "tailnet-user:2"} {
		key := namedcache.Key{Owner: owner, Project: "project", Name: "build"}
		id := proto.NewULID()
		data, err := d.namedCaches.Acquire(context.Background(), key, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "object"), make([]byte, 3+i), 0600); err != nil {
			t.Fatal(err)
		}
		if err := d.namedCaches.Release(context.Background(), key, id); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if _, err := d.namedCaches.Acquire(context.Background(), key, proto.NewULID()); err != nil {
				t.Fatal(err)
			}
		}
	}
	response := httptest.NewRecorder()
	d.handleStorageStats(response, httptest.NewRequest(http.MethodGet, "/v0/storage", nil), Identity{Login: "one@example.test", UserID: 1})
	var stats proto.StorageStats
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || stats.NamedCaches == nil || stats.NamedCaches.Items != 1 || stats.NamedCaches.Bytes != 3 || stats.NamedCaches.Protected != 0 {
		t.Fatalf("owner inventory: %d %+v", response.Code, stats.NamedCaches)
	}
}

func TestNamedCacheAdmissionRefusesSnapshotOverlap(t *testing.T) {
	d, server := testDaemon(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("local data"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := snapshot.Build(root, []string{"target"})
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{Argv: []string{"true"}, ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(), CacheProjectID: strings.Repeat("a", 32), Selection: proto.SelectionPolicy{Caches: []proto.CacheBinding{{Name: "build", Path: "target"}}}}
	resp := rawSubmitSpec(t, server.URL, proto.NewULID(), root, spec, manifest)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal(resp.Status)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.jobs) != 0 {
		t.Fatal("admitted overlapping snapshot")
	}
}
