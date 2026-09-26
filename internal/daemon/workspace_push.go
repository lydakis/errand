package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/archive"
	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) pushSession(row workspaceRecord, clientID string) *changeops.TransferSession {
	return &changeops.TransferSession{Reuse: d.workspaces.checkpointCache, Directory: filepath.Join(d.workspaces.dir, row.ID, "push", clientID), Root: filepath.Join(d.workspaces.dir, row.ID, "data"), RootID: row.Identity, Owner: row.Owner, SourceID: clientID, MaxSourceBytes: d.cfg.MaxLimits.MaxWorkspaceBytes, MaxChangeBytes: d.cfg.MaxLimits.MaxChangeBytes}
}
func (d *Daemon) pushWorkspace(r *http.Request, id Identity) (workspaceRecord, error) {
	d.workspaces.mu.Lock()
	defer d.workspaces.mu.Unlock()
	return d.pushWorkspaceLocked(r, id)
}

// pinPushWorkspace revalidates an admitted upload's workspace and opens its
// data directory before removal can run. Removal may unlink it mid-upload, but
// the open handle keeps its inode allocated, so a workspace later published
// under the same ID cannot present row.Identity to the upload's final
// revalidation. Pushes waiting for admission hold no handle.
func (d *Daemon) pinPushWorkspace(r *http.Request, id Identity, admitted workspaceRecord) (workspaceRecord, *os.File, error) {
	d.workspaces.mu.Lock()
	defer d.workspaces.mu.Unlock()
	row, err := d.pushWorkspaceLocked(r, id)
	if err != nil {
		return row, nil, err
	}
	if row.Identity != admitted.Identity {
		return row, nil, os.ErrNotExist
	}
	pin, err := openWorkspaceData(filepath.Join(d.workspaces.dir, row.ID, "data"), row.Identity)
	return row, pin, err
}

