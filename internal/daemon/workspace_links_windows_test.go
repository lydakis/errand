//go:build windows

package daemon

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestWorkspaceDirectorySymlinkPushOnWindows(t *testing.T) {
	d, _ := testDaemon(t)
	root := workspaceWith(t, map[string]string{"target/value": "body"})
	// The lexical first link points through a link created later on extraction.
	for _, link := range []struct{ name, target string }{{"z", "target"}, {"a", "z"}} {
		if err := os.Symlink(link.target, filepath.Join(root, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := snapshot.Build(root, []string{"a", "target", "target/value", "z"})
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := proto.NewULID()
	// Send the runner protocol directly, as a Mac/Linux client would. The
	// Windows client itself does not support local workspace state yet.
	upload := func(metadata any, manifest proto.Manifest, creation bool) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, err := mw.CreateFormField("metadata")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(part).Encode(metadata); err != nil {
			t.Fatal(err)
		}
		if creation {
			part, err = mw.CreateFormField("manifest")
			if err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(part).Encode(manifest); err != nil {
				t.Fatal(err)
			}
		}
		part, err = mw.CreateFormFile("workspace", "workspace.tar")
		if err != nil {
			t.Fatal(err)
		}
		if err := snapshot.PackContext(t.Context(), part, root, manifest); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.SetPathValue("id", workspaceID)
		response := httptest.NewRecorder()
		if creation {
			d.handleWorkspaceCreate(response, req, Identity{})
		} else {
			d.handleWorkspacePush(response, req, Identity{})
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("upload: %d %s", response.Code, response.Body)
		}
		return response
	}
	upload(proto.Workspace{ID: workspaceID, Name: "links"}, baseline, true)
	remote := filepath.Join(d.workspaces.dir, workspaceID, "data")
	if body, err := os.ReadFile(filepath.Join(remote, "a", "value")); err != nil || string(body) != "body" {
		t.Fatalf("initial directory chain = %q, %v", body, err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target", filepath.Join(root, "nested", "alias")); err != nil {
		t.Fatal(err)
	}
	current, err := snapshot.Build(root, []string{"a", "nested", "nested/alias", "target", "target/value", "z"})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := changes.PrepareSourceDelta(t.Context(), baseline, current, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	const clientID = "0123456789abcdef0123456789abcdef"
	transferID := proto.NewULID()
	upload(proto.PushRequest{ID: transferID, ClientID: clientID, Delta: &delta, SourceRoot: current.RootHash()}, delta.RemoteManifest, false)
	req := httptest.NewRequest(http.MethodPost, "/?client="+clientID, strings.NewReader("{}"))
	req.SetPathValue("id", workspaceID)
	req.SetPathValue("transfer", transferID)
	response := httptest.NewRecorder()
	d.handleWorkspacePushApply(response, req, Identity{})
	if response.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", response.Code, response.Body)
	}
	if body, err := os.ReadFile(filepath.Join(remote, "nested", "alias", "value")); err != nil || string(body) != "body" {
		t.Fatalf("pushed directory link = %q, %v", body, err)
	}
}

// Directory links reach a Windows job as directory links, including a link
// through another link that is created after it, and one in a subdirectory.
// The Windows client isn't supported yet, so this goes through a job rather
// than a workspace, whose client keeps POSIX-checked local state.
func TestJobDirectoryLinksOnWindows(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"target/value": "body"})
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ name, target string }{{"z", "target"}, {"a", "z"}, {"nested/alias", "../target"}} {
		if err := os.Symlink(link.target, filepath.Join(root, filepath.FromSlash(link.name))); err != nil {
			t.Fatal(err)
		}
	}
	_, status := submitChangeJob(t, d, ts.URL, root,
		[]string{"cmd", "/d", "/c", `type a\value >nul && type nested\alias\value >nul && exit /b 7`})
	result := status.Result
	if result == nil || result.StartError != "" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result = %+v", result)
	}
}
