package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// Allow the runner's 64 MiB manifest plus creation metadata in a descriptor.
const maxWorkspaceResponseBytes = 65 << 20

// Names resolve to the current workspace identity. IDs also work after removal,
// while retained job receipts remain available. Missing names match no jobs.
func ListWorkspace(peerURL, name string, activeOnly bool) ([]proto.JobListEntry, error) {
	id := name
	if !proto.ValidULID(id) {
		w, err := GetWorkspace(peerURL, name)
		if err != nil {
			var response *controlHTTPError
			if errors.As(err, &response) && response.statusCode == http.StatusNotFound {
				return []proto.JobListEntry{}, nil
			}
			return nil, err
		}
		id = w.ID
	}
	return listWorkspaceJobs(peerURL, activeOnly, id)
}

func GetWorkspace(peerURL, name string) (proto.Workspace, error) {
	return getWorkspace(peerURL, name, false)
}

// getWorkspaceDescriptor requests identity and selection metadata without the
// creation manifest. Older runners may ignore the query and return the full row.
func getWorkspaceDescriptor(peerURL, name string) (proto.Workspace, error) {
	return getWorkspace(peerURL, name, true)
}

func getWorkspace(peerURL, name string, omitManifest bool) (proto.Workspace, error) {
	var result proto.Workspace
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	endpoint := strings.TrimSuffix(peerURL, "/") + "/v0/workspaces/" + url.PathEscape(name)
	if omitManifest {
		endpoint += "?manifest=omit"
	}
	err := getJSONContext(ctx, endpoint, maxWorkspaceResponseBytes, "workspace", &result)
	if err == nil && (!proto.ValidULID(result.ID) || proto.ValidateWorkspaceName(result.Name) != nil) {
		err = fmt.Errorf("runner returned an invalid workspace")
	}
	return result, err
}

func ListWorkspaces(peerURL string) ([]proto.WorkspaceSummary, error) {
	var result []proto.WorkspaceSummary
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	err := getJSONContext(ctx, strings.TrimSuffix(peerURL, "/")+"/v0/workspaces", 4<<20, "workspaces", &result)
	return result, err
}

func RemoveWorkspace(peerURL, name string) (proto.WorkspaceRemoval, error) {
	var removal proto.WorkspaceRemoval
	workspace, err := GetWorkspace(peerURL, name)
	if err != nil {
		return removal, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimSuffix(peerURL, "/")+"/v0/workspaces/"+workspace.ID, nil)
	if err != nil {
		return removal, err
	}
	resp, err := directHTTP.Do(req)
	if err != nil {
		return removal, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return removal, &controlHTTPError{statusCode: resp.StatusCode, message: apiError(raw), err: fmt.Errorf("removing workspace: %s: %s", resp.Status, apiError(raw))}
	}
	if err := json.Unmarshal(raw, &removal); err != nil {
		return removal, fmt.Errorf("removing workspace: decoding response: %w", err)
	}
	return removal, nil
}

func CreateWorkspace(opts RunOptions, name string) (proto.Workspace, error) {
	var result proto.Workspace
	if err := proto.ValidateWorkspaceName(name); err != nil {
		return result, err
	}
	prep := prepareSnapshot(opts.Root, opts.IncludeAll, opts.NoSnapshot, opts.Caches...)
	if prep.err != nil {
		return result, fmt.Errorf("%s: %w", prep.stage, prep.err)
	}
	selection := prep.selection
	selection.Artifacts = opts.Artifacts
	selection.Caches = opts.Caches
	request := proto.Workspace{Where: opts.Where, ID: proto.NewULID(), Name: name, Project: opts.Project, Selection: selection}
	if len(opts.Caches) > 0 {
		var err error
		request.CacheProjectID, err = cacheProjectID(opts.Root)
		if err != nil {
			return result, err
		}
	}
	resultWithError := tryCandidates(opts, func(attempt RunOptions) (workspaceCreation, bool) {
		w, err := createPreparedWorkspace(attempt, prep, request)
		return workspaceCreation{w, err}, placementRejection(err)
	})
	return resultWithError.workspace, resultWithError.err
}

type workspaceCreation struct {
	workspace proto.Workspace
	err       error
}

