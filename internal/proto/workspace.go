package proto

import (
	"fmt"
	"regexp"
	"time"
)

var workspaceNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func ValidateWorkspaceName(name string) error {
	if ValidULID(name) {
		return fmt.Errorf("workspace name must not look like a workspace ID")
	}
	if !workspaceNamePattern.MatchString(name) {
		return fmt.Errorf("workspace name must start with a letter and contain at most 64 letters, digits, hyphens, or underscores")
	}
	return nil
}

// Workspace identifies an explicitly created persistent working tree. Manifest
// and Selection describe its immutable creation snapshot, not its live files.
type Workspace struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	CreatedAt      time.Time       `json:"created_at"`
	Project        string          `json:"project,omitempty"`
	JobID          string          `json:"job_id,omitempty"`
	Manifest       Manifest        `json:"manifest"`
	Selection      SelectionPolicy `json:"selection_policy,omitempty"`
	CacheProjectID string          `json:"cache_project_id,omitempty"`
}

type WorkspaceSummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Project   string    `json:"project,omitempty"`
	JobID     string    `json:"job_id,omitempty"`
}
