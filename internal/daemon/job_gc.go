package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

type jobGCRecord struct {
	job       *Job
	settledAt time.Time
}

type jobRemovalOutcome uint8

const (
	jobRemovalSkipped jobRemovalOutcome = iota
	jobRemovalProtected
	jobRemovalRemoved
)

type collectedRecord struct {
	Owner          string    `json:"owner,omitempty"`
	RequestDigest  string    `json:"-"`
	CollectedAt    time.Time `json:"collected_at"`
	OutputsPending bool      `json:"outputs_pending,omitempty"`
	OutputClientID string    `json:"output_client_id,omitempty"`
}

func (d *Daemon) loadCollected() error {
	if err := ensureChildDirectoryDurable(d.collectedDir(), 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(d.collectedDir())
	if err != nil {
		return err
	}
	tombstones, err := d.gcTombstoneIDs()
	if err != nil {
		return fmt.Errorf("scanning collection tombstones: %w", err)
	}
	now := d.admissionNow(time.Now())
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		id := name[:len(name)-len(".json")]
		if !proto.ValidULID(id) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d.collectedDir(), name))
		if err != nil {
			return err
		}
		var record collectedRecord
		if err := json.Unmarshal(raw, &record); err != nil || record.CollectedAt.IsZero() {
			return fmt.Errorf("invalid collection marker %s", name)
		}
		if collectedMarkerExpired(record, now) {
			if tombstones[id] {
				d.collected[id] = record
				continue
			}
			if err := os.Remove(filepath.Join(d.collectedDir(), name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing expired collection marker %s: %w", name, err)
			}
			continue
		}
		d.collected[id] = record
	}
	return nil
}

func collectedMarkerExpired(record collectedRecord, now time.Time) bool {
	if record.OutputsPending {
		return !now.Before(record.CollectedAt.Add(pendingOutputMarkerTTL))
	}
	return !now.Before(record.CollectedAt.Add(collectedMarkerTTL))
}

