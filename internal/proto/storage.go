package proto

// StorageDetails contains logical byte counts, not unique allocated disk blocks.
// Shared named caches and job receipts are accounted separately from workspaces.
type StorageDetails struct {
	Workspaces  []WorkspaceStorage  `json:"workspaces"`
	NamedCaches []NamedCacheStorage `json:"named_caches"`
	Jobs        []JobStorage        `json:"jobs"`
}

type WorkspaceStorage struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	JobIDs        []string `json:"job_ids,omitempty"`
	WorkingBytes  int64    `json:"working_bytes"`
	BaseBytes     int64    `json:"base_bytes"`
	TransferBytes int64    `json:"transfer_bytes,omitempty"`
	MetadataBytes int64    `json:"metadata_bytes"`
	Bytes         int64    `json:"bytes"`
}

type NamedCacheStorage struct {
	WorkspaceID string   `json:"workspace_id,omitempty"`
	JobIDs      []string `json:"job_ids,omitempty"`
	Name        string   `json:"name"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id,omitempty"`
	Bytes       int64    `json:"bytes"` // last settled size while a job holds the cache
}

type JobStorage struct {
	ID             string `json:"id"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	Bytes          int64  `json:"bytes"`
	CleanupPending bool   `json:"cleanup_pending,omitempty"`
}
