package changes

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestCheckpointReadOwnsManifest(t *testing.T) {
	root, bundle, _ := applyFixture(t, "original\n", "source\n")
	c := checkpointFor(t, transferTarget(t, root))
	if _, err := c.Initialize(bundle.BaseManifest); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		v, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v.Manifest, bundle.BaseManifest) {
			t.Fatal("caller changed retained checkpoint")
		}
		v.Manifest.Entries[0].SHA256 = "caller mutation"
	}
}

func TestCheckpointReadRevalidatesRecordAndRelationship(t *testing.T) {
	for _, mutation := range []string{"in-place", "replace", "missing", "symlink", "relationship"} {
		t.Run(mutation, func(t *testing.T) {
			c := checkpointFor(t, transferTarget(t, t.TempDir()))
			if _, err := c.Initialize(proto.Manifest{}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Read(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(c.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(c.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "in-place", "replace":
				bad := bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":9`), 1)
				if bytes.Equal(raw, bad) {
					t.Fatal("fixture did not change record")
				}
				name := c.StatePath
				if mutation == "replace" {
					name += ".new"
				}
				if err := os.WriteFile(name, bad, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if mutation == "replace" {
					if err := os.Rename(name, c.StatePath); err != nil {
						t.Fatal(err)
					}
				}
			case "missing", "symlink":
				if err := os.Remove(c.StatePath); err != nil {
					t.Fatal(err)
				}
				if mutation == "symlink" {
					other := filepath.Join(t.TempDir(), "record")
					if err := os.WriteFile(other, raw, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(other, c.StatePath); err != nil {
						t.Fatal(err)
					}
				}
			case "relationship":
				c.SourceID = "another-source"
			}
			if _, err := c.Read(); err == nil {
				t.Fatal("reused stale checkpoint")
			}
		})
	}
}

func TestCheckpointReadObservesOtherHandleAdvance(t *testing.T) {
	root, bundle, staged := applyFixture(t, "original\n", "source\n")
	target := transferTarget(t, root)
	c := checkpointFor(t, target)
	if _, err := c.Initialize(bundle.BaseManifest); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(); err != nil {
		t.Fatal(err)
	}
	other := TransferCheckpoint{Root: c.Root, RootID: c.RootID, Owner: c.Owner, SourceID: c.SourceID, StatePath: c.StatePath}
	if _, err := target.Apply(staged, bundle, nil, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	want, err := other.Advance(0, target.StatePath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Read()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("stale checkpoint: %+v, %v", got, err)
	}
}

// A record retained after publication must be exactly what decoding the
// published bytes yields, and must not alias the caller's manifest.
func TestCheckpointPublicationRetainsDecodedEquivalentRecord(t *testing.T) {
	root, bundle, _ := applyFixture(t, "original\n", "source\n")
	c := checkpointFor(t, transferTarget(t, root))
	base := cloneSourceManifest(bundle.BaseManifest)
	if _, err := c.Initialize(base); err != nil {
		t.Fatal(err)
	}
	if c.cache == nil || c.cache.record == nil {
		t.Fatal("published checkpoint was not retained")
	}
	raw, err := os.ReadFile(c.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, c.cache.record.raw) {
		t.Fatal("retained bytes differ from the published record")
	}
	var decoded checkpointState
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, c.cache.record.state) {
		t.Fatalf("retained state differs from decoding:\n%+v\n%+v", c.cache.record.state, decoded)
	}
	base.Entries[0].SHA256 = "caller mutation"
	v, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v.Manifest, bundle.BaseManifest) {
		t.Fatal("caller mutation reached the retained checkpoint")
	}
}

func TestCheckpointPublicationSkipsStatesJSONWouldRewrite(t *testing.T) {
	c := checkpointFor(t, transferTarget(t, t.TempDir()))
	base := proto.Manifest{Entries: []proto.ManifestEntry{{Path: "bad\xffname", Type: proto.EntryFile, Mode: 0o644,
		SHA256: strings.Repeat("0", 64)}}}
	if _, err := c.Initialize(base); err != nil {
		t.Skip("manifest rejected before publication:", err)
	}
	if c.cache != nil && c.cache.record != nil {
		t.Fatal("retained a record whose strings JSON rewrites")
	}
}
