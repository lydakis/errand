// Package daemon is the errand runner: one HTTP listener on a private
// address, whois-derived authorization, at-most-once job admission, and
// receipts on disk.
package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/archive"
	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/logio"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

const StateAmbiguous = proto.StateAmbiguous

const (
	maxSpecBytes          = 1 << 20
	maxManifestBytes      = 64 << 20
	defaultUploadOverhead = 1 << 30
	changeStreamIdleLimit = 2 * time.Minute
	setupQuiesceDuration  = 5 * time.Minute
)

const (
	defaultCacheMaxBytes = 5 << 30
	defaultCacheTTL      = 14 * 24 * time.Hour
)

type Config struct {
	Listen           string
	StateDir         string
	AllowUsers       []string
	Capability       string
	TailscaledSocket string
	InsecureNoAuth   bool
	Identity         tailnet.Provider
	MaxUploadBytes   int64
	MaxLimits        proto.Limits // ceiling a spec may request
	Version          string

	CacheDisabled bool
	CacheMaxBytes int64
	CacheTTL      time.Duration

	// MaxJobs defaults to one for direct callers. MaxQueued is the exact
	// waiting capacity; zero disables queueing.
	MaxJobs   int
	MaxQueued int
}

type Daemon struct {
	cfg      Config
	identity tailnet.Provider
	selfUID  uint32
	cache    *blobCache // nil when the cache is disabled

	mu        sync.Mutex
	jobs      map[string]*Job
	running   map[string]*Job
	collected map[string]collectedRecord
	clockMu   sync.Mutex
	// admissionHighWater never moves backward and is persisted before it is
	// used to expire replay-prevention markers.
	admissionHighWater time.Time
	// queue is the admission-ordered waiting list. Entries remain here while
	// staging and, once staged, while queued for a running slot.
	queue             []*Job
	draining          bool
	setupQuiesceToken string
	setupQuiesceUntil time.Time
	lockFile          *os.File
	closeOnce         sync.Once
	closeErr          error
}

