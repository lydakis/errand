package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

func recordTestWorkspaceOrigin(t *testing.T, id string, files map[string]string) (root, dir string, initial proto.Manifest) {
	t.Helper()
	root = t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prep := prepareSnapshot(root, true, false)
	if prep.err != nil {
		t.Fatal(prep.err)
	}
	const peer = "http://runner"
	if err := recordWorkspaceOrigin(RunOptions{PeerURL: peer, Root: root}, id, prep.manifest); err != nil {
		t.Fatal(err)
	}
	dir, err := workspaceTransferDir(peer, id)
	if err != nil {
		t.Fatal(err)
	}
	return root, dir, prep.manifest
}

func TestWorkspaceOriginOmitsCreationManifest(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, dir, want := recordTestWorkspaceOrigin(t, "01M2280R0T4152A3BSV4C2976R", map[string]string{"a": "a", "nested/b": "b"})
	_, otherDir, _ := recordTestWorkspaceOrigin(t, "01M2280R0T4152A3BSV4C2976S", map[string]string{"c": "c"})
	raw, err := os.ReadFile(filepath.Join(dir, "origin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"initial_root", "peer_url", "root", "root_identity", "workspace_id"}) {
		t.Fatalf("origin record fields: %v", keys)
	}
	o, err := readWorkspaceOrigin(dir)
	if err != nil {
		t.Fatal(err)
	}
	if o.InitialRoot != want.RootHash() {
		t.Fatalf("origin names creation root %s, want %s", o.InitialRoot, want.RootHash())
	}
	got, err := o.initial(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("creation manifest: got %+v, want %+v", got, want)
	}
	// Pushes read every relationship's origin during recovery. None of them
	// may depend on a creation manifest, including another checkout's.
	for _, d := range []string{dir, otherDir} {
		if err := os.WriteFile(filepath.Join(d, "initial.json"), []byte("{bad"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readWorkspaceOrigin(dir); err != nil {
		t.Fatalf("origin read needs the creation manifest: %v", err)
	}
	if err := recoverWorkspaceApplications(root); err != nil {
		t.Fatalf("recovery needs a creation manifest: %v", err)
	}
	stats, err := workspaceTransferStats(context.Background())
	if err != nil || stats.Items != 2 {
		t.Fatalf("inventory needs a creation manifest: %+v %v", stats, err)
	}
	if _, err := o.initial(dir); err == nil {
		t.Fatal("damaged creation manifest was accepted")
	}
}

func TestWorkspaceOriginRejectsUnboundCreationManifest(t *testing.T) {
	for _, damage := range []string{"foreign", "truncated", "missing"} {
		t.Run(damage, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			_, damaged, _ := recordTestWorkspaceOrigin(t, "01M2280R0T4152A3BSV4C2976R", map[string]string{"a": "a"})
			_, healthy, _ := recordTestWorkspaceOrigin(t, "01M2280R0T4152A3BSV4C2976S", map[string]string{"b": "b"})
			path := filepath.Join(damaged, "initial.json")
			switch damage {
			case "foreign":
				// A valid manifest of another relationship must not pin or seed this one.
				raw, err := os.ReadFile(filepath.Join(healthy, "initial.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw[:len(raw)/2], 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			o, err := readWorkspaceOrigin(damaged)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.initial(damaged); err == nil {
				t.Fatal("unbound creation manifest was accepted")
			}
			garbage := filepath.Join(healthy, ".source-abandoned")
			if err := os.Mkdir(garbage, 0700); err != nil {
				t.Fatal(err)
			}
			result, err := workspaceTransferGC(time.Now().Add(time.Hour), false)
			if err == nil || result.Failed != 1 || result.Removed != 1 {
				t.Fatalf("GC with damaged creation manifest: %+v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(damaged, "origin.json")); err != nil {
				t.Fatalf("damaged state must remain: %v", err)
			}
			if _, err := os.Stat(garbage); !os.IsNotExist(err) {
				t.Fatal("healthy relationship was skipped", err)
			}
		})
	}
}

func TestWorkspaceOriginRejectsEmbeddedManifestFormat(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, dir, initial := recordTestWorkspaceOrigin(t, "01M2280R0T4152A3BSV4C2976R", map[string]string{"a": "a"})
	o, err := readWorkspaceOrigin(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Earlier clients embedded the creation manifest and wrote no initial.json.
	embedded := struct {
		Root        string              `json:"root"`
		RootID      fsidentity.Identity `json:"root_identity"`
		WorkspaceID string              `json:"workspace_id"`
		PeerURL     string              `json:"peer_url"`
		Initial     proto.Manifest      `json:"initial"`
	}{o.Root, o.RootID, o.WorkspaceID, o.PeerURL, initial}
	raw, err := json.Marshal(embedded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "origin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "initial.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkspaceOrigin(dir); err == nil || os.IsNotExist(err) || !strings.Contains(err.Error(), "invalid workspace origin") {
		t.Fatalf("embedded-manifest origin: %v", err)
	}
	if err := recoverWorkspaceApplications(root); err != nil {
		t.Fatalf("an unreadable origin blocked recovery: %v", err)
	}
	result, err := workspaceTransferGC(time.Now().Add(time.Hour), false)
	if err == nil || result.Failed != 1 || result.Removed != 0 {
		t.Fatalf("GC with embedded-manifest origin: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "origin.json")); err != nil {
		t.Fatalf("unreadable state must remain: %v", err)
	}
}

func TestWorkspaceOriginFollowsDurableCreationManifest(t *testing.T) {
	t.Run("manifest write fails", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		root := t.TempDir()
		prep := prepareSnapshot(root, true, false)
		if prep.err != nil {
			t.Fatal(prep.err)
		}
		const peer, id = "http://runner", "01M2280R0T4152A3BSV4C2976R"
		dir, err := workspaceTransferDir(peer, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "initial.json"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := recordWorkspaceOrigin(RunOptions{PeerURL: peer, Root: root}, id, prep.manifest); err == nil {
			t.Fatal("origin recorded without its creation manifest")
		}
		if _, err := os.Stat(filepath.Join(dir, "origin.json")); !os.IsNotExist(err) {
			t.Fatalf("origin written before its creation manifest: %v", err)
		}
	})
	t.Run("interrupted before origin", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		root, dir, _ := recordTestWorkspaceOrigin(t, "01M2280R0T4152A3BSV4C2976R", map[string]string{"a": "a"})
		if err := os.Remove(filepath.Join(dir, "origin.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := readWorkspaceOrigin(dir); !os.IsNotExist(err) {
			t.Fatalf("incomplete relationship: %v", err)
		}
		if err := recoverWorkspaceApplications(root); err != nil {
			t.Fatal(err)
		}
		result, err := workspaceTransferGC(time.Now().Add(time.Hour), false)
		if err != nil || result.Failed != 0 || result.Removed != 1 {
			t.Fatalf("GC of incomplete relationship: %+v %v", result, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("incomplete relationship remains: %v", err)
		}
	})
}

func TestTransferGCKeepsCreationBodies(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, dir, initial := recordTestWorkspaceOrigin(t, "01M2280R0T4152A3BSV4C2976R", map[string]string{"value": "creation\n"})
	o, err := readWorkspaceOrigin(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Move the checkpoint past the creation snapshot, so only the creation
	// manifest still references the original body.
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "value"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prep := prepareSnapshot(source, true, false)
	if prep.err != nil {
		t.Fatal(prep.err)
	}
	session := o.session(dir)
	id := proto.NewULID()
	if _, _, err := session.Stage(context.Background(), id, source, prep.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Apply(id, nil, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "value")); err != nil || string(got) != "changed\n" {
		t.Fatalf("applied source: %q %v", got, err)
	}
	result, err := workspaceTransferGC(time.Now().Add(time.Hour), false)
	if err != nil || result.Failed != 0 {
		t.Fatalf("GC: %+v %v", result, err)
	}
	for _, e := range initial.Entries {
		if e.Type != proto.EntryFile {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "fetch", "blobs", e.SHA256)); err != nil {
			t.Fatalf("GC collected creation body of %s: %v", e.Path, err)
		}
	}
}
