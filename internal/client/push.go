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
	manifeststate "github.com/lydakis/errand/internal/manifest"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

type PushOptions struct {
	PeerURL, Workspace, Root, Path          string
	Apply, MaterializeConflicts, IncludeAll bool
	Stats                                   *TransferStats
	meter                                   *transferMeter
	workspace                               *proto.Workspace // pinned for the lifetime of a watch
	origin                                  *workspaceOrigin
	watchState                              *pushWatchState
	retryCheckpoint                         bool
	refreshSource                           bool
}

type pushWatchState struct {
	manifest    string
	base        *manifeststate.Snapshot
	baseChecked bool
	generation  string
	builder     snapshot.Builder
	watcher     *snapshot.Watch
}

var errWatchUnchanged = errors.New("watched source is unchanged")

type pushSourceError struct{ error }

func (e *pushSourceError) Unwrap() error { return e.error }

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
	var ws proto.Workspace
	var err error
	if opts.workspace != nil {
		ws = *opts.workspace
	} else {
		ws, err = getWorkspaceDescriptor(opts.PeerURL, opts.Workspace)
	}
	if err != nil {
		return result, err
	}
	result.WorkspaceID = ws.ID
	dir, err := workspaceTransferDir(opts.PeerURL, ws.ID)
	if err != nil {
		return result, err
	}
	var origin workspaceOrigin
	if opts.origin != nil {
		origin = *opts.origin
	} else {
		origin, err = readWorkspaceOrigin(dir)
	}
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
	if opts.watchState != nil {
		if err := opts.watchState.observeGeneration(dir); err != nil {
			return err
		}
	}
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
		if opts.watchState != nil {
			opts.watchState.manifest = ""
			opts.watchState.base = nil
			opts.watchState.baseChecked = false
		}
		return err
	}
	var builder *snapshot.Builder
	if opts.watchState != nil {
		builder = &opts.watchState.builder
	}
	var prep snapshotPreparation
	var sourceState *manifeststate.Snapshot
	var preparedDelta *changeops.SnapshotDelta
	if opts.watchState != nil && opts.watchState.watcher != nil {
		sourceState, prep.gitInfo, prep.selection, prep.guard, prep.err = opts.watchState.watcher.PrepareSnapshot(builder)
		prep.stage = "preparing watched source"
	} else {
		prep = prepareSnapshotWithBuilder(origin.Root, opts.IncludeAll, ws.Selection.Caches, builder)
	}
	if prep.err != nil {
		return &pushSourceError{fmt.Errorf("%s: %w", prep.stage, prep.err)}
	}
	policy := prep.selection
	policy.Artifacts = ws.Selection.Artifacts
	policy.Caches = ws.Selection.Caches
	if (proto.Spec{Selection: policy}).Digest() != (proto.Spec{Selection: ws.Selection}).Digest() {
		return fmt.Errorf("push selection policy differs from workspace creation; create a new workspace for the new policy")
	}
	if sourceState == nil {
		sourceState, err = manifeststate.New(context.Background(), prep.manifest)
		if err != nil {
			return err
		}
	}
	manifestHash, err := sourceState.RootHash(context.Background())
	if err != nil {
		return err
	}
	if opts.watchState != nil && opts.watchState.manifest == manifestHash {
		return errWatchUnchanged
	}
	if opts.refreshSource || pending.Request.ID == "" || pushManifestRoot(pending.Request) != manifestHash {
		clientID, err := localChangeClientID()
		if err != nil {
			return err
		}
		pending = pendingPush{Request: proto.PushRequest{ID: proto.NewULID(), ClientID: clientID}}
		{
			// One-shot pushes negotiate a fresh accepted-source checkpoint. Watch
			// may reuse its accepted checkpoint between successful batches.
			state := opts.watchState
			if state == nil {
				state = &pushWatchState{}
			}
			if !state.baseChecked {
				base, err := pushBase(opts.PeerURL, ws.ID, clientID)
				if err != nil {
					return err
				}
				if base != nil {
					state.base, err = manifeststate.New(context.Background(), *base)
					if err != nil {
						return err
					}
					if opts.watchState != nil {
						if err := state.base.PrepareUpdates(context.Background()); err != nil {
							return err
						}
					}
				}
				state.baseChecked = true
			}
			base := state.base
			if base != nil {
				plan, err := changeops.PrepareSnapshotDelta(context.Background(), base, sourceState, proto.DefaultLimits().MaxChangeBytes)
				if err != nil {
					return err
				}
				preparedDelta = plan
				delta := plan.Bundle()
				pending.Request.Delta = &delta
				pending.Request.SourceRoot = manifestHash
			}
		}
		// Delta recovery uses its frozen changed bodies and SourceRoot. Keeping
		// the full inventory here would encode and fsync it on every state write.
		if pending.Request.Delta == nil {
			pending.Request.Manifest, err = sourceState.Manifest(context.Background())
			if err != nil {
				return err
			}
		}
		parent := filepath.Join(dir, "push-sources")
		if err := ensurePrivateLocalDirectory(parent); err != nil {
			return err
		}
		source := filepath.Join(parent, pending.Request.ID)
		if err := changeops.CopyTransferSource(context.Background(), origin.Root, source, pushSourceManifest(pending.Request), proto.DefaultLimits().MaxWorkspaceBytes); err != nil {
			changeops.RemoveTree(source)
			return &pushSourceError{err}
		}
		if err := prep.guard.Verify(); err != nil {
			changeops.RemoveTree(source)
			return &pushSourceError{err}
		}
		if err := replaceTransferJSON(pendingPath, pending); err != nil {
			return err
		}
		// A watch can supersede thousands of staged snapshots. The durable
		// pending record now owns the new source; no older source can be retried.
		if err := prunePushSources(parent, pending.Request.ID); err != nil {
			return err
		}
	}
	var response proto.PushResult
	if opts.Apply && pending.Staged != nil {
		response = *pending.Staged
	} else {
		response, err = uploadPush(opts.PeerURL, ws.ID, filepath.Join(dir, "push-sources", pending.Request.ID), pending.Request, opts.meter)
		if err != nil {
			// This typed rejection is emitted before the receiver stages anything.
			// Never replace an uncertain apply or retry an arbitrary HTTP 409.
			if errors.Is(err, errPushCheckpointChanged) && !opts.retryCheckpoint {
				opts.retryCheckpoint, opts.refreshSource = true, true
				if opts.watchState != nil {
					opts.watchState.base = nil
					opts.watchState.baseChecked = false
					opts.watchState.manifest = ""
				}
				return pushChangesLocked(opts, ws, origin, dir, result)
			}
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
		if opts.watchState != nil && errors.Is(err, changeops.ErrNoTransferChanges) {
			opts.watchState.manifest = manifestHash
		}
		return err
	}
	if !opts.Apply {
		if opts.watchState != nil {
			opts.watchState.manifest = manifestHash
		}
		return nil
	}
	pending.Applying = true
	pending.Apply = proto.PushApplyRequest{Path: opts.Path, Conflicts: opts.MaterializeConflicts}
	if err := replaceTransferJSON(pendingPath, pending); err != nil {
		return err
	}
	err = finishPush(opts.PeerURL, ws.ID, dir, pending, result, opts.meter)
	if err == nil && opts.watchState != nil {
		opts.watchState.generation = pending.Request.ID
		if opts.watchState.base != nil && pending.Request.Delta != nil {
			var base *manifeststate.Snapshot
			var err error
			if preparedDelta != nil {
				base, err = preparedDelta.Accepted(context.Background(), result.Paths)
			} else {
				base, err = changeops.AcceptedSnapshotDelta(context.Background(), opts.watchState.base, *pending.Request.Delta, result.Paths)
			}
			if err != nil {
				return err
			}
			opts.watchState.base = base
		}
		opts.watchState.manifest = manifestHash
	}
	return err
}