func New(cfg Config) (*Daemon, error) {
	if cfg.Capability == "" {
		cfg.Capability = proto.DefaultCapability
	}
	identity := cfg.Identity
	if identity == nil && cfg.TailscaledSocket != "" {
		identity = tailnet.NewLocalAPI(cfg.TailscaledSocket)
	}
	if cfg.MaxLimits == (proto.Limits{}) {
		cfg.MaxLimits = proto.DefaultLimits()
	}
	if cfg.MaxLimits.MaxLogBytes <= 0 || cfg.MaxLimits.MaxRuntimeSec <= 0 ||
		cfg.MaxLimits.MaxWorkspaceBytes <= 0 || cfg.MaxLimits.MaxChangeBytes <= 0 {
		return nil, fmt.Errorf("runner limit ceilings must be positive")
	}
	if cfg.MaxUploadBytes == 0 {
		cfg.MaxUploadBytes = cfg.MaxLimits.MaxWorkspaceBytes + defaultUploadOverhead
	}
	if cfg.MaxUploadBytes <= cfg.MaxLimits.MaxWorkspaceBytes {
		return nil, fmt.Errorf("max upload bytes must exceed the workspace byte ceiling")
	}
	if cfg.CacheMaxBytes == 0 {
		cfg.CacheMaxBytes = defaultCacheMaxBytes
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = defaultCacheTTL
	}
	if cfg.CacheMaxBytes < 0 || cfg.CacheTTL < 0 {
		return nil, fmt.Errorf("cache size and TTL must not be negative")
	}
	if cfg.MaxJobs == 0 {
		cfg.MaxJobs = 1
	}
	if cfg.MaxJobs < 0 {
		return nil, fmt.Errorf("max jobs must be positive")
	}
	if cfg.MaxQueued < -1 {
		return nil, fmt.Errorf("max queued must not be less than -1")
	}
	if cfg.MaxQueued == -1 {
		cfg.MaxQueued = 0
	}
	d := &Daemon{
		cfg: cfg, jobs: map[string]*Job{}, running: map[string]*Job{}, collected: map[string]collectedRecord{},
		identity: identity, selfUID: currentUID(),
	}
	if err := d.lockStateDir(); err != nil {
		return nil, err
	}
	if err := d.loadAdmissionClock(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := d.loadCollected(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := d.loadExisting(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if !cfg.CacheDisabled {
		cache, err := newBlobCache(filepath.Join(cfg.StateDir, "cache", "blobs"), cfg.CacheMaxBytes, cfg.CacheTTL)
		if err != nil {
			_ = d.Close()
			return nil, fmt.Errorf("opening snapshot cache: %w", err)
		}
		d.cache = cache
	}
	return d, nil
}

func (d *Daemon) lockStateDir() error {
	if err := os.MkdirAll(d.cfg.StateDir, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(d.cfg.StateDir, ".daemon.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return fmt.Errorf("state directory %q is already in use: %w", d.cfg.StateDir, err)
	}
	d.lockFile = f
	return nil
}

// Close releases the process-wide ownership of the daemon state directory.
func (d *Daemon) Close() error {
	d.closeOnce.Do(func() {
		if d.lockFile == nil {
			return
		}
		if err := syscall.Flock(int(d.lockFile.Fd()), syscall.LOCK_UN); err != nil {
			d.closeErr = err
		}
		if err := d.lockFile.Close(); err != nil && d.closeErr == nil {
			d.closeErr = err
		}
	})
	return d.closeErr
}

func (d *Daemon) jobsDir() string { return filepath.Join(d.cfg.StateDir, "jobs") }

func (d *Daemon) collectedDir() string { return filepath.Join(d.cfg.StateDir, "collected") }

func replaceJSON(dest string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(dest, append(b, '\n'))
}

func replaceJSONDurable(dest string, v any) error {
	return replaceJSONDurableWith(dest, v, syncDirectory)
}

func replaceJSONDurableWith(dest string, v any, syncDir func(string) error) error {
	if err := replaceJSON(dest, v); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dest))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func ensureChildDirectoryDurable(path string, mode os.FileMode) error {
	return ensureChildDirectoryDurableWith(path, mode, syncDirectory)
}

func ensureChildDirectoryDurableWith(path string, mode os.FileMode, syncParent func(string) error) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return syncParent(filepath.Dir(path))
}

func replaceFile(dest string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".replace-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func decodeStrictJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

// loadExisting restores receipts from disk. A job dir without a result is
// ambiguous: never replayed, reported as such.
func (d *Daemon) loadExisting() error {
	if err := os.MkdirAll(d.jobsDir(), 0o700); err != nil {
		return err
	}
	if _, err := cleanupGCTombstones(context.Background(), d.jobsDir(), removeOwnedTree); err != nil {
		return fmt.Errorf("scanning interrupted job GC tombstones: %w", err)
	}
	if err := d.pruneCollected(context.Background(), d.admissionNow(time.Now())); err != nil {
		return fmt.Errorf("pruning collection markers after tombstone recovery: %w", err)
	}
	entries, err := os.ReadDir(d.jobsDir())
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() && strings.HasPrefix(ent.Name(), ".gc-") {
			continue
		}
		if !ent.IsDir() || !proto.ValidULID(ent.Name()) {
			continue
		}
		dir := filepath.Join(d.jobsDir(), ent.Name())
		if err := changeops.CleanupTemps(dir); err != nil {
			return fmt.Errorf("cleaning interrupted change collection %s: %w", ent.Name(), err)
		}
		j := newJob(ent.Name(), dir)
		close(j.done)
		j.markExecutionDone()
		j.markLogReady()
		if admRaw, err := os.ReadFile(filepath.Join(dir, "admission.json")); err == nil {
			json.Unmarshal(admRaw, &j.Admission)
		}
		specRaw, err := os.ReadFile(filepath.Join(dir, "spec.json"))
		if err != nil {
			isolateUnreadableReceipt(j, "spec.json", err)
			d.jobs[j.ID] = j
			continue
		}
		var receipt proto.ReceiptSpec
		if err := decodeStrictJSON(specRaw, &receipt); err != nil {
			isolateUnreadableReceipt(j, "spec.json", err)
			d.jobs[j.ID] = j
			continue
		}
		if receipt.ReceiptVersion != proto.ReceiptVersion {
			return fmt.Errorf("loading receipt %s: unsupported receipt version %d", ent.Name(), receipt.ReceiptVersion)
		}
		j.Spec = receipt.SpecWithoutEnv()
		if len(receipt.EnvNames) == 0 {
			j.RequestDigest = j.Spec.Digest()
		}
		if resRaw, err := os.ReadFile(filepath.Join(dir, "result.json")); err == nil {
			var res proto.Result
			if err := decodeStrictJSON(resRaw, &res); err != nil {
				isolateUnreadableReceipt(j, "result.json", err)
				d.jobs[j.ID] = j
				continue
			}
			if res.State != proto.StateExited && res.State != proto.StateKilled && res.State != proto.StateAmbiguous {
				return fmt.Errorf("loading result %s: invalid terminal state %q", ent.Name(), res.State)
			}
			j.result = &res
			j.state = res.State
		} else if !os.IsNotExist(err) {
			isolateUnreadableReceipt(j, "result.json", err)
			d.jobs[j.ID] = j
			continue
		}
		if j.result == nil {
			if persistedQueuedWithoutScope(j.Dir) {
				d.reconcileQueued(j)
			} else {
				d.reconcileUnfinished(j)
			}
		} else {
			scopePath := filepath.Join(j.Dir, "scope.json")
			if _, err := os.Lstat(scopePath); err == nil || !os.IsNotExist(err) {
				d.reconcileSettledCleanup(j)
			}
		}
		d.jobs[j.ID] = j
		if _, ok := d.collected[j.ID]; ok {
			if err := os.Remove(filepath.Join(d.collectedDir(), j.ID+".json")); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing stale collection marker %s: %w", j.ID, err)
			}
			delete(d.collected, j.ID)
		}
	}
	return nil
}

func isolateUnreadableReceipt(j *Job, name string, decodeErr error) {
	_, cleanupErrs := cleanupPersistedRuntime(j)
	settledAt := time.Now()
	detail := fmt.Sprintf("receipt is unreadable: %s: %v", name, decodeErr)
	res := &proto.Result{
		State: proto.StateAmbiguous, SettledAt: &settledAt,
		ChangesOK: false, CleanupOK: len(cleanupErrs) == 0, LogsComplete: false,
		TransactionError: detail,
	}
	if len(cleanupErrs) > 0 {
		res.TransactionError = appendTransactionError(res.TransactionError,
			"cleanup: "+strings.Join(cleanupErrs, "; "))
	}
	j.state = proto.StateAmbiguous
	j.result = res
	j.event("receipt-load-failed", res.TransactionError)
}

// cleanupPersistedRuntime consumes a persisted process-scope marker. The
// workspace is recovery evidence because cwd membership is a secondary way
// to find descendants that scrub the inherited marker, so it is retained
// until the process scope is confirmed empty. The scope record is removed
// only after both process and workspace cleanup succeed.
func cleanupPersistedRuntime(j *Job) (killed []int, cleanupErrs []string) {
	scopePath := filepath.Join(j.Dir, "scope.json")
	workspace := filepath.Join(j.Dir, "workspace")
	raw, err := os.ReadFile(scopePath)
	scopePresent := err == nil
	switch {
	case err == nil:
		var rec scopeRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, []string{"scope record is unreadable; surviving processes cannot be found"}
		}
		scope, err := resumeProcessScope(rec.Token, workspace)
		if err != nil {
			return nil, []string{err.Error()}
		}
		killed, err = scope.cleanup(2 * time.Second)
		if err != nil {
			return killed, []string{err.Error()}
		}
	case os.IsNotExist(err):
		// A process cannot have started before scope.json was persisted. This is
		// the pre-scope crash window, so only the partial workspace can remain.
	default:
		return nil, []string{err.Error()}
	}

	if err := removeOwnedTree(workspace); err != nil {
		return killed, []string{"removing workspace: " + err.Error()}
	}
	if err := removeOwnedTree(filepath.Join(j.Dir, "change-base")); err != nil {
		return killed, []string{"removing submitted change base: " + err.Error()}
	}
	// queued.json distinguishes a job that cannot have started from one whose
	// execution is ambiguous. Remove it before scope.json so no crash can leave
	// the unsafe marker-without-scope combination after a process may have run.
	if err := removeQueuedMarker(j.Dir); err != nil {
		return killed, []string{"removing queued marker: " + err.Error()}
	}
	if scopePresent {
		if err := removeScopeRecord(scopePath); err != nil {
			return killed, []string{"removing process scope record: " + err.Error()}
		}
	}
	return killed, nil
}

func persistedQueuedWithoutScope(dir string) bool {
	if _, err := os.Lstat(filepath.Join(dir, queuedMarkerName)); err != nil {
		return false
	}
	_, err := os.Lstat(filepath.Join(dir, "scope.json"))
	return os.IsNotExist(err)
}

