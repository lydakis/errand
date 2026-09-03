package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

var testManifestRoot = (proto.Manifest{}).RootHash()

func writeTestChangeMultipart(mw *multipart.Writer, bundle proto.ChangeBundle, jobDir string) error {
	metadata, err := mw.CreateFormField("bundle")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(metadata).Encode(bundle); err != nil {
		return err
	}
	for _, archive := range []struct {
		name string
		open func(string) (*os.File, error)
	}{
		{name: "base", open: changeops.OpenBaseArchive},
		{name: "remote", open: changeops.OpenRemoteArchive},
	} {
		part, err := mw.CreateFormFile(archive.name, archive.name+".tar")
		if err != nil {
			return err
		}
		file, err := archive.open(jobDir)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(part, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
	}
	return mw.Close()
}

func extractTestChangeBundle(t *testing.T, jobDir string, bundle proto.ChangeBundle) string {
	t.Helper()
	staged := t.TempDir()
	for _, tree := range []struct {
		name    string
		open    func(string) (*os.File, error)
		extract func(io.Reader, string, proto.ChangeBundle, int64) error
	}{
		{name: "base", open: changeops.OpenBaseArchive, extract: changeops.ExtractBase},
		{name: "remote", open: changeops.OpenRemoteArchive, extract: changeops.ExtractRemote},
	} {
		destination := filepath.Join(staged, tree.name)
		if err := os.Mkdir(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		archive, err := tree.open(jobDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.extract(archive, destination, bundle, 1<<30); err != nil {
			archive.Close()
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return staged
}

func testChangeApplyFixture(t *testing.T, baseValue, remoteValue string) (string, proto.ChangeBundle, string) {
	t.Helper()
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte(baseValue), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"artifact"})
	if err != nil {
		t.Fatal(err)
	}
	if err := changeops.CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte(remoteValue), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := changeops.CollectWorkspaceChangesContext(context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte(baseValue), 0o600); err != nil {
		t.Fatal(err)
	}
	return local, bundle, extractTestChangeBundle(t, jobDir, bundle)
}

func TestLocalChangeClientIDIsStable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first, err := localChangeClientID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := localChangeClientID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !proto.ValidChangeClientID(first) {
		t.Fatalf("local change client IDs = %q and %q", first, second)
	}
}

func TestEnsurePrivateLocalDirectorySkipsMutationWhenAlreadySecure(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	err := ensurePrivateLocalDirectoryWithChmod(path, func(*os.File, os.FileMode) error {
		called = true
		return errors.New("chmod forbidden")
	})
	if err != nil || called {
		t.Fatalf("secure directory = called %v, error %v", called, err)
	}
}

func TestEnsurePrivateLocalDirectoryToleratesForbiddenPrivacyTightening(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	err := ensurePrivateLocalDirectoryWithChmod(path, func(*os.File, os.FileMode) error {
		return errors.New("chmod forbidden")
	})
	if err != nil {
		t.Fatalf("readable but not writable directory was rejected: %v", err)
	}
}

func TestEnsurePrivateLocalDirectoryFailsWhenUnsafeModeCannotBeRepaired(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0o733); err != nil {
		t.Fatal(err)
	}
	err := ensurePrivateLocalDirectoryWithChmod(path, func(*os.File, os.FileMode) error {
		return errors.New("chmod forbidden")
	})
	if err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Fatalf("unsafe directory error = %v", err)
	}
}

func TestEnsurePrivateLocalDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	path := filepath.Join(root, "state")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateLocalDirectory(path); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink state directory error = %v", err)
	}
}

func TestEnsurePrivateLocalDirectoryRejectsWritableAncestor(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "state")
	if err := ensurePrivateLocalDirectory(path); err == nil || !strings.Contains(err.Error(), "ancestor") {
		t.Fatalf("writable ancestor error = %v", err)
	}
}

func TestEnsurePrivateLocalDirectoryAllowsStickyWritableAncestor(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateLocalDirectory(filepath.Join(parent, "state")); err != nil {
		t.Fatalf("sticky writable ancestor was rejected: %v", err)
	}
}

