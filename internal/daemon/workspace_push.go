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
	"strings"

	"github.com/lydakis/errand/internal/archive"
	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) pushSession(row workspaceRecord, clientID string) changeops.TransferSession {
	return changeops.TransferSession{Directory: filepath.Join(d.workspaces.dir, row.ID, "push", clientID), Root: filepath.Join(d.workspaces.dir, row.ID, "data"), RootID: row.Identity, Owner: row.Owner, SourceID: clientID, MaxBytes: d.cfg.MaxLimits.MaxChangeBytes}
}
func (d *Daemon) pushWorkspace(r *http.Request, id Identity) (workspaceRecord, error) {
	if !proto.ValidULID(r.PathValue("id")) {
		return workspaceRecord{}, os.ErrNotExist
	}
	d.workspaces.mu.Lock()
	defer d.workspaces.mu.Unlock()
	row, err := d.workspaces.lookup(d.workspaceOwner(id), r.PathValue("id"))
	if err != nil {
		return row, err
	}
	return row, workspaceDataIdentity(filepath.Join(d.workspaces.dir, row.ID, "data"), row.Identity)
}
func (d *Daemon) handleWorkspacePush(w http.ResponseWriter, r *http.Request, id Identity) {
	unlock := d.workspaces.lockWorkspace(r.PathValue("id"))
	defer unlock()
	row, err := d.pushWorkspace(r, id)
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
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
	if err := archive.Validate(request.Manifest); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	for _, e := range request.Manifest.Entries {
		if pathpolicy.InCache(e.Path, row.Selection.Caches) {
			httpError(w, 400, "push contains a named cache path")
			return
		}
	}
	if err := d.recoverWorkspacePushes(row); err != nil {
		httpError(w, 409, err.Error())
		return
	}
	session := d.pushSession(row, request.ClientID)
	if err := session.Initialize(r.Context(), filepath.Join(d.workspaces.dir, row.ID, "change-base"), row.Manifest); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	source, err := os.MkdirTemp(session.Directory, ".upload-")
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer changeops.RemoveTree(source)
	part, err := nextPart(mr, "workspace")
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := archive.Extract(part, source, request.Manifest, d.cfg.MaxLimits.MaxWorkspaceBytes); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if _, err := mr.NextPart(); err != io.EOF {
		httpError(w, 400, "unexpected push payload")
		return
	}
	if err := changeops.SyncTransferSource(source, request.Manifest); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	_, bundle, err := session.Stage(r.Context(), request.ID, source, request.Manifest)
	if err != nil {
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

// Startup cleans interrupted uploads; their contents are never published stages.
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
		entries, err := os.ReadDir(filepath.Join(d.workspaces.dir, row.ID, "push"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			dir := d.pushSession(row, e.Name()).Directory
			children, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, c := range children {
				if strings.HasPrefix(c.Name(), ".upload-") {
					if err := changeops.RemoveTree(filepath.Join(dir, c.Name())); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
