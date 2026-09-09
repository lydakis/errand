package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestWorkspaceUploadAdmissionAndInventory(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"value": "initial\n"})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "uploads")
	if err != nil {
		t.Fatal(err)
	}
	row, err := d.workspaces.read(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	upload, err := d.workspaces.beginUpload(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if upload != nil {
			_ = d.workspaces.finishUpload(upload)
		}
	}()
	before, err := client.StorageStatsDetailed(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upload.dir, "partial"), make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := client.StorageStatsDetailed(ts.URL)
	if err != nil || after.Workspaces.Bytes-before.Workspaces.Bytes != 8192 || after.Details.Workspaces[0].TransferBytes != 8192 {
		t.Fatalf("upload bytes absent from inventory: before=%+v after=%+v err=%v", before, after, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := d.workspaces.beginUpload(ctx, row); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second upload did not wait cancelably: %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(d.workspaces.dir, ".push-upload-*"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("upload admission allocated excess storage: %v %v", entries, err)
	}
	if err := client.RemoveWorkspace(ts.URL, ws.Name); err != nil {
		t.Fatal(err)
	}
	after, err = client.StorageStatsDetailed(ts.URL)
	if err != nil || after.Workspaces.Items != 1 || after.Workspaces.Bytes != 8192 || after.Details.Workspaces[0].TransferBytes != 8192 {
		t.Fatalf("removed workspace hid active upload: %+v %v", after, err)
	}
	if err := d.workspaces.finishUpload(upload); err != nil {
		t.Fatal(err)
	}
	upload = nil
	after, err = client.StorageStatsDetailed(ts.URL)
	if err != nil || after.Workspaces.Items != 0 || after.Workspaces.Bytes != 0 {
		t.Fatalf("upload cleanup retained accounting: %+v %v", after, err)
	}
}

func TestUploadAccountingIsOwnerScoped(t *testing.T) {
	s, err := openWorkspaces(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.root.Close()
	row := workspaceRecord{Owner: "other"}
	upload, err := s.beginUpload(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	defer s.finishUpload(upload)
	if err := os.WriteFile(filepath.Join(upload.dir, "private"), []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	stats := proto.StorageStats{Workspaces: &proto.StorageCategory{}, Details: &proto.StorageDetails{}}
	if err := s.addUploadStorage(t.Context(), "caller", &stats, make(map[string]bool)); err != nil {
		t.Fatal(err)
	}
	if stats.Workspaces.Bytes != 0 || stats.Workspaces.Items != 0 || len(stats.Details.Workspaces) != 0 {
		t.Fatalf("another owner's upload leaked: %+v", stats)
	}
}

func TestFailedUploadCleanupKeepsAdmissionBoundUntilRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := openWorkspaces(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.root.Close()
	row := workspaceRecord{Owner: "owner"}
	upload, err := s.beginUpload(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	// The upload can be inspected, but its parent refuses deletion.
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if err := s.finishUpload(upload); err == nil {
		t.Fatal("expected cleanup to fail in unwritable parent")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := s.beginUpload(ctx, row); err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("cleanup failure lost the upload reservation: %v", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	restarted, err := openWorkspaces(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.root.Close()
	if _, err := os.Stat(upload.dir); !os.IsNotExist(err) {
		t.Fatalf("restart left upload debris: %v", err)
	}
	next, err := restarted.beginUpload(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.finishUpload(next); err != nil {
		t.Fatal(err)
	}
}