func TestEnsurePrivateLocalDirectoryRejectsWritableLexicalSymlinkAncestor(t *testing.T) {
	unsafeParent := t.TempDir()
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(unsafeParent, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateLocalDirectory(filepath.Join(link, "state")); err == nil ||
		!strings.Contains(err.Error(), "writable by other users") {
		t.Fatalf("writable lexical ancestor error = %v", err)
	}
}

func TestInitializeNoSnapshotChangeStateBindsEmptyManifestToWorkspace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	jobID := proto.NewULID()
	opts := RunOptions{PeerURL: "http://runner.test", Root: root, NoSnapshot: true}
	if err := initializeChangeState(context.Background(), &opts, jobID, testManifestRoot); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalChangeState(opts.PeerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Root != root || state.RootID.IsZero() || state.ManifestRoot != testManifestRoot {
		t.Fatalf("no-snapshot change state = %+v", state)
	}
}

func TestAutomaticApplyWorkerAppliesCompletedDetachedJob(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := snapshot.Build(remote, []string{"artifact"})
	if err != nil {
		t.Fatal(err)
	}
	if err := changeops.CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, collected, err := changeops.CollectWorkspaceChangesContext(
		context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !collected {
		t.Fatal("changed workspace did not produce a bundle")
	}
	summary := &proto.ChangeSummary{
		Paths: bundle.Paths, PathCount: len(bundle.Paths), BundleRoot: bundle.RootHash(), Bytes: bundle.Bytes,
	}
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}

	jobID := proto.NewULID()
	zero := 0
	changeRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/jobs/" + jobID:
			_ = json.NewEncoder(w).Encode(proto.JobStatus{ID: jobID, State: proto.StateExited, Result: &proto.Result{
				ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true, Changes: summary,
			}})
		case "/v0/jobs/" + jobID + "/changes":
			changeRequests++
			if changeRequests == 1 {
				http.Error(w, "try again", http.StatusServiceUnavailable)
				return
			}
			mw := multipart.NewWriter(w)
			w.Header().Set("Content-Type", mw.FormDataContentType())
			if err := writeTestChangeMultipart(mw, bundle, jobDir); err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: server.URL, Root: local, ManifestRoot: baseline.RootHash(),
		SubmissionStarted: true, AdmissionConfirmed: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyPending,
	}); err != nil {
		t.Fatal(err)
	}

	if err := runAutomaticApplyWorkerContext(context.Background(), server.URL, jobID, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "remote" {
		t.Fatalf("detached automatic apply value = %q, %v", got, err)
	}
	state, err := loadLocalChangeState(server.URL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.AutomaticApply != automaticApplyApplied || state.AutomaticApplyDir == "" {
		t.Fatalf("automatic apply state = %+v", state)
	}
	if changeRequests != 2 {
		t.Fatalf("change download requests = %d, want retry after transient failure", changeRequests)
	}
}

func TestManualApplyRecoversFailedAutomaticApply(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	local, bundle, staged := testChangeApplyFixture(t, "base", "remote")
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	opts := RunOptions{PeerURL: peerURL, Root: local, ApplyOnSuccess: true}
	if err := initializeChangeState(context.Background(), &opts, jobID, bundle.BaselineRoot); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	state.SubmissionStarted = true
	state.AdmissionConfirmed = true
	state.Terminal = true
	state.AutomaticApply = automaticApplyFailed
	state.AutomaticApplyErr = "temporary fetch failure"
	if err := saveLocalChangeState(state); err != nil {
		t.Fatal(err)
	}

	if _, err := applyChangeBundle(peerURL, jobID, local, staged, bundle, nil, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "remote" {
		t.Fatalf("manual recovery value = %q, %v", got, err)
	}
	state, err = loadLocalChangeState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.AutomaticApply != automaticApplyApplied || state.AutomaticApplyErr != "" || state.AutomaticApplyDir != staged {
		t.Fatalf("recovered automatic apply state = %+v", state)
	}
}

func TestPartialManualApplyDoesNotCompleteAutomaticApplyPolicy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(remote, name), []byte("base"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := snapshot.Build(remote, []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := changeops.CaptureWorkspaceBaseContext(context.Background(), remote, jobDir, baseline); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(remote, name), []byte("remote"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bundle, collected, err := changeops.CollectWorkspaceChangesContext(
		context.Background(), remote, jobDir, baseline, proto.SelectionPolicy{}, 1<<20,
	)
	if err != nil || !collected {
		t.Fatalf("collecting changes = %t, %v", collected, err)
	}
	staged := extractTestChangeBundle(t, jobDir, bundle)
	local := t.TempDir()
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(local, name), []byte("base"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, Root: local,
		ManifestRoot: baseline.RootHash(), SubmissionStarted: true, AdmissionConfirmed: true,
		Terminal: true, ApplyOnSuccess: true, AutomaticApply: automaticApplyFailed,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := applyChangeBundle(
		peerURL, jobID, local, staged, bundle, map[string]bool{"first": true}, false,
	); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.AutomaticApply != automaticApplyFailed {
		t.Fatalf("partial manual apply completed automatic policy: %+v", state)
	}
	first, _ := os.ReadFile(filepath.Join(local, "first"))
	second, _ := os.ReadFile(filepath.Join(local, "second"))
	if string(first) != "remote" || string(second) != "base" {
		t.Fatalf("partial apply values = %q, %q", first, second)
	}
}

func TestChangeGCProtectsUnfinishedAutomaticApply(t *testing.T) {
	now := time.Now()
	state := localChangeState{
		SubmissionStarted: true, Terminal: true, ApplyOnSuccess: true,
		AutomaticApply: automaticApplyPending,
	}
	if !localChangeStateNeedsProtection(state, false, now.Add(-time.Hour), now) {
		t.Fatal("unfinished automatic apply was not protected from local change GC")
	}
	state.AutomaticApply = automaticApplyApplied
	if localChangeStateNeedsProtection(state, false, now.Add(-time.Hour), now) {
		t.Fatal("completed automatic apply remained protected from age-based GC")
	}
}

func TestApplyChangeBundleRejectsDifferentSubmittedManifest(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, Root: root, ManifestRoot: testManifestRoot,
	}); err != nil {
		t.Fatal(err)
	}
	bundle := proto.ChangeBundle{
		V: changeops.BundleVersion, BaselineRoot: strings.Repeat("a", 64),
	}
	if _, err := applyChangeBundle(peerURL, jobID, root, t.TempDir(), bundle, nil, false); err == nil ||
		!strings.Contains(err.Error(), "submitted workspace") {
		t.Fatalf("mismatched manifest apply error = %v", err)
	}
}

func TestFetchChangesExplainsMissingAndFailedWorkspaceChanges(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tc := range []struct {
		name   string
		result proto.Result
		want   string
	}{
		{name: "unchanged", result: proto.Result{ChangesOK: true}, want: "produced no workspace changes"},
		{
			name: "unchanged with unrelated failure",
			result: proto.Result{
				ChangesOK: true, TransactionError: "workspace cleanup failed",
			},
			want: "produced no workspace changes",
		},
		{name: "retention failed", result: proto.Result{ChangesOK: false, TransactionError: "permission denied"}, want: "permission denied"},
		{name: "retention failed without detail", result: proto.Result{ChangesOK: false}, want: "were not retained"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := proto.NewULID()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(proto.JobDetails{
					JobStatus: proto.JobStatus{ID: jobID, State: proto.StateExited, Result: &tc.result},
				})
			}))
			defer server.Close()
			_, err := FetchChanges(ChangeFetchOptions{PeerURL: server.URL, JobID: jobID})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("fetch error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFetchedChangePathReturnsDeletionMetadata(t *testing.T) {
	staged := t.TempDir()
	bundle := proto.ChangeBundle{
		Paths: []string{"artifact"},
	}
	got := fetchedChangePath(staged, bundle, map[string]bool{"artifact": true}, "artifact")
	want := filepath.Join(staged, "bundle.json")
	if got != want {
		t.Fatalf("fetched deletion path = %q, want metadata path %q", got, want)
	}
}

func TestSelectChangePathAcceptsRetainedDescendantForFetch(t *testing.T) {
	bundle := proto.ChangeBundle{
		Paths: []string{"dist"},
		RemoteManifest: proto.Manifest{Entries: []proto.ManifestEntry{
			{Path: "dist", Type: proto.EntryDir, Mode: 0o755},
			{Path: "dist/app.js", Type: proto.EntryFile, Mode: 0o600, Size: 3},
		}},
	}
	selected, err := selectChangePath(bundle, "dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !selected["dist"] {
		t.Fatalf("selected roots = %v, want dist", selected)
	}
	staged := t.TempDir()
	if got, want := fetchedChangePath(staged, bundle, selected, "dist/app.js"),
		filepath.Join(staged, "remote", "dist", "app.js"); got != want {
		t.Fatalf("fetched descendant path = %q, want %q", got, want)
	}
	if err := validateApplySelection(selected, "dist/app.js"); err == nil ||
		!strings.Contains(err.Error(), "apply the root") {
		t.Fatalf("apply selection error = %v", err)
	}
}

func TestSelectChangePathAcceptsDeletedRetainedDescendant(t *testing.T) {
	bundle := proto.ChangeBundle{
		Paths: []string{"dist"},
		BaseManifest: proto.Manifest{Entries: []proto.ManifestEntry{
			{Path: "dist", Type: proto.EntryDir, Mode: 0o755},
			{Path: "dist/removed.js", Type: proto.EntryFile, Mode: 0o600, Size: 3},
		}},
		RemoteManifest: proto.Manifest{Entries: []proto.ManifestEntry{
			{Path: "dist", Type: proto.EntryDir, Mode: 0o755},
		}},
	}
	selected, err := selectChangePath(bundle, "dist/removed.js")
	if err != nil || !selected["dist"] {
		t.Fatalf("deleted descendant selection = %v, %v", selected, err)
	}
	staged := t.TempDir()
	if got, want := fetchedChangePath(staged, bundle, selected, "dist/removed.js"),
		filepath.Join(staged, "bundle.json"); got != want {
		t.Fatalf("deleted descendant fetch path = %q, want %q", got, want)
	}
}

func TestLocalChangeClientIDCleansInterruptedTemporaryFile(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := filepath.Join(stateHome, "errand")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, ".client-id-interrupted")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := localChangeClientID()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.ValidChangeClientID(id) {
		t.Fatalf("local change client ID = %q", id)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("interrupted client ID file survived: %v", err)
	}
}

func TestRecoveryIgnoresMalformedUnrelatedChangeState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateRoot, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(stateRoot, "jobs")
	if err := os.MkdirAll(jobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, strings.Repeat("a", 64)+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(t.TempDir()); err != nil {
		t.Fatalf("unrelated malformed state blocked recovery: %v", err)
	}
}

func TestRecoveryWithoutWorkspaceTransactionIgnoresGlobalStateDirectory(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	stateRoot, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "jobs"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(t.TempDir()); err != nil {
		t.Fatalf("recovery without a workspace transaction read global state: %v", err)
	}
}

func TestRecoveryFailsClosedWhenMalformedStateMayOwnWorkspaceTransaction(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateRoot, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(stateRoot, "jobs")
	if err := os.MkdirAll(jobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobs, strings.Repeat("a", 64)+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, changeops.NewApplyTransaction()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(workspace); err == nil || !strings.Contains(err.Error(), "loading") {
		t.Fatalf("recovery with unowned transaction error = %v", err)
	}
}

func TestRecoveryFailsClosedWhenWorkspaceTransactionHasNoStateDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, changeops.NewApplyTransaction()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(workspace); err == nil || !strings.Contains(err.Error(), "no matching local change state") {
		t.Fatalf("recovery without private state error = %v", err)
	}
}

func TestRecoveryFailsClosedWhenWorkspaceTransactionHasNoMatchingState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stateRoot, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateRoot, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, changeops.NewApplyTransaction()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := recoverWorkspaceApplications(workspace); err == nil || !strings.Contains(err.Error(), "no matching local change state") {
		t.Fatalf("recovery without matching state error = %v", err)
	}
}

func TestLocalChangeStateRejectsDifferentPeer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: "http://runner-a.test", ManifestRoot: testManifestRoot, Root: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLocalChangeState("http://runner-b.test", jobID); !os.IsNotExist(err) {
		t.Fatalf("different-peer lookup error = %v, want not exist", err)
	}
}

