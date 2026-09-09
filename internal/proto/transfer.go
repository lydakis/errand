package proto

const ErrorCodePushStageMissing = "push_stage_missing"

// PushRequest identifies an immutable source snapshot. Replaying its ID retries
// the same transfer; applying a staged transfer uses that ID and no new upload.
type PushRequest struct {
	ID       string   `json:"id"`
	ClientID string   `json:"client_id"`
	Manifest Manifest `json:"manifest"`
}
type PushApplyRequest struct {
	Path      string `json:"path,omitempty"`
	Conflicts bool   `json:"conflicts,omitempty"`
}
type PushResult struct {
	ID           string   `json:"id"`
	WorkspaceID  string   `json:"workspace_id"`
	Paths        []string `json:"paths"`
	Conflicts    []string `json:"conflicts,omitempty"`
	Materialized bool     `json:"materialized,omitempty"`
	// Recovered is set by the client when completing an earlier uncertain apply.
	Recovered bool `json:"recovered,omitempty"`
}

type TransferGCRequest struct {
	OlderThanSeconds int64 `json:"older_than_seconds"`
	DryRun           bool  `json:"dry_run"`
}
