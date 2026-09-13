package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceDescriptorSupportsLegacyServersAndSharedValidation(t *testing.T) {
	for _, scenario := range []string{"compact", "legacy", "invalid-id", "invalid-name"} {
		t.Run(scenario, func(t *testing.T) {
			want := proto.Workspace{ID: proto.NewULID(), Name: "descriptor", Selection: proto.SelectionPolicy{Artifacts: []string{"output"}}, Manifest: proto.Manifest{Entries: []proto.ManifestEntry{{Path: "dir", Type: proto.EntryDir, Mode: 0755}}}}
			if scenario == "invalid-id" {
				want.ID = "invalid"
			}
			if scenario == "invalid-name" {
				want.Name = "invalid/name"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response := want
				if query := r.URL.Query().Get("manifest"); query != "" {
					if query != "omit" {
						t.Errorf("unexpected manifest query: %q", query)
					}
					if scenario != "legacy" {
						response.Manifest = proto.Manifest{}
					}
				}
				json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			descriptor, descriptorErr := getWorkspaceDescriptor(server.URL, "descriptor")
			full, fullErr := GetWorkspace(server.URL, "descriptor")
			if scenario == "invalid-id" || scenario == "invalid-name" {
				if descriptorErr == nil || fullErr == nil {
					t.Fatalf("invalid identity accepted: descriptor=%v full=%v", descriptorErr, fullErr)
				}
				return
			}
			if descriptorErr != nil || fullErr != nil {
				t.Fatalf("descriptor=%v full=%v", descriptorErr, fullErr)
			}
			if !reflect.DeepEqual(full, want) {
				t.Fatalf("full lookup lost creation manifest: %+v", full)
			}
			if scenario == "compact" {
				want.Manifest = proto.Manifest{}
			}
			if !reflect.DeepEqual(descriptor, want) {
				t.Fatalf("descriptor=%+v want=%+v", descriptor, want)
			}
		})
	}
}
