// Package proto defines the wire and receipt types shared by the errand
// client and daemon. Digests are computed over Go's deterministic JSON
// encoding (struct field order is fixed; map keys are sorted).
package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

const (
	ProtoVersion   = 0
	ReceiptVersion = 1
	// MaxJobListEntries is the bounded per-runner listing window.
	MaxJobListEntries = 200

	// DefaultCapability is the tailnet ACL app-capability key the daemon
	// looks for in a caller's WhoIs capability map.
	DefaultCapability = "lydakis.dev/cap/errand"

	ActionSubmit     = "submit"
	ActionReadOwn    = "read-own"
	ActionKillOwn    = "kill-own"
	ActionForwardOwn = "forward-own"
	ActionCaches     = "manage-caches"
	ActionGCJobs     = "gc-own"

	ErrorCodeSnapshotCacheMiss = "snapshot_cache_miss"

	// ChangeReconciliationWindow bounds how long an unacknowledged runner
	// marker and its unresolved client-side apply state remain protected.
	ChangeReconciliationWindow = 30 * 24 * time.Hour
)

type APIError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// SetupQuiesce is the local setup handshake used to keep an idle runner idle
// while its service definition is replaced and restarted.
type SetupQuiesce struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type SetupQuiesceRelease struct {
	Token string `json:"token"`
}

const (
	EntryFile    = "file"
	EntryDir     = "dir"
	EntrySymlink = "symlink"
)

type ManifestEntry struct {
	Path   string `json:"path"` // slash-separated, relative to workspace root
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"` // symlink target, as recorded
}

// Manifest lists every entry in a workspace snapshot, sorted by Path.
type Manifest struct {
	Entries []ManifestEntry `json:"entries"`
}

func (m Manifest) RootHash() string {
	return digest(m)
}

// SelectionPolicy freezes the ignore rules and explicit artifact paths used
// for retention. Artifacts never expand the input snapshot. Submitted manifest
// paths remain eligible so their modification or deletion is always observable.
type SelectionPolicy struct {
	Artifacts []string `json:"artifacts,omitempty"` // retention only; exact paths override ignores
	Prefix    string   `json:"prefix,omitempty"`
	Ignore    []string `json:"ignore,omitempty"`
	CaseFold  bool     `json:"case_fold,omitempty"`
}

func (p SelectionPolicy) IsZero() bool {
	return p.Prefix == "" && len(p.Ignore) == 0 && !p.CaseFold && len(p.Artifacts) == 0
}