func createPreparedWorkspace(opts RunOptions, prep snapshotPreparation, request proto.Workspace) (proto.Workspace, error) {
	if err := prep.guard.Verify(); err != nil {
		return proto.Workspace{}, err
	}
	endpoint := strings.TrimSuffix(opts.PeerURL, "/") + "/v0/workspaces/" + request.ID + "/snapshot/diff"
	plan, err := negotiateSnapshotAt(context.Background(), endpoint, prep.manifest)
	if err != nil {
		// Like job submission, cache negotiation is optional. A complete
		// upload uses the original endpoint and is safe without capability.
		plan = shipPlan{}
	}
	if err := prep.guard.Verify(); err != nil {
		return proto.Workspace{}, err
	}
	if err := recordWorkspaceOrigin(opts, request.ID, prep.manifest); err != nil {
		return proto.Workspace{}, fmt.Errorf("recording workspace origin: %w", err)
	}
	var result proto.Workspace
	err = uploadWithSnapshotFallback(plan, func(attempt shipPlan) error {
		var err error
		result, err = createPreparedWorkspaceOnce(opts, prep, request, attempt)
		return err
	}, nil)
	return result, err
}

func createPreparedWorkspaceOnce(opts RunOptions, prep snapshotPreparation, request proto.Workspace, plan shipPlan) (proto.Workspace, error) {
	var result proto.Workspace
	if err := prep.guard.Verify(); err != nil {
		return result, err
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			part, err := mw.CreateFormField("metadata")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(request); err != nil {
				return err
			}
			part, err = mw.CreateFormField("manifest")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(prep.manifest); err != nil {
				return err
			}
			part, err = mw.CreateFormFile("workspace", "workspace.tar")
			if err != nil {
				return err
			}
			if err := snapshot.PackPartial(part, opts.Root, prep.manifest, plan.ships); err != nil {
				return err
			}
			if err := prep.guard.Verify(); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), submitRequestTimeout)
	defer cancel()
	endpoint := strings.TrimSuffix(opts.PeerURL, "/") + "/v0/workspaces/" + request.ID
	if plan.partial {
		endpoint += "/snapshot"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pr)
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// Durable reconstruction and admission may outlast the control-request
	// header budget. Keep the existing upload context and uncertainty handling.
	resp, err := maintenanceHTTP.Do(req)
	if err != nil {
		return result, fmt.Errorf("creating workspace (check workspaces before retrying): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var payload proto.APIError
		if resp.StatusCode == http.StatusConflict && json.Unmarshal(raw, &payload) == nil && payload.Code == proto.ErrorCodeSnapshotCacheMiss {
			// The receiver has not published a workspace. Keep the origin and
			// creation ID for the one permitted full-body retry.
			return result, fmt.Errorf("creating workspace: %w: %s", errSnapshotCacheMiss, payload.Error)
		}
		var err error = fmt.Errorf("creating workspace: %s: %s", resp.Status, apiError(raw))
		if resp.StatusCode == http.StatusPreconditionFailed {
			err = &placementRefusal{err}
		}
		// These rejections precede publication. Transport failures, timeouts and
		// server errors remain uncertain and must preserve the frozen origin.
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
			http.StatusConflict, http.StatusPreconditionFailed, http.StatusRequestEntityTooLarge:
			err = errors.Join(err, discardWorkspaceOrigin(opts.PeerURL, request.ID))
		}
		return result, err
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, maxWorkspaceResponseBytes)).Decode(&result)
	return result, err
}

func prepareWorkspaceRun(opts RunOptions) snapshotPreparation {
	if opts.NoSnapshot || opts.IncludeAll {
		return snapshotPreparation{stage: "selecting workspace", err: fmt.Errorf("--workspace cannot be combined with --no-snapshot or --include-all")}
	}
	w, err := GetWorkspace(opts.PeerURL, opts.Workspace)
	if IsNotFound(err) {
		err = fmt.Errorf("%s has no workspace named %s; errand workspaces lists them", peerLabel(opts.PeerName, opts.PeerURL), opts.Workspace)
	}
	if err != nil {
		return snapshotPreparation{stage: "selecting workspace", err: err}
	}
	if opts.Caches != nil && !slices.Equal(opts.Caches, w.Selection.Caches) {
		return snapshotPreparation{stage: "selecting workspace", err: fmt.Errorf("a workspace's cache bindings are fixed when it's created; drop --cache or make a new workspace")}
	}
	return snapshotPreparation{manifest: w.Manifest, selection: w.Selection, workspace: &w}
}