func TestDefiniteSubmitRejectionRemovesChangeState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/snapshot/diff" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "runner capacity is full"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		Stdout: io.Discard, Stderr: io.Discard,
	}, make(chan os.Signal), testInterruptNotifications(), nil)
	if code != ExitTransaction {
		t.Fatalf("rejected run exit = %d", code)
	}
	entries, err := os.ReadDir(filepath.Join(stateHome, "errand", "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("definitely rejected job retained change state: %v", entries)
	}
}

func TestSubmitRetryConflictRetainsChangeStateAndHandle(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	var puts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/snapshot/diff" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			puts++
			if puts == 1 {
				panic(http.ErrAbortHandler)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "job id exists, but request identity cannot be verified after restart",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	var stderr bytes.Buffer
	code := runWithDetachNotifications(RunOptions{
		PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
		Stdout: io.Discard, Stderr: &stderr,
	}, make(chan os.Signal), testInterruptNotifications(), nil)
	if code != ExitTransaction {
		t.Fatalf("ambiguous run exit = %d", code)
	}
	entries, err := os.ReadDir(filepath.Join(stateHome, "errand", "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ambiguous submit retained %d change states, want 1", len(entries))
	}
	if !strings.Contains(stderr.String(), "the job may have been admitted; handle ") {
		t.Fatalf("ambiguous submit diagnostic = %q", stderr.String())
	}
}

func TestChangeDownloadsForDifferentJobsRunConcurrently(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := changeops.CollectWorkspaceChangesContext(context.Background(), remote, jobDir, proto.Manifest{}, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	jobA, jobB := proto.NewULID(), proto.NewULID()
	enteredA := make(chan struct{})
	releaseA := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseA) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/jobs/"+jobA+"/changes" {
			close(enteredA)
			<-releaseA
		}
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", mw.FormDataContentType())
		if err := writeTestChangeMultipart(mw, bundle, jobDir); err != nil {
			t.Error(err)
		}
	}))
	defer func() {
		release()
		server.Close()
	}()
	for localChangeTransferLockName(localChangeKey(server.URL, jobA)) ==
		localChangeTransferLockName(localChangeKey(server.URL, jobB)) {
		jobB = proto.NewULID()
	}
	summary := proto.ChangeSummary{Paths: bundle.Paths, PathCount: len(bundle.Paths), BundleRoot: bundle.RootHash(), Bytes: bundle.Bytes}
	doneA := make(chan error, 1)
	go func() {
		_, _, err := downloadChangeBundle(server.URL, jobA, summary)
		doneA <- err
	}()
	select {
	case <-enteredA:
	case <-time.After(5 * time.Second):
		t.Fatal("first download did not reach the server")
	}
	doneB := make(chan error, 1)
	go func() {
		_, _, err := downloadChangeBundle(server.URL, jobB, summary)
		doneB <- err
	}()
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		release()
		<-doneA
		t.Fatal("an unrelated change download was serialized behind the first")
	}
	release()
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
}