// reconcileQueued settles an acknowledged queue entry that cannot have run.
// The marker remains as receipt evidence, so another crash before result.json
// is written cannot turn a known never-started job into an ambiguous one.
func (d *Daemon) reconcileQueued(j *Job) {
	startError := "daemon restarted while job was queued; command never started"
	var cleanupErrs []string
	if err := removeOwnedTree(filepath.Join(j.Dir, "workspace")); err != nil {
		cleanupErrs = append(cleanupErrs, "removing workspace: "+err.Error())
	}
	if err := removeOwnedTree(filepath.Join(j.Dir, "change-base")); err != nil {
		cleanupErrs = append(cleanupErrs, "removing submitted change base: "+err.Error())
	}
	settledAt := time.Now()
	res := &proto.Result{
		State: proto.StateExited, StartError: startError,
		SettledAt: &settledAt,
		ChangesOK: true, CleanupOK: len(cleanupErrs) == 0, LogsComplete: true,
	}
	if len(cleanupErrs) > 0 {
		res.TransactionError = "cleanup: " + strings.Join(cleanupErrs, "; ")
	}
	j.event("reconciled-queued-after-restart", startError)
	if err := j.writeJSON("result.json", res); err != nil {
		j.event("result-write-failed", err.Error())
		res.State = StateAmbiguous
		res.TransactionError = appendTransactionError(res.TransactionError, "persisting result: "+err.Error())
	}
	j.state = res.State
	j.result = res
}

// reconcileSettledCleanup retries cleanup without rewriting the immutable
// terminal result. The original CleanupOK=false remains a truthful record of
// settlement; append-only events record the later recovery attempt.
func (d *Daemon) reconcileSettledCleanup(j *Job) {
	killed, cleanupErrs := cleanupPersistedRuntime(j)
	if len(killed) > 0 {
		j.event("scope-killed", fmt.Sprintf("pids=%v (terminal recovery)", killed))
	}
	if len(cleanupErrs) > 0 {
		j.event("recovery-cleanup-failed", strings.Join(cleanupErrs, "; "))
		return
	}
	j.event("recovery-cleanup-succeeded", "retained runtime state removed after terminal settlement")
}

// reconcileUnfinished settles a job the previous daemon left without a
// result: terminate any surviving scoped processes, clean the workspace,
// and write a durable ambiguous result. It never replays, and it never
// treats the absence of surviving processes as evidence the command did
// not run — the exit status is simply unknown.
func (d *Daemon) reconcileUnfinished(j *Job) {
	// "Execution state unknown" is load-bearing: Result.Started stays false
	// because errand cannot know, and this wording keeps the receipt from
	// reading as "never ran". The events file may still record a started pid.
	detail := "daemon restarted before a result was recorded; execution state unknown; not replayed"
	killed, cleanupErrs := cleanupPersistedRuntime(j)
	if len(killed) > 0 {
		j.event("scope-killed", fmt.Sprintf("pids=%v (reconciliation)", killed))
		detail = fmt.Sprintf(
			"daemon restarted before a result was recorded; %d surviving processes terminated; exit status unknown; not replayed",
			len(killed))
	}

	settledAt := time.Now()
	res := &proto.Result{
		State: StateAmbiguous, TransactionError: detail,
		SettledAt: &settledAt,
		ChangesOK: false, CleanupOK: len(cleanupErrs) == 0, LogsComplete: false,
	}
	bundle, changeErr := changeops.Load(j.Dir)
	if changeErr == nil {
		baseArchive, err := changeops.OpenBaseArchive(j.Dir)
		if err == nil {
			err = baseArchive.Close()
		}
		if err == nil {
			remoteArchive, openErr := changeops.OpenRemoteArchive(j.Dir)
			if openErr == nil {
				openErr = remoteArchive.Close()
			}
			err = openErr
		}
		changeErr = err
	}
	switch {
	case changeErr == nil:
		res.ChangesOK = true
		res.Changes = summarizeChangeBundle(bundle)
	case !errors.Is(changeErr, os.ErrNotExist):
		res.TransactionError = appendTransactionError(res.TransactionError,
			"recovering committed workspace changes: "+changeErr.Error())
	}
	if len(cleanupErrs) > 0 {
		res.TransactionError = appendTransactionError(res.TransactionError,
			"cleanup: "+strings.Join(cleanupErrs, "; "))
	}
	j.event("reconciled-after-restart", res.TransactionError)
	if err := j.writeJSON("result.json", res); err != nil {
		j.event("result-write-failed", err.Error())
		res.TransactionError = appendTransactionError(res.TransactionError, "persisting result: "+err.Error())
	}
	j.state = StateAmbiguous
	j.result = res
}

// release frees a settled job's reservation and drains the queue.
func (d *Daemon) release(j *Job) {
	d.mu.Lock()
	delete(d.running, j.ID)
	d.removeQueuedLocked(j)
	d.mu.Unlock()
	d.drainQueue()
}

// drainQueue starts queued jobs while free running slots exist, FIFO.
func (d *Daemon) drainQueue() {
	d.mu.Lock()
	if d.draining {
		d.mu.Unlock()
		return
	}
	d.draining = true
	d.mu.Unlock()
	go d.runQueue()
}

func (d *Daemon) runQueue() {
	for {
		d.mu.Lock()
		if len(d.running) >= d.cfg.MaxJobs || len(d.queue) == 0 {
			d.draining = false
			d.mu.Unlock()
			return
		}
		next := d.queue[0]
		next.mu.Lock()
		ready := next.state == proto.StateQueued
		next.mu.Unlock()
		if !ready {
			d.draining = false
			d.mu.Unlock()
			return
		}
		d.queue = d.queue[1:]
		d.running[next.ID] = next
		d.mu.Unlock()
		d.launchDequeued(next)
	}
}

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/info", d.auth("", d.handleInfo))
	mux.HandleFunc("POST /v0/setup/quiesce", d.auth("", d.handleSetupQuiesce))
	mux.HandleFunc("DELETE /v0/setup/quiesce", d.auth("", d.handleSetupQuiesceRelease))
	mux.HandleFunc("GET /v0/jobs", d.auth(proto.ActionReadOwn, d.handleList))
	mux.HandleFunc("POST /v0/snapshot/diff", d.auth(proto.ActionSubmit, d.handleSnapshotDiff))
	mux.HandleFunc("GET /v0/storage", d.auth(proto.ActionReadOwn, d.handleStorageStats))
	mux.HandleFunc("POST /v0/cache/gc", d.auth(proto.ActionCaches, d.handleCacheGC))
	mux.HandleFunc("POST /v0/jobs/gc", d.auth(proto.ActionGCJobs, d.handleJobGC))
	mux.HandleFunc("GET /v0/change-reconciliation", d.auth(proto.ActionGCJobs, d.handleChangeReconciliation))
	mux.HandleFunc("POST /v0/change-reconciliation/ack", d.auth(proto.ActionGCJobs, d.handleChangeReconciliationAck))
	mux.HandleFunc("PUT /v0/jobs/{id}", d.auth(proto.ActionSubmit, d.handleSubmit))
	mux.HandleFunc("GET /v0/jobs/{id}", d.auth(proto.ActionReadOwn, d.handleStatus))
	mux.HandleFunc("GET /v0/jobs/{id}/logs", d.auth(proto.ActionReadOwn, d.handleLogs))
	mux.HandleFunc("GET /v0/jobs/{id}/changes", d.auth(proto.ActionReadOwn, d.handleChanges))
	mux.HandleFunc("POST /v0/jobs/{id}/ports/{port}/connect", d.auth(proto.ActionForwardOwn, d.handleForward))
	mux.HandleFunc("POST /v0/jobs/{id}/signal", d.auth(proto.ActionKillOwn, d.handleSignal))
	mux.HandleFunc("POST /v0/jobs/{id}/kill", d.auth(proto.ActionKillOwn, d.handleKill))
	return mux
}

