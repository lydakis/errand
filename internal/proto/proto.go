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

	// DefaultCapability is the tailnet ACL app-capability key the daemon
	// looks for in a caller's WhoIs capability map.
	DefaultCapability = "lydakis.dev/cap/errand"

	ActionSubmit  = "submit"
	ActionReadOwn = "read-own"
	ActionKillOwn = "kill-own"
	ActionCaches  = "manage-caches"
	ActionGCJobs  = "gc-own"

	ErrorCodeSnapshotCacheMiss = "snapshot_cache_miss"
)

type APIError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
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

type Limits struct {
	MaxLogBytes       int64 `json:"max_log_bytes"`
	MaxRuntimeSec     int64 `json:"max_runtime_sec"`
	MaxWorkspaceBytes int64 `json:"max_workspace_bytes"`
}

func DefaultLimits() Limits {
	return Limits{
		MaxLogBytes:       64 << 20,
		MaxRuntimeSec:     2 * 60 * 60,
		MaxWorkspaceBytes: 2 << 30,
	}
}

// Spec is the immutable canonical request. Its digest is the admission
// identity: same job ID + same digest is a retry, a different digest is a
// conflict.
type Spec struct {
	V            int               `json:"v"`
	Argv         []string          `json:"argv"`
	Env          map[string]string `json:"env,omitempty"`
	EnvSources   map[string]string `json:"env_sources,omitempty"` // name -> literal | passenv
	Workdir      string            `json:"workdir,omitempty"`     // relative, within workspace
	ManifestRoot string            `json:"manifest_root"`
	Limits       Limits            `json:"limits"`
	GitCommit    string            `json:"git_commit,omitempty"`
	GitDirty     bool              `json:"git_dirty,omitempty"`
}

func (s Spec) Digest() string {
	return digest(s)
}

// ReceiptSpec is the durable, non-secret view of an admitted request. No value
// derived from the runtime environment is persisted.
type ReceiptSpec struct {
	ReceiptVersion int               `json:"receipt_version"`
	V              int               `json:"v"`
	Argv           []string          `json:"argv"`
	EnvNames       []string          `json:"env_names,omitempty"`
	EnvSources     map[string]string `json:"env_sources,omitempty"`
	Workdir        string            `json:"workdir,omitempty"`
	ManifestRoot   string            `json:"manifest_root"`
	Limits         Limits            `json:"limits"`
	GitCommit      string            `json:"git_commit,omitempty"`
	GitDirty       bool              `json:"git_dirty,omitempty"`
	// RequestDigest is read only to migrate receipts written before receipt
	// version 1. New receipts never populate it because it includes Env values.
	RequestDigest string `json:"request_digest,omitempty"`
}

func NewReceiptSpec(s Spec) ReceiptSpec {
	names := make([]string, 0, len(s.Env))
	for name := range s.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	return ReceiptSpec{
		ReceiptVersion: ReceiptVersion,
		V:              s.V, Argv: s.Argv, EnvNames: names, EnvSources: s.EnvSources, Workdir: s.Workdir,
		ManifestRoot: s.ManifestRoot, Limits: s.Limits,
		GitCommit: s.GitCommit, GitDirty: s.GitDirty,
	}
}

func (r ReceiptSpec) SpecWithoutEnv() Spec {
	return Spec{
		V: r.V, Argv: r.Argv, EnvSources: r.EnvSources, Workdir: r.Workdir, ManifestRoot: r.ManifestRoot,
		Limits: r.Limits, GitCommit: r.GitCommit, GitDirty: r.GitDirty,
	}
}

// Admission records who was allowed to run a job, and why.
type Admission struct {
	Time       time.Time `json:"time"`
	UserID     int64     `json:"user_id,omitempty"`
	UserLogin  string    `json:"user_login,omitempty"`
	NodeID     string    `json:"node_id,omitempty"`
	NodeName   string    `json:"node_name,omitempty"`
	RemoteAddr string    `json:"remote_addr"`
	Method     string    `json:"method"` // capability | allowlist | insecure-test
	Facts      Facts     `json:"facts"`
}

// Result is written once at terminal completion. Process result and
// transaction result are separate outcomes.
type Result struct {
	State            string     `json:"state,omitempty"`
	Started          bool       `json:"started"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	SettledAt        *time.Time `json:"settled_at,omitempty"`
	DurationMS       int64      `json:"duration_ms,omitempty"`
	ExitCode         *int       `json:"exit_code"` // nil when signaled or never started
	Signal           string     `json:"signal,omitempty"`
	SignalNum        int        `json:"signal_num,omitempty"`
	StartError       string     `json:"start_error,omitempty"`
	TransactionError string     `json:"transaction_error,omitempty"`
	LimitExceeded    string     `json:"limit_exceeded,omitempty"` // log_bytes | runtime | workspace_bytes
	OutputsOK        bool       `json:"outputs_ok"`
	CleanupOK        bool       `json:"cleanup_ok"`
	LogsComplete     bool       `json:"logs_complete"`
}

// ExecutionContext records the top-level execution facts controlled by the
// daemon. It is diagnostic evidence, not transitive execution tracing.
type ExecutionContext struct {
	Path string   `json:"path,omitempty"`
	Argv []string `json:"argv"`
	// PATHSHA256 is read only for receipt migration. New receipts never
	// populate it because PATH may be an explicitly supplied secret.
	PATHSHA256 string `json:"path_env_sha256,omitempty"`
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

type CacheGCResult struct {
	RemovedBlobs int   `json:"removed_blobs"`
	FreedBytes   int64 `json:"freed_bytes"`
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
	// Busy means a submission right now would be refused: every running
	// slot and queue position is taken.
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