func TestInterruptCancelsChangeBaselineLockWait(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWorkspaceChangeLock(root, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		done <- runWithDetachNotifications(RunOptions{
			PeerURL: "http://runner.test", Root: root, Argv: []string{"/bin/true"},
			Stdout: io.Discard, Stderr: io.Discard,
		}, sigCh, testInterruptNotifications(), nil)
	}()
	time.Sleep(100 * time.Millisecond)
	sigCh <- os.Interrupt
	select {
	case code := <-done:
		if code != signalExit("interrupt", 2) {
			t.Fatalf("interrupted run exit = %d", code)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-holderDone
		t.Fatal("interrupt did not cancel change baseline lock wait")
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceChangeLockContextCanBeCanceled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withWorkspaceChangeLock(root, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := withWorkspaceChangeLockContext(ctx, root, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("withWorkspaceChangeLockContext() error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
}

func TestLocalChangeLockNamesAreBounded(t *testing.T) {
	names := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		name := localChangeTransferLockName(fmt.Sprintf("job-%d", i))
		names[name] = true
		if strings.Contains(name, fmt.Sprintf("job-%d", i)) {
			t.Fatalf("lock name exposes unbounded key: %q", name)
		}
	}
	if len(names) > localChangeLockStripes {
		t.Fatalf("created %d distinct lock names, want at most %d", len(names), localChangeLockStripes)
	}
}

func TestInterruptedSnapshotNegotiationRemovesUnsubmittedChangeState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/snapshot/diff" {
			http.NotFound(w, r)
			return
		}
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	sigCh := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() {
		done <- runWithDetachNotifications(RunOptions{
			PeerURL: server.URL, Root: root, Argv: []string{"/bin/true"},
			Stdout: io.Discard, Stderr: io.Discard,
		}, sigCh, testInterruptNotifications(), nil)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot negotiation did not start")
	}
	sigCh <- os.Interrupt
	select {
	case code := <-done:
		if code != signalExit("interrupt", 2) {
			t.Fatalf("interrupted run exit = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted run did not return")
	}
	jobs := filepath.Join(stateHome, "errand", "jobs")
	entries, err := os.ReadDir(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsubmitted change state survived: %v", entries)
	}
}

func TestDownloadChangeBundleHasNoTotalRequestDeadline(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := changeops.CollectWorkspaceChangesContext(context.Background(), remote, jobDir, proto.Manifest{}, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	mw := multipart.NewWriter(&payload)
	if err := writeTestChangeMultipart(mw, bundle, jobDir); err != nil {
		t.Fatal(err)
	}

	oldHTTP := maintenanceHTTP
	t.Cleanup(func() { maintenanceHTTP = oldHTTP })
	maintenanceHTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if _, ok := req.Context().Deadline(); ok {
			t.Error("change download has a total request deadline")
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Header: http.Header{"Content-Type": []string{mw.FormDataContentType()}},
			Body:   io.NopCloser(bytes.NewReader(payload.Bytes())), Request: req,
		}, nil
	})}
	summary := proto.ChangeSummary{Paths: bundle.Paths, PathCount: len(bundle.Paths), BundleRoot: bundle.RootHash(), Bytes: bundle.Bytes}
	staged, _, err := downloadChangeBundle("http://runner.test", proto.NewULID(), summary)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(staged, "remote", "artifact")); err != nil || string(got) != "new" {
		t.Fatalf("downloaded artifact = %q, %v", got, err)
	}
}

