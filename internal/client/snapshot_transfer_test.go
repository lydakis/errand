package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestSnapshotNegotiationLargeManifest(t *testing.T) {
	var manifest proto.Manifest
	var hashes []string
	for i := 0; i < 63000; i++ {
		hash := fmt.Sprintf("%064x", i)
		manifest.Entries = append(manifest.Entries, proto.ManifestEntry{Path: fmt.Sprintf("file-%d", i), Type: proto.EntryFile, Mode: 0600, Size: 1, SHA256: hash})
		hashes = append(hashes, hash)
	}
	response, _ := json.Marshal(proto.SnapshotDiffResponse{Missing: hashes})
	if len(response) <= 4<<20 {
		t.Fatal("fixture must exceed the former response limit")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request proto.SnapshotDiffRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Blobs) != len(hashes) {
			t.Errorf("bad negotiation request: %v", err)
		}
		w.Write(response)
	}))
	defer server.Close()
	plan, err := negotiateSnapshotAt(context.Background(), server.URL, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Entries {
		if !plan.ships(entry) {
			t.Fatalf("cold-cache plan omitted %s", entry.Path)
		}
	}
}

func TestPushAllowsStagingBeyondControlDeadline(t *testing.T) {
	oldDirect, oldMaintenance := directHTTP, maintenanceHTTP
	t.Cleanup(func() { directHTTP, maintenanceHTTP = oldDirect, oldMaintenance })
	control := directTransport.Clone()
	control.ResponseHeaderTimeout = 20 * time.Millisecond
	bulk := maintenanceTransport.Clone()
	bulk.ResponseHeaderTimeout = time.Second
	t.Cleanup(control.CloseIdleConnections)
	t.Cleanup(bulk.CloseIdleConnections)
	directHTTP = &http.Client{Transport: control}
	maintenanceHTTP = &http.Client{Transport: bulk}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		// Private reconstruction and durable staging continue after upload.
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(proto.PushResult{ID: "transfer", WorkspaceID: "workspace"})
	}))
	defer server.Close()
	if _, err := uploadPushOnce(server.URL, "workspace", t.TempDir(), proto.PushRequest{ID: "transfer", Delta: &proto.ChangeBundle{}}, nil, shipPlan{}); err != nil {
		t.Fatalf("valid slow staging was cut off by the control timeout: %v", err)
	}
}

func TestSnapshotNegotiationDeduplicatesContent(t *testing.T) {
	manifest := proto.Manifest{Entries: []proto.ManifestEntry{
		{Path: "a", Type: proto.EntryFile, Size: 1, SHA256: fmt.Sprintf("%064x", 1)},
		{Path: "b", Type: proto.EntryFile, Size: 1, SHA256: fmt.Sprintf("%064x", 1)},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request proto.SnapshotDiffRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Blobs) != 1 {
			t.Errorf("negotiated %d blobs for one unique content hash", len(request.Blobs))
		}
		json.NewEncoder(w).Encode(proto.SnapshotDiffResponse{Missing: []string{manifest.Entries[0].SHA256}})
	}))
	defer server.Close()
	plan, err := negotiateSnapshotAt(context.Background(), server.URL, manifest)
	if err != nil || !plan.ships(manifest.Entries[0]) || !plan.ships(manifest.Entries[1]) {
		t.Fatalf("duplicate paths must both be shipped when their content is missing: %+v %v", plan, err)
	}
}

func TestPushFallbackRequiresExplicitCacheMissAndStopsAfterFullUpload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		code    string
		uploads int
	}{
		{"cache miss", http.StatusConflict, proto.ErrorCodeSnapshotCacheMiss, 2},
		{"other conflict", http.StatusConflict, "", 1},
		{"forbidden", http.StatusForbidden, proto.ErrorCodeSnapshotCacheMiss, 1},
		{"server failure", http.StatusInternalServerError, proto.ErrorCodeSnapshotCacheMiss, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "file"), []byte(strings.Repeat("x", 128<<10)), 0600); err != nil {
				t.Fatal(err)
			}
			manifest, err := snapshot.Build(root, []string{"file"})
			if err != nil {
				t.Fatal(err)
			}
			uploads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if strings.HasSuffix(r.URL.Path, "/diff") {
					json.NewEncoder(w).Encode(proto.SnapshotDiffResponse{})
					return
				}
				uploads++
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(proto.APIError{Code: tc.code, Error: "rejected"})
			}))
			defer server.Close()
			if _, err := uploadPush(server.URL, "workspace", root, proto.PushRequest{ID: "transfer", Delta: &proto.ChangeBundle{RemoteManifest: manifest}, SourceRoot: manifest.RootHash()}, nil); err == nil {
				t.Fatal("rejected push succeeded")
			}
			if uploads != tc.uploads {
				t.Fatalf("uploads=%d, want %d", uploads, tc.uploads)
			}
		})
	}
}

func replyMissingSnapshotBlobs(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var req proto.SnapshotDiffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Error(err)
		http.Error(w, "invalid diff", 400)
		return
	}
	missing := make([]string, 0, len(req.Blobs))
	for _, blob := range req.Blobs {
		missing = append(missing, blob.SHA256)
	}
	json.NewEncoder(w).Encode(proto.SnapshotDiffResponse{Missing: missing})
}

func TestRequiredSnapshotEndpointDoesNotPermitUploadFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	uploads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			uploads++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	opts := RunOptions{PeerURL: server.URL, Root: root, Argv: []string{"true"}, Stdout: io.Discard, Stderr: io.Discard}
	if code := Run(opts); code != ExitTransaction {
		t.Fatalf("missing snapshot endpoint: exit=%d", code)
	}
	if _, err := CreateWorkspace(opts, "dev"); !IsNotFound(err) {
		t.Fatalf("missing creation negotiation endpoint: %v", err)
	}
	if _, err := pushBase(server.URL, proto.NewULID(), "0123456789abcdef0123456789abcdef"); !IsNotFound(err) {
		t.Fatalf("missing push checkpoint endpoint: %v", err)
	}
	if uploads != 0 {
		t.Fatalf("sent %d uploads after required endpoints were missing", uploads)
	}
}
