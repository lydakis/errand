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
	"os"
	"path/filepath"
	"strings"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

type PushOptions struct {
	PeerURL, Workspace, Root, Path          string
	Apply, MaterializeConflicts, IncludeAll bool
	Stats                                   *TransferStats
	meter                                   *transferMeter
}
type pendingPush struct {
	Request  proto.PushRequest      `json:"request"`
	Staged   *proto.PushResult      `json:"staged,omitempty"`
	Applying bool                   `json:"applying,omitempty"`
	Apply    proto.PushApplyRequest `json:"apply"`
}

func PushChanges(opts PushOptions) (proto.PushResult, error) {
	opts.meter = startTransfer(opts.Stats)
	defer opts.meter.finish()
	var result proto.PushResult
	if opts.Workspace == "" {
		return result, fmt.Errorf("--workspace is required")
	}
	if opts.MaterializeConflicts && !opts.Apply {
		return result, fmt.Errorf("--conflicts requires --apply")
	}
	ws, err := GetWorkspace(opts.PeerURL, opts.Workspace)
	if err != nil {
		return result, err
	}
	result.WorkspaceID = ws.ID
	dir, err := workspaceTransferDir(opts.PeerURL, ws.ID)
	if err != nil {
		return result, err
	}
	origin, err := readWorkspaceOrigin(dir)
	if err != nil {
		return result, fmt.Errorf("push requires this workspace's originating checkout: %w", err)
	}
	if err := validateApplyCallerWorkspace(origin.Root, opts.Root); err != nil {
		return result, err
	}
	err = withWorkspaceChangeLock(origin.Root, func() error {
		if err := validateLocalWorkspaceIdentity(localChangeState{Root: origin.Root, RootID: origin.RootID}); err != nil {
			return err
		}
		if err := recoverWorkspaceApplications(origin.Root); err != nil {
			return err
		}
		unlock, err := lockWorkspaceTransfer(dir)
		if err != nil {
			return err
		}
		defer unlock()
		return pushChangesLocked(opts, ws, origin, dir, &result)
	})
	return result, err
}
func pushChangesLocked(opts PushOptions, ws proto.Workspace, origin workspaceOrigin, dir string, result *proto.PushResult) error {
	pendingPath := filepath.Join(dir, "push.json")
	var pending pendingPush
	raw, err := os.ReadFile(pendingPath)
	if err == nil {
		if err := json.Unmarshal(raw, &pending); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if pending.Applying {
		// Complete the exact interrupted request before accepting another one. The
		// remote receipt makes a lost response safe to retry despite later job edits.
		err := finishPush(opts.PeerURL, ws.ID, dir, pending, result, opts.meter)
		result.Recovered = true
		return err
	}
	prep := prepareSnapshot(origin.Root, opts.IncludeAll, false, ws.Selection.Caches...)
	if prep.err != nil {
		return fmt.Errorf("%s: %w", prep.stage, prep.err)
	}
	policy := prep.selection
	policy.Artifacts = ws.Selection.Artifacts
	policy.Caches = ws.Selection.Caches
	if (proto.Spec{Selection: policy}).Digest() != (proto.Spec{Selection: ws.Selection}).Digest() {
		return fmt.Errorf("push selection policy differs from workspace creation; create a new workspace for the new policy")
	}
	if pending.Request.ID == "" || pending.Request.Manifest.RootHash() != prep.manifest.RootHash() {
		clientID, err := localChangeClientID()
		if err != nil {
			return err
		}
		pending = pendingPush{Request: proto.PushRequest{ID: proto.NewULID(), ClientID: clientID, Manifest: prep.manifest}}
		parent := filepath.Join(dir, "push-sources")
		if err := ensurePrivateLocalDirectory(parent); err != nil {
			return err
		}
		source := filepath.Join(parent, pending.Request.ID)
		if err := changeops.CopyTransferSource(context.Background(), origin.Root, source, prep.manifest, proto.DefaultLimits().MaxWorkspaceBytes); err != nil {
			changeops.RemoveTree(source)
			return err
		}
		if err := prep.guard.Verify(); err != nil {
			changeops.RemoveTree(source)
			return err
		}
		if err := replaceTransferJSON(pendingPath, pending); err != nil {
			return err
		}
	}
	var response proto.PushResult
	if opts.Apply && pending.Staged != nil {
		response = *pending.Staged
	} else {
		response, err = uploadPush(opts.PeerURL, ws.ID, filepath.Join(dir, "push-sources", pending.Request.ID), pending.Request, opts.meter)
		if err != nil {
			return err
		}
		pending.Staged = &response
		if err := replaceTransferJSON(pendingPath, pending); err != nil {
			return err
		}
	}
	*result = response
	opts.meter.paths(response.Paths, nil)
	if _, err := changeops.SelectTransferPaths(proto.ChangeBundle{Paths: response.Paths}, opts.Path); err != nil {
		return err
	}
	if !opts.Apply {
		return nil
	}
	pending.Applying = true
	pending.Apply = proto.PushApplyRequest{Path: opts.Path, Conflicts: opts.MaterializeConflicts}
	if err := replaceTransferJSON(pendingPath, pending); err != nil {
		return err
	}
	return finishPush(opts.PeerURL, ws.ID, dir, pending, result, opts.meter)
}
func uploadPush(peer, workspace, source string, request proto.PushRequest, meter *transferMeter) (proto.PushResult, error) {
	var result proto.PushResult
	pr, pw := io.Pipe()
	defer pr.Close()
	mw := multipart.NewWriter(pw)
	ctx, cancel := context.WithTimeout(context.Background(), submitRequestTimeout)
	defer cancel()
	go func() {
		err := func() error {
			part, err := mw.CreateFormField("metadata")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(request); err != nil {
				return err
			}
			part, err = mw.CreateFormFile("workspace", "workspace.tar")
			if err != nil {
				return err
			}
			if err := snapshot.PackContext(ctx, part, source, request.Manifest); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(peer, "/")+"/v0/workspaces/"+workspace+"/push", meter.readCloser(pr))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := directHTTP.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	raw, err := readBoundedBody(resp.Body, changeops.MaxBundleMetadataBytes, "push response")
	if err != nil {
		return result, err
	}
	if resp.StatusCode != 201 {
		return result, fmt.Errorf("staging push: %s: %s", resp.Status, apiError(raw))
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if result.ID != request.ID || result.WorkspaceID != workspace {
		return result, fmt.Errorf("push response identifies another transfer")
	}
	return result, nil
}

var errPushStageMissing = errors.New("push stage is missing")

func finishPush(peer, workspace, dir string, pending pendingPush, result *proto.PushResult, meter *transferMeter) error {
	defer func() { meter.paths(result.Paths, nil) }()
	err := finishPushOnce(peer, workspace, dir, pending, result)
	if !errors.Is(err, errPushStageMissing) {
		return err
	}
	// Only the daemon's explicit missing-stage response permits re-staging.
	// Generic 404s and damaged attempts do not establish that apply never ran.
	_, err = uploadPush(peer, workspace, filepath.Join(dir, "push-sources", pending.Request.ID), pending.Request, meter)
	if err == nil {
		err = finishPushOnce(peer, workspace, dir, pending, result)
	}
	var conflict *changeops.MergeConflictError
	if errors.As(err, &conflict) {
		return err
	}
	if err != nil {
		return fmt.Errorf("push outcome unknown; repeat push to recover: %w", err)
	}
	return nil
}

func finishPushOnce(peer, workspace, dir string, pending pendingPush, result *proto.PushResult) error {
	ctx, cancel := context.WithTimeout(context.Background(), maintenanceTimeout)
	defer cancel()
	raw, err := json.Marshal(pending.Apply)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(peer, "/") + "/v0/workspaces/" + workspace + "/push/" + pending.Request.ID + "/apply?client=" + pending.Request.ClientID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := maintenanceHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("push outcome unknown; repeat push to recover: %w", err)
	}
	defer resp.Body.Close()
	raw, err = readBoundedBody(resp.Body, changeops.MaxBundleMetadataBytes, "push response")
	if err != nil {
		return fmt.Errorf("push outcome unknown; repeat push to recover: %w", err)
	}
	var apiErr proto.APIError
	if resp.StatusCode == http.StatusNotFound && json.Unmarshal(raw, &apiErr) == nil && apiErr.Code == proto.ErrorCodePushStageMissing {
		return errPushStageMissing
	}
	var receipt proto.PushResult
	if json.Unmarshal(raw, &receipt) != nil || receipt.ID != pending.Request.ID || receipt.WorkspaceID != workspace ||
		(resp.StatusCode != http.StatusOK && !(resp.StatusCode == http.StatusConflict && len(receipt.Conflicts) > 0)) {
		return fmt.Errorf("push outcome unknown; repeat push to recover: applying push: %s: %s", resp.Status, apiError(raw))
	}
	*result = receipt
	if err := os.Remove(filepath.Join(dir, "push.json")); err != nil {
		return err
	}
	if err := syncLocalDirectory(dir); err != nil {
		return err
	}
	if err := changeops.RemoveTree(filepath.Join(dir, "push-sources", pending.Request.ID)); err != nil {
		return err
	}
	if len(result.Conflicts) > 0 {
		return &changeops.MergeConflictError{Paths: result.Conflicts, Materialized: result.Materialized}
	}
	return nil
}
func replaceTransferJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".record-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncLocalDirectory(filepath.Dir(path))
}