func prunePushSources(parent, keep string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != keep && proto.ValidULID(e.Name()) {
			if err := changeops.RemoveTree(filepath.Join(parent, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
func uploadPush(peer, workspace, source string, request proto.PushRequest, meter *transferMeter) (proto.PushResult, error) {
	// A small delta costs less to send directly than another network round trip
	// to discover whether its bodies are cached. Larger deltas still negotiate.
	if request.Delta != nil && smallPushDelta(request.Delta.RemoteManifest) {
		return uploadPushOnce(peer, workspace, source, request, meter, shipPlan{})
	}
	endpoint := strings.TrimSuffix(peer, "/") + "/v0/workspaces/" + workspace + "/push/diff"
	plan, err := negotiateSnapshotAt(context.Background(), endpoint, pushSourceManifest(request))
	if err != nil {
		return proto.PushResult{}, err
	}
	var result proto.PushResult
	err = uploadWithSnapshotFallback(plan, func(attempt shipPlan) error {
		var err error
		result, err = uploadPushOnce(peer, workspace, source, request, meter, attempt)
		return err
	}, nil)
	return result, err
}

func uploadPushOnce(peer, workspace, source string, request proto.PushRequest, meter *transferMeter, plan shipPlan) (proto.PushResult, error) {
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
			wire := request
			if wire.Delta != nil {
				wire.Manifest = proto.Manifest{}
			}
			if err := json.NewEncoder(part).Encode(wire); err != nil {
				return err
			}
			part, err = mw.CreateFormFile("workspace", "workspace.tar")
			if err != nil {
				return err
			}
			if err := snapshot.PackPartialContext(ctx, part, source, pushSourceManifest(request), plan.ships); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	endpoint := strings.TrimSuffix(peer, "/") + "/v0/workspaces/" + workspace + "/push"
	if request.Delta != nil {
		// Old decoders ignore unknown JSON fields, so capability negotiation
		// alone cannot protect a long-running client from a daemon rollback.
		endpoint += "/delta-v1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, meter.readCloser(pr))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// Reconstruction and durable staging happen after the upload is read.
	// Their budget must not shrink to the short control-request timeout.
	resp, err := maintenanceHTTP.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	raw, err := readBoundedBody(resp.Body, changeops.MaxBundleMetadataBytes, "push response")
	if err != nil {
		return result, err
	}
	if resp.StatusCode != 201 {
		var apiErr proto.APIError
		if resp.StatusCode == http.StatusConflict && json.Unmarshal(raw, &apiErr) == nil && apiErr.Code == proto.ErrorCodePushCheckpointChanged {
			return result, errPushCheckpointChanged
		}
		if resp.StatusCode == http.StatusConflict && json.Unmarshal(raw, &apiErr) == nil && apiErr.Code == proto.ErrorCodeSnapshotCacheMiss {
			return result, fmt.Errorf("staging push: %w: %s", errSnapshotCacheMiss, apiErr.Error)
		}
		return result, &controlHTTPError{statusCode: resp.StatusCode, err: fmt.Errorf("staging push: %s: %s", resp.Status, apiError(raw))}
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if result.ID != request.ID || result.WorkspaceID != workspace {
		return result, fmt.Errorf("push response identifies another transfer")
	}
	return result, nil
}

var errPushCheckpointChanged = errors.New("push checkpoint changed before staging")

var errPushStageMissing = errors.New("push stage is missing")

func finishPush(peer, workspace, dir string, pending pendingPush, result *proto.PushResult, meter *transferMeter) error {
	if err := writePushGeneration(dir, pending.Request.ID); err != nil {
		return err
	}
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
		return &controlHTTPError{statusCode: resp.StatusCode, err: fmt.Errorf("push outcome unknown; repeat push to recover: applying push: %s: %s", resp.Status, apiError(raw))}
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