func (d *Daemon) handleSetupQuiesce(w http.ResponseWriter, _ *http.Request, id Identity) {
	if !id.Local {
		httpError(w, http.StatusForbidden, "runner setup is available only through the local Unix socket")
		return
	}
	now := time.Now()
	d.mu.Lock()
	if d.setupQuiesceToken != "" && now.Before(d.setupQuiesceUntil) {
		d.mu.Unlock()
		httpError(w, http.StatusConflict, "runner setup is already in progress")
		return
	}
	o := d.occupancyLocked()
	if active := activeJobSummary(o); active != "" {
		d.mu.Unlock()
		httpError(w, http.StatusConflict, "runner has active jobs ("+active+"); wait until it is idle before restarting")
		return
	}
	token := proto.NewULID()
	expiresAt := now.Add(setupQuiesceDuration)
	d.setupQuiesceToken = token
	d.setupQuiesceUntil = expiresAt
	d.mu.Unlock()
	writeJSON(w, http.StatusCreated, proto.SetupQuiesce{Token: token, ExpiresAt: expiresAt})
}

func (d *Daemon) handleSetupQuiesceRelease(w http.ResponseWriter, r *http.Request, id Identity) {
	if !id.Local {
		httpError(w, http.StatusForbidden, "runner setup is available only through the local Unix socket")
		return
	}
	var req proto.SetupQuiesceRelease
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || req.Token == "" {
		httpError(w, http.StatusBadRequest, "a setup quiesce token is required")
		return
	}
	d.mu.Lock()
	if req.Token != d.setupQuiesceToken {
		d.mu.Unlock()
		httpError(w, http.StatusConflict, "setup quiesce token does not match")
		return
	}
	d.setupQuiesceToken = ""
	d.setupQuiesceUntil = time.Time{}
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

type handlerFunc func(http.ResponseWriter, *http.Request, Identity)

// auth resolves the caller and requires the given action ("" means any
// authorization suffices, e.g. for /v0/info). Fail closed.
func (d *Daemon) auth(action string, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Unix-socket callers are identified by kernel credentials; the
		// same-host rule exists only because WhoIs cannot see loopback.
		if _, local := localPeerFromContext(r.Context()); !local && !d.cfg.InsecureNoAuth {
			localAddr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
			if unsupportedSelfTarget(r.RemoteAddr, localAddr) {
				httpError(w, http.StatusForbidden, "self-targeting is not supported; choose another peer")
				return
			}
		}
		id, err := d.identifyRequest(r)
		if err != nil {
			httpError(w, http.StatusForbidden, err.Error())
			return
		}
		if action != "" && !id.Allowed(action) {
			httpError(w, http.StatusForbidden,
				fmt.Sprintf("caller %s lacks the %q action", id.Owner(), action))
			return
		}
		h(w, r, id)
	}
}

func (d *Daemon) handleInfo(w http.ResponseWriter, r *http.Request, _ Identity) {
	d.mu.Lock()
	o := d.occupancyLocked()
	busy := d.capacityFullLocked() || d.setupQuiesceToken != "" && time.Now().Before(d.setupQuiesceUntil)
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, proto.Info{
		Proto:        proto.ProtoVersion,
		Version:      d.cfg.Version,
		Busy:         busy,
		StagingJobs:  o.staging,
		StartingJobs: o.starting,
		RunningJobs:  o.running,
		QueuedJobs:   o.queued,
		MaxJobs:      d.cfg.MaxJobs,
		MaxQueued:    d.cfg.MaxQueued,
		Facts:        measureFacts(),
	})
}

// Negotiation is advisory; extraction detects blobs evicted before submission.
func (d *Daemon) handleSnapshotDiff(w http.ResponseWriter, r *http.Request, _ Identity) {
	if d.cache == nil {
		httpError(w, http.StatusNotFound, "snapshot cache is disabled on this runner")
		return
	}
	var req proto.SnapshotDiffRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxManifestBytes)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	missing, err := d.cache.MissingContext(r.Context(), req.Blobs)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, proto.SnapshotDiffResponse{Missing: missing})
}