func (d *Daemon) pruneCollectedID(jobID string, now time.Time, tombstonePending bool) error {
	d.mu.Lock()
	record, ok := d.collected[jobID]
	d.mu.Unlock()
	if !ok || !collectedMarkerExpired(record, now) {
		return nil
	}
	if tombstonePending {
		return nil
	}
	if err := os.Remove(filepath.Join(d.collectedDir(), jobID+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	d.mu.Lock()
	if current, ok := d.collected[jobID]; ok && current == record {
		delete(d.collected, jobID)
	}
	d.mu.Unlock()
	return nil
}

func (d *Daemon) pruneCollected(ctx context.Context, now time.Time) error {
	tombstones, err := d.gcTombstoneIDs()
	if err != nil {
		return err
	}
	d.mu.Lock()
	ids := make([]string, 0, len(d.collected))
	for id, record := range d.collected {
		if collectedMarkerExpired(record, now) {
			ids = append(ids, id)
		}
	}
	d.mu.Unlock()
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.pruneCollectedID(id, now, tombstones[id]); err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) handleJobGC(w http.ResponseWriter, r *http.Request, id Identity) {
	var req proto.JobGCRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "decoding job GC policy: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		httpError(w, http.StatusBadRequest, "job GC policy must contain one JSON object")
		return
	}
	if req.OlderThanSeconds == nil && req.Keep == nil {
		httpError(w, http.StatusBadRequest, "job GC requires older_than_seconds or keep")
		return
	}
	if req.OlderThanSeconds != nil && *req.OlderThanSeconds <= 0 {
		httpError(w, http.StatusBadRequest, "older_than_seconds must be positive")
		return
	}
	if req.OlderThanSeconds != nil && *req.OlderThanSeconds > int64((1<<63-1)/int64(time.Second)) {
		httpError(w, http.StatusBadRequest, "older_than_seconds is too large")
		return
	}
	if req.Keep != nil && *req.Keep < 0 {
		httpError(w, http.StatusBadRequest, "keep must not be negative")
		return
	}
	markerNow, err := d.advanceAdmissionClock(time.Now())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "persisting admission clock: "+err.Error())
		return
	}
	result := proto.JobGCResult{DryRun: req.DryRun}
	if !req.DryRun {
		failures, err := d.cleanupOwnedGCTombstones(r.Context(), id, removeOwnedTree)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			httpError(w, http.StatusInternalServerError, "scanning interrupted job GC tombstones: "+err.Error())
			return
		}
		result.CleanupFailures = failures
	}
	if err := d.pruneCollected(r.Context(), markerNow); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		httpError(w, http.StatusInternalServerError, "pruning collection markers: "+err.Error())
		return
	}

	d.mu.Lock()
	owned := make([]*Job, 0, len(d.jobs))
	for _, j := range d.jobs {
		if d.ownsJob(id, j) {
			owned = append(owned, j)
		}
	}
	d.mu.Unlock()

	records := make([]jobGCRecord, 0, len(owned))
	for _, j := range owned {
		if err := r.Context().Err(); err != nil {
			return
		}
		j.mu.Lock()
		settledAt, ok := gcEligibleLocked(j)
		j.mu.Unlock()
		if !ok {
			result.ProtectedJobs++
			continue
		}
		records = append(records, jobGCRecord{job: j, settledAt: settledAt})
	}

	sort.Slice(records, func(i, k int) bool {
		if records[i].settledAt.Equal(records[k].settledAt) {
			return records[i].job.ID > records[k].job.ID
		}
		return records[i].settledAt.After(records[k].settledAt)
	})
	keep := 0
	if req.Keep != nil {
		keep = *req.Keep
	}
	cutoff := time.Time{}
	if req.OlderThanSeconds != nil {
		cutoff = time.Now().Add(-time.Duration(*req.OlderThanSeconds) * time.Second)
	}
	for index, record := range records {
		if err := r.Context().Err(); err != nil {
			return
		}
		if req.Keep != nil && index < keep {
			continue
		}
		if !cutoff.IsZero() && !record.settledAt.Before(cutoff) {
			continue
		}
		result.SelectedJobs++
		size, err := treeBytes(r.Context(), record.job.Dir)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			if outcome, raced := d.jobRemovalRace(record.job); raced {
				if outcome == jobRemovalProtected {
					result.ProtectedJobs++
				} else {
					result.SkippedJobs++
				}
				continue
			}
			result.FailedJobs++
			continue
		}
		if req.DryRun {
			result.FreedBytes += size
			continue
		}
		if err := r.Context().Err(); err != nil {
			return
		}
		outcome, cleanupErr, err := d.removeJobReceipt(record.job)
		if err != nil {
			result.FailedJobs++
			continue
		}
		switch outcome {
		case jobRemovalRemoved:
			result.RemovedJobs++
			if cleanupErr != nil {
				result.CleanupFailures++
			} else {
				result.FreedBytes += size
			}
		case jobRemovalProtected:
			result.ProtectedJobs++
		case jobRemovalSkipped:
			result.SkippedJobs++
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (d *Daemon) handleCollectedJobs(w http.ResponseWriter, r *http.Request, id Identity) {
	cursor := r.URL.Query().Get("cursor")
	if cursor != "" && !proto.ValidULID(cursor) {
		httpError(w, http.StatusBadRequest, "invalid collection cursor")
		return
	}
	clientID := r.URL.Query().Get("client_id")
	if !proto.ValidOutputClientID(clientID) {
		httpError(w, http.StatusBadRequest, "invalid output client ID")
		return
	}
	owner := id.Owner()
	d.mu.Lock()
	ids := make([]string, 0, len(d.collected))
	for jobID, record := range d.collected {
		if record.OutputsPending && record.OutputClientID == clientID &&
			(d.cfg.InsecureNoAuth || (owner != "" && record.Owner == owner)) {
			ids = append(ids, jobID)
		}
	}
	d.mu.Unlock()
	sort.Strings(ids)
	start := sort.Search(len(ids), func(i int) bool { return ids[i] > cursor })
	end := min(start+proto.CollectedJobsPageLimit, len(ids))
	page := proto.CollectedJobsPage{JobIDs: append([]string(nil), ids[start:end]...)}
	if end < len(ids) {
		page.NextCursor = ids[end-1]
	}
	writeJSON(w, http.StatusOK, page)
}

func (d *Daemon) handleCollectedJobsAck(w http.ResponseWriter, r *http.Request, id Identity) {
	var request proto.CollectedJobsAck
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		httpError(w, http.StatusBadRequest, "decoding collection acknowledgement: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		httpError(w, http.StatusBadRequest, "collection acknowledgement must contain one JSON object")
		return
	}
	if !proto.ValidOutputClientID(request.ClientID) {
		httpError(w, http.StatusBadRequest, "invalid output client ID")
		return
	}
	if len(request.JobIDs) == 0 || len(request.JobIDs) > proto.CollectedJobsPageLimit {
		httpError(w, http.StatusBadRequest, "collection acknowledgement has an invalid job count")
		return
	}
	seen := make(map[string]struct{}, len(request.JobIDs))
	result := proto.CollectedJobsAckResult{}
	owner := id.Owner()
	for _, jobID := range request.JobIDs {
		if !proto.ValidULID(jobID) {
			httpError(w, http.StatusBadRequest, "collection acknowledgement contains an invalid job ID")
			return
		}
		if _, ok := seen[jobID]; ok {
			continue
		}
		seen[jobID] = struct{}{}
		if err := r.Context().Err(); err != nil {
			return
		}
		d.mu.Lock()
		record, ok := d.collected[jobID]
		owned := d.cfg.InsecureNoAuth || (owner != "" && record.Owner == owner)
		if !ok || !owned || !record.OutputsPending || record.OutputClientID != request.ClientID {
			d.mu.Unlock()
			continue
		}
		record.OutputsPending = false
		record.OutputClientID = ""
		if err := replaceJSONDurable(filepath.Join(d.collectedDir(), jobID+".json"), record); err != nil {
			d.mu.Unlock()
			httpError(w, http.StatusInternalServerError, "persisting collection acknowledgement: "+err.Error())
			return
		}
		d.collected[jobID] = record
		d.mu.Unlock()
		result.Acknowledged++
	}
	if err := d.pruneCollected(r.Context(), d.admissionNow(time.Now())); err != nil {
		if r.Context().Err() != nil {
			return
		}
		httpError(w, http.StatusInternalServerError, "pruning acknowledged collection markers: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func gcEligibleLocked(j *Job) (time.Time, bool) {
	if j.deleting || j.logReaders != 0 || j.outputReaders != 0 || j.result == nil ||
		(j.state != proto.StateExited && j.state != proto.StateKilled) ||
		!j.result.CleanupOK || !j.result.OutputsOK || !j.result.LogsComplete ||
		j.result.TransactionError != "" {
		return time.Time{}, false
	}
	if j.result.SettledAt == nil || j.result.SettledAt.IsZero() {
		return time.Time{}, false
	}
	return *j.result.SettledAt, true
}

func (d *Daemon) jobRemovalRace(j *Job) (jobRemovalOutcome, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if d.jobs[j.ID] != j {
		return jobRemovalSkipped, true
	}
	if _, ok := gcEligibleLocked(j); !ok {
		return jobRemovalProtected, true
	}
	return jobRemovalSkipped, false
}

func (d *Daemon) removeJobReceipt(j *Job) (jobRemovalOutcome, error, error) {
	tombstone := filepath.Join(d.jobsDir(), ".gc-"+j.ID+"-"+proto.NewULID())
	d.mu.Lock()
	j.mu.Lock()
	if d.jobs[j.ID] != j {
		j.mu.Unlock()
		d.mu.Unlock()
		return jobRemovalSkipped, nil, nil
	}
	if _, ok := gcEligibleLocked(j); !ok {
		j.mu.Unlock()
		d.mu.Unlock()
		return jobRemovalProtected, nil, nil
	}
	record := collectedRecord{
		Owner: admissionOwner(j.Admission), RequestDigest: j.RequestDigest,
		CollectedAt: d.admissionNow(time.Now()), OutputsPending: len(j.Spec.Outputs) > 0,
		OutputClientID: j.Spec.OutputClientID,
	}
	marker := filepath.Join(d.collectedDir(), j.ID+".json")
	if err := replaceJSONDurable(marker, record); err != nil {
		j.mu.Unlock()
		d.mu.Unlock()
		return jobRemovalSkipped, nil, err
	}
	d.collected[j.ID] = record
	j.deleting = true
	if err := os.Rename(j.Dir, tombstone); err != nil {
		j.deleting = false
		delete(d.collected, j.ID)
		_ = os.Remove(marker)
		j.mu.Unlock()
		d.mu.Unlock()
		return jobRemovalSkipped, nil, err
	}
	delete(d.jobs, j.ID)
	j.mu.Unlock()
	d.mu.Unlock()
	if err := removeOwnedTree(tombstone); err != nil {
		return jobRemovalRemoved, err, nil
	}
	return jobRemovalRemoved, nil, nil
}

func cleanupGCTombstones(ctx context.Context, jobsDir string, remove func(string) error) (int, error) {
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return 0, err
	}
	failures := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return failures, err
		}
		if !entry.IsDir() || len(entry.Name()) < len(".gc-") || entry.Name()[:len(".gc-")] != ".gc-" {
			continue
		}
		if err := remove(filepath.Join(jobsDir, entry.Name())); err != nil {
			failures++
		}
	}
	return failures, nil
}

