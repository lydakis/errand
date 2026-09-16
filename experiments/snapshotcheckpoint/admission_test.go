//go:build darwin || linux

package snapshotcheckpoint

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestAdmissionUsesExpandedGitSelection(t *testing.T) {
	ctx := context.Background()
	root, cache := t.TempDir(), t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	for _, name := range []string{"input", "d/a", "d/b"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := defaultPreparation(false)
	config.entryLimit = 4
	first, err := prepare(ctx, root, cache, snapshot.SelectOptions{}, config)
	if err != nil || !first.Written {
		t.Fatalf("seed: %+v %v", first, err)
	}
	if err := os.Mkdir(filepath.Join(root, "e"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "e/c"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	paths, _, _, err := snapshot.SelectFiles(root)
	if err != nil || len(paths) != 4 {
		t.Fatalf("raw selection: %v %v", paths, err)
	}
	for _, direct := range []bool{false, true} {
		config.store = observationStore{direct: direct}
		got, err := prepare(ctx, root, cache, snapshot.SelectOptions{}, config)
		if err != nil {
			t.Fatal(err)
		}
		if got.CacheStatus != "oversized" || got.CheckpointBytes != 0 || got.Written || got.Reused != 0 || got.Hashed != 4 || got.State.Len() != 6 {
			t.Fatalf("loaded unusable checkpoint: %+v", got)
		}
	}
}

func TestUnsupportedIdentityBypassesExistingCheckpoint(t *testing.T) {
	for _, empty := range []bool{false, true} {
		root, cache := fixture(t)
		if empty {
			if err := os.WriteFile(filepath.Join(root, ".errandignore"), []byte("*\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		oracle(t, root, cache)
		before, err := os.ReadFile(filepath.Join(cache, "checkpoint"))
		if err != nil {
			t.Fatal(err)
		}
		config := defaultPreparation(false)
		config.identify = func(string, proto.SelectionPolicy, snapshot.SelectOptions) (identity, error) {
			return identity{}, errors.New("no identity evidence")
		}
		got, err := prepare(context.Background(), root, cache, snapshot.SelectOptions{}, config)
		if err != nil {
			t.Fatal(err)
		}
		if got.CacheStatus != "unsupported" || got.CacheError == "" || got.Written || got.Reused != 0 || got.CheckpointBytes != 0 {
			t.Fatalf("unsupported: %+v", got)
		}
		cold, err := Cold(context.Background(), root, snapshot.SelectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		a, err := got.State.RootHash(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b, err := cold.State.RootHash(context.Background())
		if err != nil || a != b {
			t.Fatalf("fallback root: %v", err)
		}
		after, err := os.ReadFile(filepath.Join(cache, "checkpoint"))
		if err != nil || string(before) != string(after) {
			t.Fatal("unsupported fallback changed checkpoint")
		}
	}
}

func TestDirectLoadValidatesWithoutRestoringSnapshot(t *testing.T) {
	ctx := context.Background()
	root, cache := fixture(t)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	oracle(t, root, cache)
	_, _, policy, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkoutIdentity(root, policy, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, status, _, err := readCheckpoint(ctx, cache, key, false)
	if err != nil || status != "hit" || loaded.state != nil {
		t.Fatalf("direct load restored state: %s %v", status, err)
	}
	for _, entries := range [][]observation{
		{{Entry: proto.ManifestEntry{Path: ".", Type: proto.EntryDir}}},
		{{Entry: proto.ManifestEntry{Path: "a", Type: proto.EntryFile, Size: math.MaxInt64, SHA256: strings.Repeat("0", 64)}}, {Entry: proto.ManifestEntry{Path: "b", Type: proto.EntryFile, Size: 1, SHA256: strings.Repeat("0", 64)}}},
		{{Entry: proto.ManifestEntry{Path: "b", Type: proto.EntryDir}}, {Entry: proto.ManifestEntry{Path: "a", Type: proto.EntryDir}}},
	} {
		if _, err := writeRawCheckpoint(ctx, cache, checkpoint{Identity: key, Entries: entries}); err != nil {
			t.Fatal(err)
		}
		for _, restore := range []bool{false, true} {
			if _, status, _, err := readCheckpoint(ctx, cache, key, restore); err != nil || status != "corrupt" {
				t.Fatalf("accepted invalid metadata: %s %v", status, err)
			}
		}
	}
}
