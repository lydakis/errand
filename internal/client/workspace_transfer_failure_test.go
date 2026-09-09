package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRejectedWorkspaceCreationReclaimsOrigin(t *testing.T) {
	for _, status := range []int{409, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "data"), []byte(strings.Repeat("x", 32768)), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "creation failed", status) }))
			defer server.Close()
			if _, err := CreateWorkspace(RunOptions{PeerURL: server.URL, Root: root, IncludeAll: true}, "duplicate"); err == nil {
				t.Fatal("expected failure")
			}
			stats, err := workspaceTransferStats(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status == 409 && stats.Bytes != 0 {
				t.Fatalf("rejected creation retains %d bytes", stats.Bytes)
			}
			if status == 502 && stats.Bytes == 0 {
				t.Fatal("uncertain creation lost its origin")
			}
		})
	}
}

func TestTransferGCReportsCorruptCheckpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	prep := prepareSnapshot(root, true, false)
	if prep.err != nil {
		t.Fatal(prep.err)
	}
	const id = "01M2280R0T4152A3BSV4C2976R"
	opts := RunOptions{PeerURL: "http://runner", Root: root}
	if err := recordWorkspaceOrigin(opts, id, prep.manifest); err != nil {
		t.Fatal(err)
	}
	dir, err := workspaceTransferDir(opts.PeerURL, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fetch", "checkpoint.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := workspaceTransferGC(time.Now().Add(time.Hour), false)
	if err == nil || result.Failed != 1 || result.Protected != 0 {
		t.Fatalf("GC hid corruption: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "origin.json")); err != nil {
		t.Fatalf("damaged state must remain: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "fetch", "checkpoint.json")); err != nil {
		t.Fatal(err)
	}
	result, err = workspaceTransferGC(time.Now().Add(time.Hour), false)
	if err == nil || result.Failed != 1 || result.Protected != 0 {
		t.Fatalf("GC hid missing checkpoint: %+v %v", result, err)
	}
}