func (d *Daemon) handleCacheGC(w http.ResponseWriter, r *http.Request, _ Identity) {
	if d.cache == nil {
		httpError(w, http.StatusNotFound, "snapshot cache is disabled on this runner")
		return
	}
	var req proto.CacheGCRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "decoding cache GC policy: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		httpError(w, http.StatusBadRequest, "cache GC policy must contain one JSON object")
		return
	}
	result, err := d.cache.GCContext(r.Context(), req.DryRun)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleSubmit is at-most-once admission: the in-memory registry (backed
// by the job directory) is the admission lock. Same ID + same digest
// returns the existing job; a different digest is a 409; a second
// concurrent job is busy.
func (d *Daemon) handleSubmit(w http.ResponseWriter, r *http.Request, id Identity) {
	jobID := r.PathValue("id")
	if !proto.ValidULID(jobID) {
		httpError(w, http.StatusBadRequest, "job id must be a ULID")
		return
	}
	freshnessErr := validateNewJobID(jobID, d.admissionNow(time.Now()))
	if freshnessErr != nil {
		d.mu.Lock()
		_, knownJob := d.jobs[jobID]
		_, knownCollected := d.collected[jobID]
		d.mu.Unlock()
		if !knownJob && !knownCollected {
			httpError(w, http.StatusBadRequest, freshnessErr.Error())
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, d.cfg.MaxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, http.StatusBadRequest, "expected multipart body: "+err.Error())
		return
	}

	var spec proto.Spec
	if err := readJSONPart(mr, "spec", maxSpecBytes, &spec); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateSpec(spec, d.cfg.MaxLimits); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	digest := spec.Digest()
	if claimed := r.Header.Get("X-Errand-Digest"); claimed != "" && claimed != digest {
		httpError(w, http.StatusBadRequest, "X-Errand-Digest does not match the submitted spec")
		return
	}
	project, projectTruncated := projectMetadata(r)
	var manifest proto.Manifest
	if err := readJSONPart(mr, "manifest", maxManifestBytes, &manifest); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if manifest.RootHash() != spec.ManifestRoot {
		httpError(w, http.StatusBadRequest, "manifest does not hash to spec's manifest_root")
		return
	}
	if err := archive.Validate(manifest); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	workspace, err := nextPart(mr, "workspace")
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	d.mu.Lock()
	if existing, ok := d.jobs[jobID]; ok {
		d.mu.Unlock()
		if !d.ownsJob(id, existing) {
			httpError(w, http.StatusForbidden, "not the owner of this job")
			return
		}
		if existing.RequestDigest == "" {
			httpError(w, http.StatusConflict, "job id exists, but request identity cannot be verified after restart")
			return
		}
		if existing.RequestDigest != digest {
			httpError(w, http.StatusConflict, "job id exists with a different request digest")
			return
		}
		writeJSON(w, http.StatusOK, existing.Status())
		return
	}
	if collected, ok := d.collected[jobID]; ok {
		d.mu.Unlock()
		if !d.cfg.InsecureNoAuth && collected.Owner != id.Owner() {
			httpError(w, http.StatusForbidden, "not the owner of this job")
			return
		}
		if collected.RequestDigest != "" && collected.RequestDigest != digest {
			httpError(w, http.StatusConflict, "job id was collected with a different request digest")
			return
		}
		httpError(w, http.StatusGone, "job receipt was collected; the job id cannot be replayed")
		return
	}
	if freshnessErr != nil {
		d.mu.Unlock()
		httpError(w, http.StatusBadRequest, freshnessErr.Error())
		return
	}
	if d.setupQuiesceToken != "" && time.Now().Before(d.setupQuiesceUntil) {
		d.mu.Unlock()
		httpError(w, http.StatusServiceUnavailable, "runner is being reconfigured; retry on another peer")
		return
	}
	if d.capacityFullLocked() {
		o := d.occupancyLocked()
		msg := fmt.Sprintf("busy: %d running, %d starting, %d staging, %d queued (capacity %d running + %d queued)",
			o.running, o.starting, o.staging, o.queued, d.cfg.MaxJobs, d.cfg.MaxQueued)
		d.mu.Unlock()
		httpError(w, http.StatusTooManyRequests, msg)
		return
	}
	dir := filepath.Join(d.jobsDir(), jobID)
	tmpDir, err := os.MkdirTemp(d.jobsDir(), ".admission-"+jobID+"-")
	if err != nil {
		d.mu.Unlock()
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	j := newJob(jobID, tmpDir)
	j.Spec = spec
	j.RequestDigest = digest
	j.baseline = manifest
	j.Admission = proto.Admission{
		Time: time.Now(), UserID: id.UserID, UserLogin: id.Login,
		NodeID: id.NodeID, NodeName: id.Node,
		RemoteAddr: r.RemoteAddr, Method: id.Method,
		LocalUID: int64(id.LocalUID), LocalUser: id.LocalUser,
		Project: project, ProjectTruncated: projectTruncated, Facts: measureFacts(),
	}
	j.state = proto.StateStaging
	if err := j.writeJSON("spec.json", proto.NewReceiptSpec(spec)); err == nil {
		err = j.writeJSON("admission.json", j.Admission)
	}
	if err == nil {
		err = os.Rename(tmpDir, dir)
	}
	if err != nil {
		d.mu.Unlock()
		os.RemoveAll(tmpDir)
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	j.Dir = dir
	d.jobs[jobID] = j
	d.queue = append(d.queue, j)
	d.mu.Unlock()

	// rejectPreExecution mirrors the pre-3.5 semantics for failures during
	// the submit request: a kill that raced the failure becomes a durable
	// terminal result; anything else rolls the admission back entirely.
	rejectPreExecution := func(err error) {
		j.event("start-rejected", err.Error())
		if res := j.settleStartFailure(); res != nil {
			j.finalize(d, res, true)
			writeJSON(w, http.StatusCreated, j.Status())
			return
		}
		if cleanupErr := d.abortAdmission(j, err); cleanupErr != nil {
			httpError(w, http.StatusInternalServerError, errors.Join(err, cleanupErr).Error())
		} else {
			if errors.Is(err, archive.ErrCacheMiss) {
				httpErrorCode(w, http.StatusConflict, proto.ErrorCodeSnapshotCacheMiss, err.Error())
			} else {
				httpError(w, http.StatusBadRequest, err.Error())
			}
		}
	}

	settled, err := j.stage(d, &stagingUpload{Reader: workspace, body: r.Body}, manifest)
	if err != nil {
		rejectPreExecution(err)
		return
	}
	if settled { // killed during staging; already finalized durably
		writeJSON(w, http.StatusCreated, j.Status())
		return
	}
	cancelled, err := d.queueStaged(j)
	if err != nil {
		rejectPreExecution(err)
		return
	}
	if cancelled {
		writeJSON(w, http.StatusCreated, j.Status())
		return
	}
	writeJSON(w, http.StatusCreated, j.Status())
}

// queueStaged commits the durable queue phase. Every launch then flows through
// the single drain worker, which preserves process-start order.
func (d *Daemon) queueStaged(j *Job) (cancelled bool, err error) {
	if err := j.writeJSON(queuedMarkerName, queuedRecord{State: proto.StateQueued}); err != nil {
		return false, fmt.Errorf("persisting queued state: %w", err)
	}
	d.mu.Lock()
	j.mu.Lock()
	if j.killed != "" {
		res := &proto.Result{
			Signal: j.killSignal.String(), SignalNum: int(j.killSignal),
			ChangesOK: true, LogsComplete: true,
		}
		d.removeQueuedLocked(j)
		j.mu.Unlock()
		d.mu.Unlock()
		j.finalize(d, res, true)
		return true, nil
	}
	j.state = proto.StateQueued
	j.mu.Unlock()
	position := 0
	for i, queued := range d.queue {
		if queued == j {
			position = i + 1
			break
		}
	}
	j.event("queued", fmt.Sprintf("position=%d", position))
	d.mu.Unlock()
	d.drainQueue()
	return false, nil
}

// launchDequeued starts a job that waited in the queue. Its submitter is
// long gone, so a launch failure settles into a durable receipt instead of
// an admission rollback.
func (d *Daemon) launchDequeued(j *Job) {
	if err := j.launch(d); err != nil {
		j.event("start-rejected", err.Error())
		res := j.settleStartFailure()
		if res == nil {
			res = &proto.Result{
				State: proto.StateExited, StartError: err.Error(),
				ChangesOK: true, LogsComplete: true,
			}
		}
		j.finalize(d, res, true)
	}
}

// cancelBeforeStart owns every cancellation before cmd.Start. A successful
// return means the killed receipt is durable, not merely that staging stopped.
func (d *Daemon) cancelBeforeStart(ctx context.Context, j *Job, reason string, sig syscall.Signal) (bool, error) {
	d.mu.Lock()
	j.mu.Lock()
	if j.result == nil && j.reaped && j.changeCancel != nil {
		j.mu.Unlock()
		d.mu.Unlock()
		return false, nil
	}
	if j.result != nil || j.reaped || j.startRejected || j.state == proto.StateExited ||
		j.state == proto.StateKilled || j.state == proto.StateAmbiguous {
		j.mu.Unlock()
		d.mu.Unlock()
		return true, fmt.Errorf("job %s is not running", j.ID)
	}
	if j.started {
		j.mu.Unlock()
		d.mu.Unlock()
		return false, nil
	}
	if j.killed == "" {
		j.killed = reason
		j.killSignal = sig
	}
	stagingCancel := j.stagingCancel
	removed := j.state == proto.StateQueued && d.removeQueuedLocked(j)
	j.mu.Unlock()
	d.mu.Unlock()

	if stagingCancel != nil {
		stagingCancel()
	}
	if removed {
		j.event("terminated", reason+" before start")
		j.finalize(d, j.cancelledBeforeStart(), true)
	}
	select {
	case <-j.done:
		return true, nil
	case <-ctx.Done():
		return true, ctx.Err()
	}
}

func (d *Daemon) removeQueuedLocked(j *Job) bool {
	for i, queued := range d.queue {
		if queued == j {
			d.queue = append(d.queue[:i], d.queue[i+1:]...)
			return true
		}
	}
	return false
}

type occupancy struct {
	staging  int
	queued   int
	starting int
	running  int
}

func activeJobSummary(o occupancy) string {
	counts := []struct {
		n int
		s string
	}{
		{o.staging, "staging"},
		{o.starting, "starting"},
		{o.running, "running"},
		{o.queued, "queued"},
	}
	var active []string
	for _, count := range counts {
		if count.n != 0 {
			active = append(active, fmt.Sprintf("%d %s", count.n, count.s))
		}
	}
	return strings.Join(active, ", ")
}

// occupancyLocked derives the public phase counts from scheduler ownership.
// d.mu must be held so queue and running membership cannot change mid-snapshot.
func (d *Daemon) occupancyLocked() occupancy {
	var o occupancy
	for _, j := range d.queue {
		j.mu.Lock()
		if j.state == proto.StateStaging {
			o.staging++
		} else {
			o.queued++
		}
		j.mu.Unlock()
	}
	for _, j := range d.running {
		j.mu.Lock()
		if j.state == proto.StateRunning {
			o.running++
		} else {
			o.starting++
		}
		j.mu.Unlock()
	}
	return o
}

func (d *Daemon) capacityFullLocked() bool {
	return len(d.running)+len(d.queue) >= d.cfg.MaxJobs+d.cfg.MaxQueued
}

type stagingUpload struct {
	io.Reader
	body io.ReadCloser
}

func (r *stagingUpload) Close() error { return r.body.Close() }

func (d *Daemon) abortAdmission(j *Job, startErr error) error {
	defer d.drainQueue() // a rollback can free a running slot
	cleanupErr := removeOwnedTree(j.Dir)
	var receiptWriteErr error
	d.mu.Lock()
	if cleanupErr == nil && d.jobs[j.ID] == j {
		delete(d.jobs, j.ID)
	}
	delete(d.running, j.ID)
	d.removeQueuedLocked(j)
	d.mu.Unlock()
	if cleanupErr != nil {
		rollbackErr := fmt.Errorf("cleaning rejected admission: %w", cleanupErr)
		startMessage := "job was rejected before execution"
		if startErr != nil {
			startMessage = startErr.Error()
		}
		settledAt := time.Now()
		result := &proto.Result{
			State: proto.StateAmbiguous, StartError: startMessage,
			SettledAt:        &settledAt,
			TransactionError: rollbackErr.Error(), ChangesOK: true,
			CleanupOK: false, LogsComplete: true,
		}
		// A failed recursive removal can already have deleted the receipt files
		// before it discovers that the job directory itself cannot be removed.
		// Rebuild the redacted durable receipt rather than retaining truth only in
		// memory until the next daemon restart.
		if err := replaceJSON(filepath.Join(j.Dir, "spec.json"), proto.NewReceiptSpec(j.Spec)); err != nil {
			receiptWriteErr = errors.Join(receiptWriteErr, fmt.Errorf("recording rejected admission spec: %w", err))
		}
		if err := replaceJSON(filepath.Join(j.Dir, "admission.json"), j.Admission); err != nil {
			receiptWriteErr = errors.Join(receiptWriteErr, fmt.Errorf("recording rejected admission identity: %w", err))
		}
		if receiptWriteErr != nil {
			result.TransactionError = errors.Join(rollbackErr, receiptWriteErr).Error()
		}
		if err := replaceJSON(filepath.Join(j.Dir, "result.json"), result); err != nil {
			receiptWriteErr = errors.Join(receiptWriteErr, fmt.Errorf("recording rejected admission result: %w", err))
			result.TransactionError = errors.Join(rollbackErr, receiptWriteErr).Error()
		}
		j.mu.Lock()
		j.Spec.Env = nil
		j.state = proto.StateAmbiguous
		j.result = result
		j.mu.Unlock()
	}
	j.markLogReady()
	select {
	case <-j.done:
	default:
		close(j.done)
	}
	if cleanupErr != nil {
		return errors.Join(fmt.Errorf("cleaning rejected admission: %w", cleanupErr), receiptWriteErr)
	}
	return nil
}

func validateSpec(s proto.Spec, maxLimits proto.Limits) error {
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return fmt.Errorf("spec has empty argv")
	}
	if s.NoSnapshot && s.ManifestRoot != (proto.Manifest{}).RootHash() {
		return fmt.Errorf("no-snapshot spec must use the empty manifest root")
	}
	if s.NoSnapshot && !s.Selection.IsZero() {
		return fmt.Errorf("no-snapshot spec must use an empty selection policy")
	}
	if _, err := pathpolicy.Compile(s.Selection); err != nil {
		return fmt.Errorf("invalid selection policy: %w", err)
	}
	receiptRaw, err := json.Marshal(proto.NewReceiptSpec(s))
	if err != nil {
		return fmt.Errorf("encoding receipt spec: %w", err)
	}
	if len(receiptRaw) > maxReceiptSpecBytes {
		return fmt.Errorf("spec metadata exceeds %d bytes", maxReceiptSpecBytes)
	}
	for name, source := range s.EnvSources {
		if _, ok := s.Env[name]; !ok {
			return fmt.Errorf("environment source for undeclared variable %q", name)
		}
		if source != "literal" && source != "passenv" {
			return fmt.Errorf("invalid environment source %q for %q", source, name)
		}
	}
	for name := range s.Env {
		if _, ok := s.EnvSources[name]; !ok {
			return fmt.Errorf("environment variable %q is missing provenance", name)
		}
	}
	if s.Workdir != "" {
		clean := path.Clean(s.Workdir)
		if path.IsAbs(s.Workdir) || clean != s.Workdir || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("unsafe workdir %q", s.Workdir)
		}
	}
	if s.GitCommit != "" {
		if len(s.GitCommit) != 40 && len(s.GitCommit) != 64 {
			return fmt.Errorf("git commit must be a full 40- or 64-character object id")
		}
		if _, err := hex.DecodeString(s.GitCommit); err != nil {
			return fmt.Errorf("git commit must be hexadecimal")
		}
	}
	l := s.Limits
	if l.MaxLogBytes <= 0 || l.MaxRuntimeSec <= 0 || l.MaxWorkspaceBytes <= 0 || l.MaxChangeBytes <= 0 {
		return fmt.Errorf("spec limits must be positive")
	}
	if l.MaxLogBytes > maxLimits.MaxLogBytes ||
		l.MaxRuntimeSec > maxLimits.MaxRuntimeSec ||
		l.MaxWorkspaceBytes > maxLimits.MaxWorkspaceBytes ||
		l.MaxChangeBytes > maxLimits.MaxChangeBytes {
		return fmt.Errorf("spec limits exceed this runner's ceiling")
	}
	if !proto.ValidChangeClientID(s.ChangeClientID) {
		return fmt.Errorf("spec requires a valid change_client_id")
	}
	return nil
}

func (d *Daemon) lookup(w http.ResponseWriter, r *http.Request, id Identity) *Job {
	d.mu.Lock()
	j := d.jobs[r.PathValue("id")]
	d.mu.Unlock()
	if j == nil {
		httpError(w, http.StatusNotFound, "no such job")
		return nil
	}
	if !d.ownsJob(id, j) {
		httpError(w, http.StatusForbidden, "not the owner of this job")
		return nil
	}
	return j
}

func (d *Daemon) ownsJob(id Identity, j *Job) bool {
	if d.cfg.InsecureNoAuth {
		return true
	}
	owner := admissionOwner(j.Admission)
	return owner != "" && id.Owner() == owner
}

// handleList returns the caller's own jobs, newest first (ULIDs are
// time-ordered), capped so the response stays bounded. An active-only query
// filters before the cap so terminal receipts cannot hide live work.
func (d *Daemon) handleList(w http.ResponseWriter, r *http.Request, id Identity) {
	activeOnly := r.URL.Query().Get("active") == "1"
	d.mu.Lock()
	owned := make([]*Job, 0, len(d.jobs))
	for _, j := range d.jobs {
		if d.ownsJob(id, j) {
			owned = append(owned, j)
		}
	}
	d.mu.Unlock()
	if activeOnly {
		active := owned[:0]
		for _, j := range owned {
			state := j.Status().State
			if state == proto.StateStaging || state == proto.StateQueued || state == proto.StateRunning {
				active = append(active, j)
			}
		}
		owned = active
	}
	sort.Slice(owned, func(i, k int) bool { return owned[i].ID > owned[k].ID })
	if len(owned) > proto.MaxJobListEntries {
		owned = owned[:proto.MaxJobListEntries]
	}
	entries := make([]proto.JobListEntry, 0, len(owned))
	for _, j := range owned {
		entries = append(entries, j.summary())
	}
	body, err := json.Marshal(entries)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "encoding job listing")
		return
	}
	if len(body)+1 > maxListResponseBytes {
		httpError(w, http.StatusInternalServerError, "job listing exceeds response limit")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

func projectMetadata(r *http.Request) (string, bool) {
	encoded := r.Header.Get("X-Errand-Project-B64")
	if encoded == "" {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	project := string(decoded)
	truncated := r.Header.Get("X-Errand-Project-Truncated") == "1"
	project = strings.ToValidUTF8(project, "�")
	project, bounded := boundedListField(project, maxListProjectBytes)
	return project, truncated || bounded
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request, id Identity) {
	if j := d.lookup(w, r, id); j != nil {
		body, err := json.Marshal(j.Details())
		if err != nil {
			httpError(w, http.StatusInternalServerError, "encoding job details")
			return
		}
		if len(body)+1 > maxDetailResponseBytes {
			httpError(w, http.StatusInternalServerError, "job details exceed response limit")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(append(body, '\n'))
	}
}

// handleLogs streams framed log events over SSE, resumable via ?from= or
// Last-Event-ID, and terminates with a "status" event.
func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request, id Identity) {
	j := d.lookup(w, r, id)
	if j == nil {
		return
	}
	if !j.acquireLogReader() {
		httpError(w, http.StatusNotFound, "no such job")
		return
	}
	defer j.releaseLogReader()
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if lei, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && lei > from {
		from = lei
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	select {
	case <-j.logReady:
	case <-r.Context().Done():
		return
	}

	j.mu.Lock()
	live := j.logw
	started := j.started
	j.mu.Unlock()

	logPath := filepath.Join(j.Dir, "io.log")
	if _, err := os.Stat(logPath); err == nil {
		ctx := r.Context()
		if err := logio.Follow(ctx, logPath, from, live, func(f proto.LogFrame) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b, _ := json.Marshal(f)
			fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", f.Seq, b)
			flusher.Flush()
			return nil
		}); err != nil {
			if ctx.Err() != nil {
				return
			}
			b, _ := json.Marshal(proto.LogStreamError{
				Message: err.Error(), Retryable: !logio.IsIntegrityError(err) && retryableLogFileError(err),
			})
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
			flusher.Flush()
			return
		}
	} else {
		status := j.Status()
		logsExpected := started || (status.Result != nil && status.Result.Started)
		if logsExpected || !os.IsNotExist(err) {
			b, _ := json.Marshal(proto.LogStreamError{
				Message: "opening persisted logs: " + err.Error(), Retryable: retryableLogFileError(err),
			})
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
			flusher.Flush()
			return
		}
	}
	select {
	case <-j.done:
	case <-r.Context().Done():
		return
	}
	b, _ := json.Marshal(j.Status())
	fmt.Fprintf(w, "event: status\ndata: %s\n\n", b)
	flusher.Flush()
}

// handleChanges streams one immutable change bundle while holding a receipt
// reader reservation, so job GC cannot remove either multipart part mid-read.
func (d *Daemon) handleChanges(w http.ResponseWriter, r *http.Request, id Identity) {
	j := d.lookup(w, r, id)
	if j == nil {
		return
	}
	if !j.acquireChangeReader() {
		httpError(w, http.StatusNotFound, "no such job")
		return
	}
	defer j.releaseChangeReader()

	status := j.Status()
	if status.Result == nil {
		httpError(w, http.StatusConflict, "job is not terminal")
		return
	}
	if status.Result.Changes == nil {
		httpError(w, http.StatusNotFound, "job produced no workspace changes")
		return
	}
	bundle, err := changeops.Load(j.Dir)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "loading change bundle: "+err.Error())
		return
	}
	if !status.Result.Changes.Matches(bundle) {
		httpError(w, http.StatusInternalServerError, "change bundle does not match the terminal receipt")
		return
	}
	baseArchive, err := changeops.OpenBaseArchive(j.Dir)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "opening base change archive: "+err.Error())
		return
	}
	defer baseArchive.Close()
	remoteArchive, err := changeops.OpenRemoteArchive(j.Dir)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "opening remote change archive: "+err.Error())
		return
	}
	defer remoteArchive.Close()

	stream := newIdleDeadlineWriter(w, changeStreamIdleLimit)
	defer stream.clear()
	mw := multipart.NewWriter(stream)
	w.Header().Set("Content-Type", mw.FormDataContentType())
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	bundlePart, err := mw.CreateFormField("bundle")
	if err != nil {
		return
	}
	if err := json.NewEncoder(bundlePart).Encode(bundle); err != nil {
		return
	}
	basePart, err := mw.CreateFormFile("base", "base.tar")
	if err != nil {
		return
	}
	if _, err := io.Copy(basePart, baseArchive); err != nil {
		return
	}
	remotePart, err := mw.CreateFormFile("remote", "remote.tar")
	if err != nil {
		return
	}
	if _, err := io.Copy(remotePart, remoteArchive); err != nil {
		return
	}
	if err := mw.Close(); err != nil {
		return
	}
	_ = stream.flush()
}

