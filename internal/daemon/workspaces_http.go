package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lydakis/errand/internal/archive"
	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) handleWorkspaceCreate(w http.ResponseWriter, r *http.Request, id Identity) {
	var request proto.Workspace
	key := r.PathValue("id")
	if !proto.ValidULID(key) {
		httpError(w, 400, "workspace id must be a ULID")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, d.cfg.MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, 400, "expected multipart workspace upload")
		return
	}
	if err := readJSONPart(mr, "metadata", maxSpecBytes, &request); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := proto.ValidateWorkspaceName(request.Name); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	d.workspaces.mu.Lock()
	_, existingErr := d.workspaces.lookup(d.workspaceOwner(id), request.Name)
	d.workspaces.mu.Unlock()
	if existingErr == nil {
		httpError(w, 409, "workspace name already exists")
		return
	}
	if !os.IsNotExist(existingErr) {
		httpError(w, 500, existingErr.Error())
		return
	}
	if request.ID != key || request.JobID != "" || !request.CreatedAt.IsZero() {
		httpError(w, 400, "invalid workspace creation metadata")
		return
	}
	if _, err := pathpolicy.Compile(request.Selection); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if len(request.Selection.Caches) > 0 && (!proto.ValidChangeClientID(request.CacheProjectID) || d.cfg.NamedCacheDisabled) {
		httpError(w, 400, "named caches require a project identity and enabled runner cache")
		return
	}
	if len(request.Project) > maxListProjectBytes {
		httpError(w, 400, "workspace project name is too long")
		return
	}
	if err := readJSONPart(mr, "manifest", maxManifestBytes, &request.Manifest); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := archive.Validate(request.Manifest); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	for _, entry := range request.Manifest.Entries {
		if pathpolicy.InCache(entry.Path, request.Selection.Caches) {
			httpError(w, 400, "snapshot contains a named cache path")
			return
		}
	}
	input, err := nextPart(mr, "workspace")
	if err != nil {
		httpError(w, 400, err.Error())
		return
	}
	s := d.workspaces
	// Extract outside the metadata lock; concurrent creates are checked again
	// before publication. No command can run in a partially uploaded tree.
	dir, err := os.MkdirTemp(s.dir, ".create-")
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer removeOwnedTree(dir)
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if err := archive.Extract(input, data, request.Manifest, d.cfg.MaxLimits.MaxWorkspaceBytes); err != nil {
		httpError(w, 400, err.Error())
		return
	}
	if err := changeops.CaptureWorkspaceBaseContext(r.Context(), data, dir, request.Manifest); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	identity, _, err := fsidentity.Lstat(data)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	request.CreatedAt = time.Now().UTC()
	record := workspaceRecord{Workspace: request, Owner: d.workspaceOwner(id), Identity: identity}
	if err := s.publish(dir, record); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errWorkspaceExists) {
			code = http.StatusConflict
		}
		httpError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, request)
}

func (d *Daemon) handleWorkspaceList(w http.ResponseWriter, r *http.Request, id Identity) {
	s := d.workspaces
	s.mu.Lock()
	rows, err := s.records()
	s.mu.Unlock()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	result := []proto.WorkspaceSummary{}
	for _, row := range rows {
		if row.Owner != d.workspaceOwner(id) {
			continue
		}
		result = append(result, proto.WorkspaceSummary{ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt, Project: row.Project, JobID: row.JobID})
	}
	writeJSON(w, 200, result)
}

func (d *Daemon) handleWorkspaceGet(w http.ResponseWriter, r *http.Request, id Identity) {
	s := d.workspaces
	s.mu.Lock()
	row, err := s.lookup(d.workspaceOwner(id), r.PathValue("id"))
	s.mu.Unlock()
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	writeJSON(w, 200, row.Workspace)
}

func (d *Daemon) handleWorkspaceRemove(w http.ResponseWriter, r *http.Request, id Identity) {
	s := d.workspaces
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if !proto.ValidULID(r.PathValue("id")) {
		httpError(w, 400, "removal requires a workspace id")
		return
	}
	row, err := s.lookup(d.workspaceOwner(id), r.PathValue("id"))
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	if row.JobID != "" {
		httpError(w, 409, fmt.Sprintf("%s: %s", errWorkspaceBusy, row.JobID))
		return
	}
	if err := workspaceDataIdentity(filepath.Join(s.dir, row.ID, "data"), row.Identity); err != nil {
		httpError(w, 409, err.Error())
		return
	}
	tombstone := ".removed-" + row.ID
	if err := s.root.Rename(row.ID, tombstone); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	if err := syncDirectory(s.dir); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.mu.Unlock()
	locked = false
	if err := removeOwnedTree(filepath.Join(s.dir, tombstone)); err != nil {
		httpError(w, 500, fmt.Sprintf("workspace removed; storage cleanup pending: %v", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func workspaceHTTPError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		httpError(w, 404, "workspace does not exist")
		return
	}
	httpError(w, 500, err.Error())
}
