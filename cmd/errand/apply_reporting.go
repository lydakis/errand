package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

// Local-only rows carry identity and apply evidence, not fabricated timestamps
// or receipt fields. Successful remote rows keep the existing JSON shape.
func (row psRow) MarshalJSON() ([]byte, error) {
	if row.applyNote == "" {
		type plain psRow
		return json.Marshal(plain(row))
	}
	return json.Marshal(struct {
		Peer           string                       `json:"peer"`
		ID             string                       `json:"id"`
		State          string                       `json:"state"`
		WorkspaceID    string                       `json:"workspace_id,omitempty"`
		AutomaticApply *client.AutomaticApplyStatus `json:"automatic_apply,omitempty"`
	}{Peer: row.Peer, ID: row.ID, State: row.State, WorkspaceID: row.WorkspaceID, AutomaticApply: row.AutomaticApply})
}

func applyNeedsAttention(status *client.AutomaticApplyStatus) bool {
	return status != nil && status.NeedsAttention()
}

func applyRecoveryHint(handle string, status *client.AutomaticApplyStatus) string {
	if !applyNeedsAttention(status) {
		return ""
	}
	prefix := ""
	if status.State == "failed" {
		prefix = "Address the reported apply error first. "
	}
	return prefix + "Once the job has finished, recover from its originating workspace: errand fetch --apply " + handle
}

func writeApplyRecoveryHint(w io.Writer, handle string, status *client.AutomaticApplyStatus) {
	if hint := applyRecoveryHint(handle, status); hint != "" {
		fmt.Fprintln(w, terminalSafeField(hint))
	}
}

func doctorApplyChecks() []doctorCheck {
	issues, err := client.InterruptedAutomaticApplies()
	var checks []doctorCheck
	if err != nil {
		checks = append(checks, doctorCheck{Name: "automatic_apply", Status: "warning", Detail: "Cannot inspect local apply state: " + err.Error(), Hint: "Inspect local client state permissions and integrity; no recovery was attempted."})
	}
	// Resolving configured names is local-only. Raw HTTP handles remain valid;
	// synthetic SSH identities require the original configured transport.
	targets, _, _ := peerTargets("", "")
	for _, issue := range issues {
		peer := issue.PeerURL
		matched := false
		for _, target := range targets {
			if target.url == issue.PeerURL {
				peer, matched = target.name, true
				break
			}
		}
		handle := peer + "/" + issue.JobID
		hint := applyRecoveryHint(handle, &issue.Status) + " (" + issue.Root + ")."
		if !matched && strings.HasPrefix(peer, "ssh://peer-") {
			hint = "Restore this job's original SSH peer configuration before recovering with errand fetch --apply from " + issue.Root + "."
		}
		checks = append(checks, doctorCheck{Name: "automatic_apply", Status: "warning", Detail: handle + ": " + formatAutomaticApply(issue.Status), Hint: hint})
	}
	return checks
}

// Read remote and local jobs in one pass per selected peer. Local records are
// already inspected once for the invocation; each peer resolves its filter once.
func psPeerRows(peer, workspace string, activeOnly bool, local []client.AutomaticApplyInspection) ([]psRow, error) {
	workspaceID := workspace
	if workspace != "" && !proto.ValidULID(workspace) {
		ws, err := client.GetWorkspace(peer, workspace)
		if client.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		workspaceID = ws.ID
	}
	records := make(map[string]client.AutomaticApplyInspection)
	for _, record := range local {
		if workspaceID == "" || record.WorkspaceID == workspaceID {
			records[record.JobID] = record
		}
	}
	var entries []proto.JobListEntry
	var err error
	if workspaceID != "" {
		entries, err = client.ListWorkspace(peer, workspaceID, activeOnly)
	} else if activeOnly {
		entries, err = client.ListActive(peer)
	} else {
		entries, err = client.List(peer)
	}
	if err != nil {
		// Decoding can leave partial entries. Only local recovery records are
		// trustworthy when the remote listing could not be read and validated.
		entries = nil
	}
	var rows []psRow
	for _, entry := range entries {
		row := psRow{JobListEntry: entry}
		if record, ok := records[entry.ID]; ok {
			row.AutomaticApply = &record.Status
			delete(records, entry.ID)
		}
		rows = append(rows, row)
	}
	var missing []string
	for id, record := range records {
		if record.Status.NeedsAttention() {
			missing = append(missing, id)
		}
	}
	var details []client.JobDetailResult
	if err == nil {
		details = client.InspectJobDetails(peer, missing)
	}
	var detailErr error
	for i, id := range missing {
		record := records[id]
		row := psRow{JobListEntry: proto.JobListEntry{ID: id, WorkspaceID: record.WorkspaceID, State: "unknown"}, AutomaticApply: &record.Status}
		row.applyNote = "Runner unavailable; inspect again when it is reachable."
		if err == nil {
			result := details[i]
			switch {
			case result.Err == nil:
				d := result.Details
				row.JobListEntry = proto.JobListEntry{ID: id, WorkspaceID: d.Spec.WorkspaceID, State: d.State,
					AdmittedAt: d.AdmittedAt, StartedAt: d.StartedAt, DurationMS: d.DurationMS,
					Command: quoteArgv(d.Spec.Argv), Project: d.Project, Workdir: d.Spec.Workdir,
					ManifestRoot: d.Spec.ManifestRoot, GitCommit: d.Spec.GitCommit, GitDirty: d.Spec.GitDirty}
				if d.Result != nil {
					row.ExitCode, row.FinishedAt, row.Signal = d.Result.ExitCode, d.Result.FinishedAt, d.Result.Signal
				}
				row.applyNote = ""
			case client.IsNotFound(result.Err):
				row.applyNote = "Job receipt is no longer retained on this runner; automatic recovery is unavailable."
			default:
				row.applyNote = "Job details unavailable; inspect again when the runner is reachable."
				if detailErr == nil {
					detailErr = result.Err
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, errors.Join(err, detailErr)
}