type idleDeadlineWriter struct {
	destination io.Writer
	controller  *http.ResponseController
	idle        time.Duration
	now         func() time.Time
}

func newIdleDeadlineWriter(destination http.ResponseWriter, idle time.Duration) *idleDeadlineWriter {
	return &idleDeadlineWriter{
		destination: destination,
		controller:  http.NewResponseController(destination),
		idle:        idle,
		now:         time.Now,
	}
}

func (w *idleDeadlineWriter) Write(p []byte) (int, error) {
	if err := w.refresh(); err != nil {
		return 0, err
	}
	return w.destination.Write(p)
}

func (w *idleDeadlineWriter) refresh() error {
	err := w.controller.SetWriteDeadline(w.now().Add(w.idle))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (w *idleDeadlineWriter) flush() error {
	if err := w.refresh(); err != nil {
		return err
	}
	err := w.controller.Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (w *idleDeadlineWriter) clear() {
	_ = w.controller.SetWriteDeadline(time.Time{})
}

func retryableLogFileError(err error) bool {
	return !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) &&
		!errors.Is(err, syscall.EISDIR) && !errors.Is(err, syscall.ELOOP) &&
		!errors.Is(err, syscall.ENOTDIR)
}

func (d *Daemon) handleSignal(w http.ResponseWriter, r *http.Request, id Identity) {
	j := d.lookup(w, r, id)
	if j == nil {
		return
	}
	var req struct {
		Signal string `json:"signal"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	sigs := map[string]syscall.Signal{"SIGINT": syscall.SIGINT, "SIGTERM": syscall.SIGTERM}
	sig, ok := sigs[req.Signal]
	if !ok {
		httpError(w, http.StatusBadRequest, "signal must be SIGINT or SIGTERM")
		return
	}
	if handled, err := d.cancelBeforeStart(r.Context(), j, "user-signal", sig); handled {
		if err != nil {
			if r.Context().Err() == nil {
				httpError(w, http.StatusConflict, err.Error())
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := j.Signal(sig); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleKill(w http.ResponseWriter, r *http.Request, id Identity) {
	j := d.lookup(w, r, id)
	if j == nil {
		return
	}
	sig := syscall.SIGTERM
	if r.URL.Query().Get("force") == "1" {
		sig = syscall.SIGKILL
	}
	if handled, err := d.cancelBeforeStart(r.Context(), j, "user-kill", sig); handled {
		if err != nil {
			if r.Context().Err() == nil {
				httpError(w, http.StatusConflict, err.Error())
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := j.terminate("user-kill", sig); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func nextPart(mr *multipart.Reader, name string) (io.Reader, error) {
	p, err := mr.NextPart()
	if err != nil {
		return nil, fmt.Errorf("missing multipart part %q", name)
	}
	if p.FormName() != name {
		return nil, fmt.Errorf("expected part %q, got %q", name, p.FormName())
	}
	return p, nil
}

func readJSONPart(mr *multipart.Reader, name string, limit int64, v any) error {
	p, err := nextPart(mr, name)
	if err != nil {
		return err
	}
	if err := json.NewDecoder(io.LimitReader(p, limit)).Decode(v); err != nil {
		return fmt.Errorf("part %q: %w", name, err)
	}
	return nil
}

func httpError(w http.ResponseWriter, code int, msg string) {
	httpErrorCode(w, code, "", msg)
}

func httpErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, proto.APIError{Error: msg, Code: code})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