func (d *Daemon) cleanupOwnedGCTombstones(
	ctx context.Context,
	id Identity,
	remove func(string) error,
) (int, error) {
	entries, err := os.ReadDir(d.jobsDir())
	if err != nil {
		return 0, err
	}
	failures := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return failures, err
		}
		if !entry.IsDir() {
			continue
		}
		jobID, ok := gcTombstoneJobID(entry.Name())
		if !ok || !d.ownsCollectedJob(id, jobID) {
			continue
		}
		if err := remove(filepath.Join(d.jobsDir(), entry.Name())); err != nil {
			failures++
		}
	}
	return failures, nil
}

func gcTombstoneJobID(name string) (string, bool) {
	const prefix = ".gc-"
	if len(name) < len(prefix)+26+1 || name[:len(prefix)] != prefix {
		return "", false
	}
	jobID := name[len(prefix) : len(prefix)+26]
	if name[len(prefix)+26] != '-' || !proto.ValidULID(jobID) {
		return "", false
	}
	return jobID, true
}

func (d *Daemon) gcTombstoneIDs() (map[string]bool, error) {
	ids := make(map[string]bool)
	entries, err := os.ReadDir(d.jobsDir())
	if os.IsNotExist(err) {
		return ids, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		jobID, ok := gcTombstoneJobID(entry.Name())
		if ok {
			ids[jobID] = true
		}
	}
	return ids, nil
}

func (d *Daemon) ownsCollectedJob(id Identity, jobID string) bool {
	d.mu.Lock()
	record, ok := d.collected[jobID]
	d.mu.Unlock()
	if !ok {
		return false
	}
	if d.cfg.InsecureNoAuth {
		return true
	}
	owner := id.Owner()
	return owner != "" && record.Owner == owner
}

func treeBytes(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}
