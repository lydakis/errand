package changes

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func observationManifest(files map[string]string) proto.Manifest {
	var m proto.Manifest
	for name, body := range files {
		e := proto.ManifestEntry{Path: name, Type: proto.EntryFile, Mode: 0600, Size: int64(len(body)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}
		if body == "/" {
			e.Type, e.Mode, e.Size, e.SHA256 = proto.EntryDir, 0700, 0, ""
		}
		m.Entries = append(m.Entries, e)
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m
}

func TestObservedSourceRetentionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		initial, current, previous, want map[string]string
		policy                           proto.SelectionPolicy
		modeChange                       bool
	}{
		{name: "ignored ancestor preserves prior artifact", previous: map[string]string{"out": "/", "out/report": "saved"}, want: map[string]string{"out": "/", "out/report": "saved"}, policy: proto.SelectionPolicy{Ignore: []string{"out/"}}},
		{name: "declared artifact deletion is observed", previous: map[string]string{"out": "/", "out/report": "saved"}, policy: proto.SelectionPolicy{Ignore: []string{"out/"}, Artifacts: []string{"out"}}},
		{name: "file replaces directory and unobserved descendants", initial: map[string]string{"out": "/"}, current: map[string]string{"out": "replacement"}, previous: map[string]string{"out": "/", "out/report": "saved"}, want: map[string]string{"out": "replacement"}, policy: proto.SelectionPolicy{Ignore: []string{"out/"}}},
		{name: "directory metadata preserves unobserved descendants", initial: map[string]string{"out": "/"}, current: map[string]string{"out": "/"}, previous: map[string]string{"out": "/", "out/report": "saved"}, want: map[string]string{"out": "/", "out/report": "saved"}, policy: proto.SelectionPolicy{Ignore: []string{"out/"}}, modeChange: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial, current, previous, want := observationManifest(tc.initial), observationManifest(tc.current), observationManifest(tc.previous), observationManifest(tc.want)
			if tc.modeChange {
				current.Entries[0].Mode = 0755
				want.Entries[0].Mode = 0755
			}
			bundle, err := workspaceDelta(context.Background(), initial, current, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ObservedSource(initial, bundle, previous, tc.policy)
			if err != nil || got.RootHash() != want.RootHash() {
				t.Fatalf("observed %+v, want %+v: %v", got, want, err)
			}
		})
	}
}