// Callers hold the inventory mutex.
func (d *Daemon) pushWorkspaceLocked(r *http.Request, id Identity) (workspaceRecord, error) {
	if !proto.ValidULID(r.PathValue("id")) {
		return workspaceRecord{}, os.ErrNotExist
	}
	row, err := d.workspaces.lookup(d.workspaceOwner(id), r.PathValue("id"))
	if err != nil {
		return row, err
	}
	return row, workspaceDataIdentity(filepath.Join(d.workspaces.dir, row.ID, "data"), row.Identity)
}
func (d *Daemon) handleWorkspacePush(w http.ResponseWriter, r *http.Request, id Identity) {
	row, err := d.pushWorkspace(r, id)
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	upload, err := d.workspaces.beginUpload(r.Context(), row)
	if err != nil {
		httpError(w, 409, err.Error())
		return
	}
	defer func() {
		if err := d.workspaces.finishUpload(upload); err != nil {
			log.Printf("workspace %s upload cleanup: %v", row.ID, err)
		}
	}()
	row, pin, err := d.pinPushWorkspace(r, id, row)
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	defer pin.Close()
	source := upload.dir
	r.Body = http.MaxBytesReader(w, r.Body, d.cfg.MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, 400, "expected multipart push")
		return
	}
	var request proto.PushRequest
	if err := readJSONPart(mr, "metadata", maxManifestBytes+maxSpecBytes, &request); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if !proto.ValidULID(request.ID) || !proto.ValidChangeClientID(request.ClientID) {
		httpError(w, 400, "invalid push identity")
		return
	}
	var prepared changeops.PreparedTransferSource
	if request.Delta != nil {
		unlock, err := d.workspaces.lockWorkspaceContext(r.Context(), row.ID)
		if err != nil {
			return
		}
		base, err := d.pushBase(row, request.ClientID)
		unlock()
		// Expansion uses immutable checkpoint metadata. Keep full-tree work
		// outside the apply gate; StagePrepared rechecks the baseline under it.
		if err == nil {
			prepared, err = changeops.ExpandTransferSourceBase(r.Context(), base, *request.Delta, request.SourceRoot, d.cfg.MaxLimits.MaxChangeBytes)
			if err == nil {
				request.Manifest = prepared.Manifest()
			}
		}
		if err != nil {
			if errors.Is(err, changeops.ErrCheckpointChanged) {
				httpErrorCode(w, http.StatusConflict, proto.ErrorCodePushCheckpointChanged, err.Error())
			} else {
				httpError(w, 409, err.Error())
			}
			return
		}
	}
	if request.Delta == nil {
		if err := archive.Validate(request.Manifest); err != nil {
			httpError(w, 400, err.Error())
			return
		}
	}
	var total int64
	for _, e := range request.Manifest.Entries {
		if e.Type == proto.EntryFile {
			if e.Size > d.cfg.MaxLimits.MaxWorkspaceBytes-total {
				httpError(w, 400, "workspace source exceeds byte limit")
				return
			}
			total += e.Size
		}
		if pathpolicy.InCache(e.Path, row.Selection.Caches) {
			httpError(w, 400, "push contains a named cache path")
			return
		}
	}
	sourceManifest := request.Manifest
	if request.Delta != nil {
		sourceManifest = request.Delta.RemoteManifest
	}
	part, err := nextPart(mr, "workspace")
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	extractOpts, restored := d.snapshotExtractOptions(r.Context())
	if err := archive.ExtractWith(&contextReader{ctx: r.Context(), r: part}, source, sourceManifest, d.cfg.MaxLimits.MaxWorkspaceBytes, extractOpts); err != nil {
		if errors.Is(err, archive.ErrCacheMiss) {
			httpErrorCode(w, http.StatusConflict, proto.ErrorCodeSnapshotCacheMiss, err.Error())
			return
		}
		httpError(w, 400, err.Error())
		return
	}
	if _, err := mr.NextPart(); err != io.EOF {
		httpError(w, 400, "unexpected push payload")
		return
	}
	if err := changeops.SyncTransferSource(source, sourceManifest); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	d.cacheWorkspaceSource(r.Context(), source, sourceManifest, restored)
	unlock, err := d.workspaces.lockWorkspaceContext(r.Context(), row.ID)
	if err != nil {
		return
	}
	defer unlock()
	// Revalidate ownership and directory identity after network I/O. The name
	// may have been removed and recreated; this upload still addresses its ID.
	// The pin keeps row.Identity unique even if the ID was reused meanwhile.
	current, err := d.pushWorkspace(r, id)
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	if current.Identity != row.Identity {
		httpError(w, http.StatusConflict, "workspace changed during upload")
		return
	}
	row = current
	if err := d.recoverWorkspacePushes(row); err != nil {
		httpError(w, 409, err.Error())
		return
	}
	session := d.pushSession(row, request.ClientID)
	if err := session.InitializeBase(r.Context(), filepath.Join(d.workspaces.dir, row.ID, "change-base"), row.creation); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	var bundle proto.ChangeBundle
	if request.Delta != nil {
		_, bundle, err = session.StagePrepared(r.Context(), request.ID, source, prepared)
	} else {
		_, bundle, err = session.Stage(r.Context(), request.ID, source, request.Manifest)
	}
	if err != nil {
		if errors.Is(err, changeops.ErrCheckpointChanged) {
			httpErrorCode(w, http.StatusConflict, proto.ErrorCodePushCheckpointChanged, err.Error())
			return
		}
		httpError(w, 409, err.Error())
		return
	}
	writeJSON(w, 201, proto.PushResult{ID: request.ID, WorkspaceID: row.ID, Paths: bundle.Paths})
}
func (d *Daemon) handleWorkspacePushApply(w http.ResponseWriter, r *http.Request, id Identity) {
	unlock := d.workspaces.lockWorkspace(r.PathValue("id"))
	defer unlock()
	row, err := d.pushWorkspace(r, id)
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	clientID := r.URL.Query().Get("client")
	if !proto.ValidChangeClientID(clientID) || !proto.ValidULID(r.PathValue("transfer")) {
		httpError(w, 400, "invalid push identity")
		return
	}
	var request proto.PushApplyRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxSpecBytes)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	session := d.pushSession(row, clientID)
	if err := d.recoverWorkspacePushes(row); err != nil {
		httpError(w, 409, err.Error())
		return
	}
	attemptDir := filepath.Join(session.Directory, "attempts", r.PathValue("transfer"))
	if _, err := os.Stat(attemptDir); os.IsNotExist(err) {
		httpErrorCode(w, http.StatusNotFound, proto.ErrorCodePushStageMissing, "push stage was collected; upload the same snapshot again")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	bundle, err := changeops.ReadTransferBundle(attemptDir)
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	selected, err := changeops.SelectTransferPaths(bundle, request.Path)
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	result, err := session.Apply(r.PathValue("transfer"), selected, request.Conflicts)
	response := proto.PushResult{ID: r.PathValue("transfer"), WorkspaceID: row.ID, Paths: result.Applied, Conflicts: result.Conflicts}
	var conflict *changeops.MergeConflictError
	if errors.As(err, &conflict) {
		response.Conflicts = conflict.Paths
		response.Materialized = conflict.Materialized
		writeJSON(w, 409, response)
		return
	}
	if err != nil {
		httpError(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, response)
}

// Called under the workspace control gate before transfers and new admissions.
// Existing jobs remain caller-managed writers, including during an apply.
func (d *Daemon) recoverWorkspacePushes(row workspaceRecord) error {
	entries, err := os.ReadDir(filepath.Join(d.workspaces.dir, row.ID, "push"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !proto.ValidChangeClientID(e.Name()) {
			return fmt.Errorf("invalid workspace transfer directory")
		}
		if err := d.pushSession(row, e.Name()).Recover(); err != nil {
			return err
		}
	}
	return nil
}

// Startup recovers published transfers. openWorkspaces removes upload debris.
func (d *Daemon) recoverAllWorkspacePushes(ctx context.Context) error {
	rows, err := d.workspaces.records()
	if err != nil {
		log.Printf("workspace transfer inventory incomplete: %v", err)
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.recoverWorkspacePushes(row); err != nil {
			log.Printf("workspace %s transfer recovery protected: %v", row.ID, err)
			continue
		}
	}
	return nil
}