type Limits struct {
	MaxLogBytes       int64 `json:"max_log_bytes"`
	MaxRuntimeSec     int64 `json:"max_runtime_sec"`
	MaxWorkspaceBytes int64 `json:"max_workspace_bytes"`
	MaxChangeBytes    int64 `json:"max_change_bytes"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxLogBytes:       64 << 20,
		MaxRuntimeSec:     2 * 60 * 60,
		MaxWorkspaceBytes: 2 << 30,
		MaxChangeBytes:    2 << 30,
	}
}

// ChangeBundle is the immutable metadata stored beside the submitted and
// completed workspace archives for one job.
type ChangeBundle struct {
	V              int      `json:"v"`
	BaselineRoot   string   `json:"baseline_root"`
	Paths          []string `json:"paths"`
	MetadataPaths  []string `json:"metadata_paths,omitempty"`
	BaseManifest   Manifest `json:"base_manifest"`
	RemoteManifest Manifest `json:"remote_manifest"`
	Bytes          int64    `json:"bytes"`
}

func (b ChangeBundle) RootHash() string { return digest(b) }

type ChangeSummary struct {
	Paths          []string `json:"paths,omitempty"`
	PathsTruncated bool     `json:"paths_truncated,omitempty"`
	PathCount      int      `json:"path_count"`
	BundleRoot     string   `json:"bundle_root"`
	Bytes          int64    `json:"bytes"`
}

func (s ChangeSummary) Matches(bundle ChangeBundle) bool {
	if s.BundleRoot != bundle.RootHash() || s.Bytes != bundle.Bytes || s.PathCount != len(bundle.Paths) ||
		len(s.Paths) > len(bundle.Paths) || s.PathsTruncated != (len(s.Paths) != len(bundle.Paths)) {
		return false
	}
	for i := range s.Paths {
		if s.Paths[i] != bundle.Paths[i] {
			return false
		}
	}
	return true
}

// Spec is the immutable canonical request. Its digest is the admission
// identity: same job ID + same digest is a retry, a different digest is a
// conflict.
type Spec struct {
	Argv           []string          `json:"argv"`
	Env            map[string]string `json:"env,omitempty"`
	EnvSources     map[string]string `json:"env_sources,omitempty"` // name -> literal | passenv
	Workdir        string            `json:"workdir,omitempty"`     // relative, within workspace
	ManifestRoot   string            `json:"manifest_root"`
	Limits         Limits            `json:"limits"`
	GitCommit      string            `json:"git_commit,omitempty"`
	GitDirty       bool              `json:"git_dirty,omitempty"`
	NoSnapshot     bool              `json:"no_snapshot,omitempty"`
	ChangeClientID string            `json:"change_client_id,omitempty"`
	Selection      SelectionPolicy   `json:"selection_policy,omitempty"`
}

func (s Spec) Digest() string {
	return digest(s)
}

// ReceiptSpec is the durable, non-secret view of an admitted request. No value
// derived from the runtime environment is persisted.
type ReceiptSpec struct {
	ReceiptVersion int               `json:"receipt_version"`
	Argv           []string          `json:"argv"`
	EnvNames       []string          `json:"env_names,omitempty"`
	EnvSources     map[string]string `json:"env_sources,omitempty"`
	Workdir        string            `json:"workdir,omitempty"`
	ManifestRoot   string            `json:"manifest_root"`
	Limits         Limits            `json:"limits"`
	GitCommit      string            `json:"git_commit,omitempty"`
	GitDirty       bool              `json:"git_dirty,omitempty"`
	NoSnapshot     bool              `json:"no_snapshot,omitempty"`
	ChangeClientID string            `json:"change_client_id,omitempty"`
	Selection      SelectionPolicy   `json:"selection_policy,omitempty"`
}

func NewReceiptSpec(s Spec) ReceiptSpec {
	namesSet := make(map[string]struct{}, len(s.Env)+len(s.EnvSources))
	for name := range s.Env {
		namesSet[name] = struct{}{}
	}
	for name := range s.EnvSources {
		namesSet[name] = struct{}{}
	}
	names := make([]string, 0, len(namesSet))
	for name := range namesSet {
		names = append(names, name)
	}
	sort.Strings(names)
	return ReceiptSpec{
		ReceiptVersion: ReceiptVersion,
		Argv:           s.Argv, EnvNames: names, EnvSources: s.EnvSources, Workdir: s.Workdir,
		ManifestRoot: s.ManifestRoot, Limits: s.Limits,
		GitCommit: s.GitCommit, GitDirty: s.GitDirty, NoSnapshot: s.NoSnapshot,
		ChangeClientID: s.ChangeClientID, Selection: s.Selection,
	}
}

func (r ReceiptSpec) SpecWithoutEnv() Spec {
	return Spec{
		Argv: r.Argv, EnvSources: r.EnvSources, Workdir: r.Workdir, ManifestRoot: r.ManifestRoot,
		Limits: r.Limits, GitCommit: r.GitCommit, GitDirty: r.GitDirty, NoSnapshot: r.NoSnapshot,
		ChangeClientID: r.ChangeClientID, Selection: r.Selection,
	}
}

// Admission records who was allowed to run a job, why, and the caller-facing
// project label attached to that admission.
type Admission struct {
	Time             time.Time `json:"time"`
	UserID           int64     `json:"user_id,omitempty"`
	UserLogin        string    `json:"user_login,omitempty"`
	NodeID           string    `json:"node_id,omitempty"`
	NodeName         string    `json:"node_name,omitempty"`
	RemoteAddr       string    `json:"remote_addr"`
	Method           string    `json:"method"` // capability | allowlist | local | insecure-test
	LocalUID         int64     `json:"local_uid,omitempty"`
	LocalUser        string    `json:"local_user,omitempty"`
	Project          string    `json:"project,omitempty"`
	ProjectTruncated bool      `json:"project_truncated,omitempty"`
	Facts            Facts     `json:"facts"`
}

// Result is written once at terminal completion. Process result and
// transaction result are separate outcomes.
type Result struct {
	State            string         `json:"state,omitempty"`
	Started          bool           `json:"started"`
	StartedAt        *time.Time     `json:"started_at,omitempty"`
	FinishedAt       *time.Time     `json:"finished_at,omitempty"`
	SettledAt        *time.Time     `json:"settled_at,omitempty"`
	DurationMS       int64          `json:"duration_ms,omitempty"`
	ExitCode         *int           `json:"exit_code"` // nil when signaled or never started
	Signal           string         `json:"signal,omitempty"`
	SignalNum        int            `json:"signal_num,omitempty"`
	StartError       string         `json:"start_error,omitempty"`
	TransactionError string         `json:"transaction_error,omitempty"`
	LimitExceeded    string         `json:"limit_exceeded,omitempty"` // log_bytes | runtime | workspace_bytes | change_bytes | change_entries | change_deadline
	ChangesOK        bool           `json:"changes_ok"`
	Changes          *ChangeSummary `json:"changes,omitempty"`
	CleanupOK        bool           `json:"cleanup_ok"`
	LogsComplete     bool           `json:"logs_complete"`
}

// ExecutionContext records the top-level execution facts controlled by the
// daemon. It is diagnostic evidence, not transitive execution tracing.
type ExecutionContext struct {
	Path string   `json:"path,omitempty"`
	Argv []string `json:"argv"`
}

type LogFrame struct {
	Seq     int64  `json:"seq"`
	Stream  string `json:"stream"` // stdout | stderr
	DataB64 string `json:"data_b64"`
	TUnixMS int64  `json:"t_unix_ms"`
}

type LogStreamError struct {
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

const (
	StateStaging   = "staging"
	StateQueued    = "queued"
	StateRunning   = "running"
	StateExited    = "exited"
	StateKilled    = "killed"
	StateAmbiguous = "ambiguous"
)

// JobStatus is the derived, non-authoritative view served over HTTP.
type JobStatus struct {
	ID     string  `json:"id"`
	State  string  `json:"state"`
	Digest string  `json:"digest,omitempty"`
	Result *Result `json:"result,omitempty"`
}

// JobDetails is the owner-visible status view for one job. Spec is the
// durable non-secret request representation, never the runtime environment.
type JobDetails struct {
	JobStatus
	Spec       ReceiptSpec `json:"spec"`
	AdmittedAt time.Time   `json:"admitted_at"`
	StartedAt  *time.Time  `json:"started_at,omitempty"`
	DurationMS int64       `json:"duration_ms,omitempty"`
	Project    string      `json:"project,omitempty"`
}

// JobListEntry is one row of a runner's job listing: enough to identify,
// time, reproduce, and triage a job without fetching its full receipt.
type JobListEntry struct {
	ID                    string     `json:"id"`
	State                 string     `json:"state"`
	Command               string     `json:"command,omitempty"`
	CommandTruncated      bool       `json:"command_truncated,omitempty"`
	AdmittedAt            time.Time  `json:"admitted_at"`
	StartedAt             *time.Time `json:"started_at,omitempty"`
	FinishedAt            *time.Time `json:"finished_at,omitempty"`
	DurationMS            int64      `json:"duration_ms,omitempty"`
	ManifestRoot          string     `json:"manifest_root,omitempty"`
	ManifestRootTruncated bool       `json:"manifest_root_truncated,omitempty"`
	GitCommit             string     `json:"git_commit,omitempty"`
	GitCommitTruncated    bool       `json:"git_commit_truncated,omitempty"`
	GitDirty              bool       `json:"git_dirty,omitempty"`
	Workdir               string     `json:"workdir,omitempty"`
	WorkdirTruncated      bool       `json:"workdir_truncated,omitempty"`
	Project               string     `json:"project,omitempty"`
	ProjectTruncated      bool       `json:"project_truncated,omitempty"`
	ExitCode              *int       `json:"exit_code,omitempty"`
	Signal                string     `json:"signal,omitempty"`
}

type BlobRef struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type SnapshotDiffRequest struct {
	Blobs []BlobRef `json:"blobs"`
}

type SnapshotDiffResponse struct {
	Missing []string `json:"missing"`
}

type CacheStats struct {
	Blobs    int   `json:"blobs"`
	Bytes    int64 `json:"bytes"`
	MaxBytes int64 `json:"max_bytes"`
	TTLHours int   `json:"ttl_hours"`
}

// StorageCategory is a count and logical byte total for one class of
// Errand-owned state.
type StorageCategory struct {
	Items int   `json:"items"`
	Bytes int64 `json:"bytes"`
}

// StorageStats is the caller-visible storage inventory on one runner. Jobs
// includes only receipts owned by the authenticated caller. Cache is nil when
// the runner's shared snapshot cache is disabled.
type StorageStats struct {
	Cache *CacheStats     `json:"cache,omitempty"`
	Jobs  StorageCategory `json:"jobs"`
}

type CacheGCRequest struct {
	DryRun bool `json:"dry_run,omitempty"`
}

type CacheGCResult struct {
	RemovedBlobs int   `json:"removed_blobs"`
	FreedBytes   int64 `json:"freed_bytes"`
	DryRun       bool  `json:"dry_run"`
}

type JobGCRequest struct {
	OlderThanSeconds *int64 `json:"older_than_seconds,omitempty"`
	Keep             *int   `json:"keep,omitempty"`
	DryRun           bool   `json:"dry_run,omitempty"`
}

type JobGCResult struct {
	SelectedJobs    int   `json:"selected_jobs"`
	RemovedJobs     int   `json:"removed_jobs"`
	ProtectedJobs   int   `json:"protected_jobs"`
	SkippedJobs     int   `json:"skipped_jobs"`
	FailedJobs      int   `json:"failed_jobs"`
	CleanupFailures int   `json:"cleanup_failures"`
	FreedBytes      int64 `json:"freed_bytes"`
	DryRun          bool  `json:"dry_run"`
}

type ChangeReconciliationPage struct {
	JobIDs     []string `json:"job_ids"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type ChangeReconciliationAck struct {
	ClientID string   `json:"client_id"`
	JobIDs   []string `json:"job_ids"`
}

type ChangeReconciliationAckResult struct {
	Acknowledged int `json:"acknowledged"`
}

const ChangeReconciliationPageLimit = 1024

func ValidChangeClientID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

type Facts struct {
	ObservedAt time.Time         `json:"observed_at"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	NumCPU     int               `json:"num_cpu"`
	KVM        bool              `json:"kvm"`
	Tools      map[string]string `json:"tools,omitempty"` // name -> resolved path
}

type Info struct {
	Proto   int    `json:"proto"`
	Version string `json:"version"`
	// Busy means a submission right now would be refused because capacity is
	// full or the runner is temporarily quiesced for setup.
	Busy         bool  `json:"busy"`
	StagingJobs  int   `json:"staging_jobs"`
	StartingJobs int   `json:"starting_jobs"`
	RunningJobs  int   `json:"running_jobs"`
	QueuedJobs   int   `json:"queued_jobs"`
	MaxJobs      int   `json:"max_jobs"`
	MaxQueued    int   `json:"max_queued"`
	Facts        Facts `json:"facts"`
}

type Event struct {
	TUnixMS int64  `json:"t_unix_ms"`
	Event   string `json:"event"`
	Detail  string `json:"detail,omitempty"`
}

func digest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// All digested types marshal without error by construction.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