func TestConsumeChangeArchiveRejectsOversizedTrailingData(t *testing.T) {
	err := consumeChangeArchive(bytes.NewReader([]byte("123456789")), 8, func(reader io.Reader) error {
		var first [1]byte
		_, err := io.ReadFull(reader, first[:])
		return err
	})
	if !errors.Is(err, errChangeResponseTooLarge) {
		t.Fatalf("consumeChangeArchive() error = %v", err)
	}
}

func TestDownloadChangeBundleRefreshesCachedStaging(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := changeops.CollectWorkspaceChangesContext(context.Background(), remote, jobDir, proto.Manifest{}, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", mw.FormDataContentType())
		if err := writeTestChangeMultipart(mw, bundle, jobDir); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	summary := proto.ChangeSummary{Paths: bundle.Paths, PathCount: len(bundle.Paths), BundleRoot: bundle.RootHash(), Bytes: bundle.Bytes}
	jobID := proto.NewULID()
	staged, _, err := downloadChangeBundle(server.URL, jobID, summary)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staged, old, old); err != nil {
		t.Fatal(err)
	}
	refreshedAfter := time.Now().Add(-time.Second)
	if _, _, err := downloadChangeBundle(server.URL, jobID, summary); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Before(refreshedAfter) {
		t.Fatalf("cached staging mtime = %v, want at least %v", info.ModTime(), refreshedAfter)
	}
	if requests != 1 {
		t.Fatalf("cached change made %d requests, want 1", requests)
	}
}

func TestFetchChangesReplacesIncompleteStagingAndBindsReceipt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := changeops.CollectWorkspaceChangesContext(context.Background(), remote, jobDir, proto.Manifest{}, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	jobID := proto.NewULID()
	summary := &proto.ChangeSummary{
		Paths: bundle.Paths, PathCount: len(bundle.Paths), BundleRoot: bundle.RootHash(), Bytes: bundle.Bytes,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/jobs/" + jobID:
			json.NewEncoder(w).Encode(proto.JobStatus{Result: &proto.Result{ChangesOK: true, Changes: summary}})
		case "/v0/jobs/" + jobID + "/changes":
			mw := multipart.NewWriter(w)
			w.Header().Set("Content-Type", mw.FormDataContentType())
			if err := writeTestChangeMultipart(mw, bundle, jobDir); err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "downloads", localChangeKey(server.URL, jobID))
	if err := os.MkdirAll(filepath.Join(dest, "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "remote", "stale"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := FetchChanges(ChangeFetchOptions{PeerURL: server.URL, JobID: jobID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(staged, "remote", "artifact"))
	if err != nil || string(got) != "new" {
		t.Fatalf("repaired staging = %q, %v", got, err)
	}
}

func TestInitializeChangeStateRefusesPendingApplicationWithoutRecovery(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	local, bundle, staged := testChangeApplyFixture(t, "old", "new")
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	owner := localChangeKey(peerURL, jobID)
	transaction := changeops.NewApplyTransaction()
	state := localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: bundle.BaselineRoot, Root: local,
		Pending: transaction,
	}
	if err := saveLocalChangeState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := changeops.Apply(
		staged, local, bundle, nil, owner, transaction, changeops.ApplyOptions{},
	); err != nil {
		t.Fatal(err)
	}
	err := initializeChangeState(
		context.Background(), &RunOptions{Root: local}, proto.NewULID(), testManifestRoot,
	)
	if err == nil || !strings.Contains(err.Error(), "interrupted change application") {
		t.Fatalf("initializeChangeState() error = %v", err)
	}
	recovered, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Pending != transaction || len(recovered.Applied) != 0 {
		t.Fatalf("recovered state = %+v", recovered)
	}
	if _, err := os.Lstat(filepath.Join(local, transaction)); err != nil {
		t.Fatalf("pending transaction was changed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(local, "artifact")); err != nil || string(got) != "new" {
		t.Fatalf("workspace changed during refusal: %q, %v", got, err)
	}
}

func TestRecoverWorkspaceApplicationsContextRefusesCanceledRecovery(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	transaction := changeops.NewApplyTransaction()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot,
		Root: root, Pending: transaction,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recoverWorkspaceApplicationsContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("recoverWorkspaceApplicationsContext() error = %v, want context.Canceled", err)
	}
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending != transaction {
		t.Fatalf("canceled recovery changed pending transaction to %q", state.Pending)
	}
}

func TestWorkspaceChangeLockSerializesCallers(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withWorkspaceChangeLock(root, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	second := make(chan struct{})
	go func() {
		_ = withWorkspaceChangeLock(root, func() error {
			close(second)
			return nil
		})
	}()
	select {
	case <-second:
		t.Fatal("second caller entered while the workspace lock was held")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-second
}

func TestChangeSummaryMatchesAllReceiptFields(t *testing.T) {
	bundle := proto.ChangeBundle{V: changeops.BundleVersion, Paths: []string{"artifact", "report"}, Bytes: 3}
	summary := proto.ChangeSummary{
		Paths: []string{"artifact"}, PathsTruncated: true, PathCount: 2,
		BundleRoot: bundle.RootHash(), Bytes: 3,
	}
	if !summary.Matches(bundle) {
		t.Fatal("matching truncated summary was rejected")
	}
	summary.Bytes++
	if summary.Matches(bundle) {
		t.Fatal("byte-mismatched summary was accepted")
	}
	summary.Bytes--
	summary.Paths = append(summary.Paths, "report", "unexpected")
	if summary.Matches(bundle) {
		t.Fatal("oversized path preview was accepted")
	}
}

func TestChangeGCRetainsPendingTransactionsAndRemovesOldCompletedState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	old := time.Now().Add(-48 * time.Hour)

	completedID := proto.NewULID()
	completed := localChangeState{
		JobID: completedID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace, Terminal: true,
	}
	if err := saveLocalChangeState(completed); err != nil {
		t.Fatal(err)
	}
	pendingID := proto.NewULID()
	pending := localChangeState{
		JobID: pendingID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace,
		Pending: changeops.NewApplyTransaction(),
	}
	if err := saveLocalChangeState(pending); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, pending.Pending), 0o700); err != nil {
		t.Fatal(err)
	}
	stalePendingID := proto.NewULID()
	stalePending := localChangeState{
		JobID: stalePendingID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace,
		Pending: changeops.NewApplyTransaction(),
	}
	if err := saveLocalChangeState(stalePending); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	completedPath := filepath.Join(root, "jobs", localChangeKey(peerURL, completedID)+".json")
	pendingPath := filepath.Join(root, "jobs", localChangeKey(peerURL, pendingID)+".json")
	stalePendingPath := filepath.Join(root, "jobs", localChangeKey(peerURL, stalePendingID)+".json")
	if err := os.Chtimes(completedPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pendingPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stalePendingPath, old, old); err != nil {
		t.Fatal(err)
	}
	activeID := proto.NewULID()
	active := localChangeState{
		JobID: activeID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace,
		SubmissionStarted: true,
	}
	if err := saveLocalChangeState(active); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(root, "jobs", localChangeKey(peerURL, activeID)+".json")
	if err := os.Chtimes(activePath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := ChangeGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 2 || result.Protected != 2 || result.Failed != 0 {
		t.Fatalf("ChangeGC() = %+v", result)
	}
	if _, err := os.Lstat(completedPath); !os.IsNotExist(err) {
		t.Fatalf("completed state survived GC: %v", err)
	}
	if _, err := os.Lstat(pendingPath); err != nil {
		t.Fatalf("pending state was collected: %v", err)
	}
	if _, err := os.Lstat(stalePendingPath); !os.IsNotExist(err) {
		t.Fatalf("journal-free pending state survived GC: %v", err)
	}
	if _, err := os.Lstat(activePath); err != nil {
		t.Fatalf("active-job baseline was collected: %v", err)
	}
}

func TestChangeGCDryRunDoesNotCreateLocksOrTightenPermissions(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	state := localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace, Terminal: true,
	}
	if err := saveLocalChangeState(state); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localChangeKey(peerURL, jobID)+".json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}
	downloads := filepath.Join(root, "downloads")
	if err := os.Mkdir(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(downloads, localChangeKey(peerURL, jobID))
	if err := os.Mkdir(download, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(download, "artifact")
	if err := os.WriteFile(artifact, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(download, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := ChangeGC(24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.Removed != 1 {
		t.Fatalf("dry-run result = %+v", result)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("dry-run removed local change state: %v", err)
	}
	if got, err := os.ReadFile(artifact); err != nil || string(got) != "retained" {
		t.Fatalf("dry-run changed downloaded staging: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "locks")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created a lock directory: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("dry-run changed state directory mode to %04o", info.Mode().Perm())
	}
}

func TestChangeGCDryRunDoesNotWidenRestrictiveDownloadedStaging(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	downloads := filepath.Join(root, "downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	key := localChangeKey("http://runner.test", proto.NewULID())
	download := filepath.Join(downloads, key)
	sealed := filepath.Join(download, "sealed")
	if err := os.MkdirAll(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sealed, "artifact"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sealed, 0o700)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(download, old, old); err != nil {
		t.Fatal(err)
	}

	result, err := ChangeGC(24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.Failed != 1 || result.Removed != 0 {
		t.Fatalf("dry-run result = %+v", result)
	}
	info, err := os.Lstat(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0 {
		t.Fatalf("dry-run widened restrictive staging to %04o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(root, "locks")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created a lock directory: %v", err)
	}
}

func TestChangeStatsAndGCHandleRestrictiveStaging(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	downloads := filepath.Join(root, "downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	key := localChangeKey("http://runner.test", proto.NewULID())
	download := filepath.Join(downloads, key)
	sealed := filepath.Join(download, "remote", "sealed")
	if err := os.MkdirAll(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sealed, "artifact"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sealed, 0o700)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(download, old, old); err != nil {
		t.Fatal(err)
	}

	stats, err := ChangeStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Items != 1 || stats.Bytes == 0 {
		t.Fatalf("ChangeStats() = %+v", stats)
	}
	result, err := ChangeGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Failed != 0 {
		t.Fatalf("ChangeGC() = %+v", result)
	}
	if _, err := os.Lstat(download); !os.IsNotExist(err) {
		t.Fatalf("restrictive staging survived GC: %v", err)
	}
}

func TestChangeStatsWaitsForStagedTreeTransfer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	downloads := filepath.Join(root, "downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	key := localChangeKey("http://runner.test", proto.NewULID())
	download := filepath.Join(downloads, key)
	if err := os.Mkdir(download, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(download, "artifact"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}

	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(key))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ChangeStats()
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("ChangeStats() completed while transfer lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ChangeStats() did not resume after transfer lock was released")
	}
}

func TestSyncExistingLocalDirectoryPropagatesSyncFailure(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := syncExistingLocalDirectory(filepath.Join(blocker, "jobs"))
	if err == nil {
		t.Fatal("syncExistingLocalDirectory() unexpectedly succeeded")
	}
	if err := syncExistingLocalDirectory(filepath.Join(root, "missing")); err != nil {
		t.Fatalf("missing directory: %v", err)
	}
}

func TestChangeGCRetainsPendingStateWhileWorkspaceIsUnavailable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	state := localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace,
		Pending: changeops.NewApplyTransaction(),
	}
	if err := saveLocalChangeState(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, state.Pending), 0o700); err != nil {
		t.Fatal(err)
	}
	offline := workspace + "-offline"
	if err := os.Rename(workspace, offline); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localChangeKey(peerURL, jobID)+".json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}

	result, err := ChangeGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 || result.Protected != 1 || result.Failed != 0 {
		t.Fatalf("ChangeGC() = %+v", result)
	}
	if _, err := os.Lstat(statePath); err != nil {
		t.Fatalf("pending state was collected while its workspace was unavailable: %v", err)
	}
}

func TestChangeGCExpiresPendingStateAfterWorkspaceProtectionWindow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	state := localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace,
		Pending: changeops.NewApplyTransaction(),
	}
	if err := saveLocalChangeState(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(workspace, workspace+"-offline"); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localChangeKey(peerURL, jobID)+".json")
	old := time.Now().Add(-unresolvedChangeStateProtection - time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := ChangeGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Protected != 0 || result.Failed != 0 {
		t.Fatalf("ChangeGC() = %+v", result)
	}
	if _, err := os.Lstat(statePath); !os.IsNotExist(err) {
		t.Fatalf("expired pending state survived GC: %v", err)
	}
}

func TestChangeGCRemovesOldUnsubmittedState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localChangeKey(peerURL, jobID)+".json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := ChangeGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Protected != 0 {
		t.Fatalf("ChangeGC() = %+v", result)
	}
	if _, err := os.Lstat(statePath); !os.IsNotExist(err) {
		t.Fatalf("unsubmitted state survived GC: %v", err)
	}
}

func TestChangeGCRemovesAbandonedSubmittedState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace,
		SubmissionStarted: true,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localChangeKey(peerURL, jobID)+".json")
	old := time.Now().Add(-unresolvedChangeStateProtection - time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := ChangeGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Protected != 0 {
		t.Fatalf("ChangeGC() = %+v", result)
	}
	if _, err := os.Lstat(statePath); !os.IsNotExist(err) {
		t.Fatalf("abandoned submitted state survived GC: %v", err)
	}
}

func TestChangeGCSkipsActiveTransfer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace, Terminal: true,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", localChangeKey(peerURL, jobID)+".json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(statePath, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(localChangeKey(peerURL, jobID)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ChangeGC(24*time.Hour, false)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	if result.Protected != 1 || result.Removed != 0 {
		unlock()
		t.Fatalf("ChangeGC() = %+v", result)
	}
	dry, err := ChangeGC(24*time.Hour, true)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if dry.Protected != 1 || dry.Removed != 0 {
		t.Fatalf("ChangeGC(dry-run) = %+v", dry)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("active transfer state was collected: %v", err)
	}
}

func TestChangeGCRevalidatesDownloadedChangesAfterScan(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace, Terminal: true,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	key := localChangeKey(peerURL, jobID)
	downloadPath := filepath.Join(root, "downloads", key)
	if err := os.MkdirAll(downloadPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloadPath, "bundle.json"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	statePath := filepath.Join(root, "jobs", key+".json")
	for _, path := range []string{statePath, downloadPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	candidates := map[string]*localChangeCandidate{}
	if err := collectChangeGCCandidates(filepath.Join(root, "jobs"), filepath.Join(root, "downloads"), candidates); err != nil {
		t.Fatal(err)
	}
	candidate := candidates[key]
	if candidate == nil {
		t.Fatal("change GC did not discover staged changes")
	}
	if err := os.Chtimes(downloadPath, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	removed, eligible, protected, err := collectLocalChangeCandidate(candidate, time.Now().Add(-24*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if removed || eligible || protected {
		t.Fatalf("refreshed candidate = removed %t, eligible %t, protected %t", removed, eligible, protected)
	}
	for _, path := range []string{statePath, downloadPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("refreshed change path %s was collected: %v", path, err)
		}
	}
}

func TestReconcileCollectedJobChangesCollectsUnobservedTerminalJob(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	jobID := proto.NewULID()
	var collectedCalls, acknowledgementCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/change-reconciliation" {
			if r.URL.Path == "/v0/change-reconciliation/ack" && r.Method == http.MethodPost {
				acknowledgementCalls++
				var request proto.ChangeReconciliationAck
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if !proto.ValidChangeClientID(request.ClientID) || len(request.JobIDs) != 1 || request.JobIDs[0] != jobID {
					t.Errorf("collection acknowledgement = %+v", request)
				}
				json.NewEncoder(w).Encode(proto.ChangeReconciliationAckResult{Acknowledged: 1})
				return
			}
			http.NotFound(w, r)
			return
		}
		if !proto.ValidChangeClientID(r.URL.Query().Get("client_id")) {
			t.Errorf("invalid collection client ID %q", r.URL.Query().Get("client_id"))
		}
		collectedCalls++
		if r.URL.Query().Get("cursor") == "" {
			json.NewEncoder(w).Encode(proto.ChangeReconciliationPage{JobIDs: []string{jobID}, NextCursor: jobID})
			return
		}
		if r.URL.Query().Get("cursor") != jobID {
			t.Errorf("collection cursor = %q, want %q", r.URL.Query().Get("cursor"), jobID)
		}
		json.NewEncoder(w).Encode(proto.ChangeReconciliationPage{})
	}))
	defer server.Close()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: server.URL, ManifestRoot: testManifestRoot, Root: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := localChangeRoot()
	if err != nil {
		t.Fatal(err)
	}
	key := localChangeKey(server.URL, jobID)
	downloadPath := filepath.Join(root, "downloads", key)
	if err := os.MkdirAll(downloadPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloadPath, "bundle.json"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "jobs", key+".json")
	old := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{statePath, downloadPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := ReconcileCollectedJobChanges(server.URL); err != nil {
		t.Fatal(err)
	}
	if collectedCalls != 2 {
		t.Fatalf("collected jobs calls = %d, want 2", collectedCalls)
	}
	if acknowledgementCalls != 1 {
		t.Fatalf("collection acknowledgement calls = %d, want 1", acknowledgementCalls)
	}
	state, err := loadLocalChangeState(server.URL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Terminal {
		t.Fatal("remote removal did not settle the local change state")
	}
	result, err := ChangeGC(24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Selected != 1 || result.Removed != 1 || result.Protected != 0 || result.Failed != 0 {
		t.Fatalf("ChangeGC() after reconciliation = %+v", result)
	}
	for _, path := range []string{statePath, downloadPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("removed remote job retained local change path %s: %v", path, err)
		}
	}
}

func TestMarkLocalChangeTerminalPersistsAfterWorkspaceRemoval(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	workspace := t.TempDir()
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: testManifestRoot, Root: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	if err := markLocalChangeTerminal(peerURL, jobID); err != nil {
		t.Fatal(err)
	}
	state, err := loadLocalChangeState(peerURL, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Terminal {
		t.Fatal("terminal observation was not persisted")
	}
}

func TestRepeatApplyRefusesDestinationChangedAfterFirstApply(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	local, bundle, staged := testChangeApplyFixture(t, "baseline", "remote")
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: bundle.BaselineRoot, Root: local,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyChangeBundle(peerURL, jobID, t.TempDir(), staged, bundle, nil, false); err == nil || !strings.Contains(err.Error(), "within the workspace") {
		t.Fatalf("apply from unrelated workspace error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "baseline" {
		t.Fatalf("unrelated apply changed destination = %q, %v", got, err)
	}
	callerDir := filepath.Join(local, "src", "package")
	if err := os.MkdirAll(callerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := applyChangeBundle(peerURL, jobID, callerDir, staged, bundle, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "artifact"), []byte("user edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyChangeBundle(peerURL, jobID, local, staged, bundle, nil, false); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Fatalf("repeat apply error = %v", err)
	}
	got, err = os.ReadFile(filepath.Join(local, "artifact"))
	if err != nil || string(got) != "user edit" {
		t.Fatalf("repeat apply changed destination = %q, %v", got, err)
	}
}

func TestApplyRefusesWorkspaceReplacedAtSamePath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	remote := t.TempDir()
	jobDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "artifact"), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := changeops.CollectWorkspaceChangesContext(context.Background(), remote, jobDir, proto.Manifest{}, proto.SelectionPolicy{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	container := t.TempDir()
	local := filepath.Join(container, "workspace")
	if err := os.Mkdir(local, 0o700); err != nil {
		t.Fatal(err)
	}
	staged := extractTestChangeBundle(t, jobDir, bundle)
	peerURL := "http://runner.test"
	jobID := proto.NewULID()
	if err := saveLocalChangeState(localChangeState{
		JobID: jobID, PeerURL: peerURL, ManifestRoot: bundle.BaselineRoot, Root: local,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(local, filepath.Join(container, "original-workspace")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(local, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := applyChangeBundle(peerURL, jobID, local, staged, bundle, nil, false); err == nil || !strings.Contains(err.Error(), "is not the workspace") {
		t.Fatalf("apply to replacement workspace error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(local, "artifact")); !os.IsNotExist(err) {
		t.Fatalf("change reached replacement workspace: %v", err)
	}
}
