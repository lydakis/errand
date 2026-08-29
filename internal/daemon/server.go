// Package daemon is the errand runner: one HTTP listener on a private
// address, whois-derived authorization, at-most-once job admission, and
// receipts on disk.
package daemon

import (
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/logio"
	"github.com/lydakis/errand/internal/proto"
)

const StateAmbiguous = proto.StateAmbiguous

const (
	maxSpecBytes          = 1 << 20
	maxManifestBytes      = 64 << 20
	defaultUploadOverhead = 1 << 30
)

type Config struct {
	Listen           string
	StateDir         string
	AllowUsers       []string
	Capability       string
	TailscaledSocket string
	InsecureNoAuth   bool
	MaxUploadBytes   int64
	MaxLimits        proto.Limits // ceiling a spec may request
	Version          string
}

type Daemon struct {
	cfg         Config
	whoisClient *http.Client

	mu        sync.Mutex
	jobs      map[string]*Job
	running   *Job
	lockFile  *os.File
	closeOnce sync.Once
	closeErr  error
}

func New(cfg Config) (*Daemon, error) {
	if cfg.Capability == "" {
		cfg.Capability = proto.DefaultCapability
	}
	if cfg.TailscaledSocket == "" {
		cfg.TailscaledSocket = "/var/run/tailscale/tailscaled.sock"
	}
	if cfg.MaxLimits == (proto.Limits{}) {
		cfg.MaxLimits = proto.DefaultLimits()
	}
	if cfg.MaxLimits.MaxLogBytes <= 0 || cfg.MaxLimits.MaxRuntimeSec <= 0 || cfg.MaxLimits.MaxWorkspaceBytes <= 0 {
		return nil, fmt.Errorf("runner limit ceilings must be positive")
	}
	if cfg.MaxUploadBytes == 0 {
		cfg.MaxUploadBytes = cfg.MaxLimits.MaxWorkspaceBytes + defaultUploadOverhead
	}
	if cfg.MaxUploadBytes <= cfg.MaxLimits.MaxWorkspaceBytes {
		return nil, fmt.Errorf("max upload bytes must exceed the workspace byte ceiling")
	}
	d := &Daemon{cfg: cfg, jobs: map[string]*Job{}, whoisClient: newWhoisClient(cfg.TailscaledSocket)}
	if err := d.lockStateDir(); err != nil {
		return nil, err
	}
	if err := d.loadExisting(); err != nil {
		_ = d.Close()
		return nil, err
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
		if d.whoisClient != nil {
			d.whoisClient.CloseIdleConnections()
		}
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

func replaceJSON(dest string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(dest, append(b, '\n'))
}

func replaceFile(dest string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".receipt-migration-*")
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

func scrubLegacyPATHReceipts(dir string, result *proto.Result) error {
	if result != nil && strings.Contains(result.StartError, "fork/exec ") {
		result.StartError = "starting process failed"
		if err := replaceJSON(filepath.Join(dir, "result.json"), result); err != nil {
			return fmt.Errorf("redacting legacy result: %w", err)
		}
	}

	eventsPath := filepath.Join(dir, "events.ndjson")
	raw, err := os.ReadFile(eventsPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading legacy events: %w", err)
	}
	var out bytes.Buffer
	changed := false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event proto.Event
		if err := json.Unmarshal(line, &event); err != nil {
			event = proto.Event{Event: "legacy-event-redacted", Detail: "unparseable legacy event removed during receipt migration"}
			changed = true
		} else if event.Event == "start-rejected" && strings.Contains(event.Detail, "fork/exec ") {
			event.Detail = "starting process failed"
			changed = true
		} else {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	if !changed {
		return nil
	}
	if err := replaceFile(eventsPath, out.Bytes()); err != nil {
		return fmt.Errorf("redacting legacy events: %w", err)
	}
	return nil
}

func hasEnvName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// loadExisting restores receipts from disk. A job dir without a result is
// ambiguous: never replayed, reported as such.
func (d *Daemon) loadExisting() error {
	if err := os.MkdirAll(d.jobsDir(), 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(d.jobsDir())
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if !ent.IsDir() || !proto.ValidULID(ent.Name()) {
			continue
		}
		dir := filepath.Join(d.jobsDir(), ent.Name())
		j := &Job{
			ID: ent.Name(), Dir: dir, done: make(chan struct{}),
			logReady: make(chan struct{}),
		}
		close(j.done)
		j.markLogReady()
		specRaw, err := os.ReadFile(filepath.Join(dir, "spec.json"))
		if err != nil {
			j.state = StateAmbiguous
			d.jobs[j.ID] = j
			continue
		}
		var receipt proto.ReceiptSpec
		if err := json.Unmarshal(specRaw, &receipt); err == nil && receipt.ReceiptVersion != 0 &&
			receipt.ReceiptVersion != proto.ReceiptVersion {
			return fmt.Errorf("loading receipt %s: unsupported receipt version %d", ent.Name(), receipt.ReceiptVersion)
		}
		if json.Unmarshal(specRaw, &receipt) == nil &&
			(receipt.ReceiptVersion == proto.ReceiptVersion || receipt.RequestDigest != "") {
			legacy := receipt.ReceiptVersion != proto.ReceiptVersion || receipt.RequestDigest != ""
			receipt.ReceiptVersion = proto.ReceiptVersion
			receipt.RequestDigest = ""
			j.Spec = receipt.SpecWithoutEnv()
			if len(receipt.EnvNames) == 0 {
				j.RequestDigest = j.Spec.Digest()
			}
			if legacy {
				if err := replaceJSON(filepath.Join(dir, "spec.json"), receipt); err != nil {
					return fmt.Errorf("migrating receipt %s: %w", ent.Name(), err)
				}
			}
		} else if json.Unmarshal(specRaw, &j.Spec) == nil {
			legacySpec := j.Spec
			if len(legacySpec.Env) == 0 {
				j.RequestDigest = legacySpec.Digest()
			}
			receipt = proto.NewReceiptSpec(legacySpec)
			if err := replaceJSON(filepath.Join(dir, "spec.json"), receipt); err != nil {
				return fmt.Errorf("migrating legacy receipt %s: %w", ent.Name(), err)
			}
			j.Spec = receipt.SpecWithoutEnv()
		} else {
			j.state = StateAmbiguous
			d.jobs[j.ID] = j
			continue
		}
		if admRaw, err := os.ReadFile(filepath.Join(dir, "admission.json")); err == nil {
			json.Unmarshal(admRaw, &j.Admission)
		}
		if resRaw, err := os.ReadFile(filepath.Join(dir, "result.json")); err == nil {
			var res proto.Result
			if json.Unmarshal(resRaw, &res) == nil {
				j.result = &res
				j.state = res.State
				if j.state != proto.StateExited && j.state != proto.StateKilled && j.state != proto.StateAmbiguous {
					j.state = proto.StateExited
				}
			}
		}
		if executionRaw, err := os.ReadFile(filepath.Join(dir, "execution.json")); err == nil {
			var execution proto.ExecutionContext
			if json.Unmarshal(executionRaw, &execution) == nil &&
				(execution.PATHSHA256 != "" || (hasEnvName(receipt.EnvNames, "PATH") && execution.Path != "")) {
				execution.PATHSHA256 = ""
				if hasEnvName(receipt.EnvNames, "PATH") {
					execution.Path = ""
				}
				if err := replaceJSON(filepath.Join(dir, "execution.json"), execution); err != nil {
					return fmt.Errorf("migrating execution receipt %s: %w", ent.Name(), err)
				}
			}
		}
		if hasEnvName(receipt.EnvNames, "PATH") {
			if err := scrubLegacyPATHReceipts(dir, j.result); err != nil {
				return fmt.Errorf("migrating receipt %s: %w", ent.Name(), err)
			}
		}
		if j.result == nil {
			j.state = StateAmbiguous
			j.event("ambiguous-after-restart", "daemon restarted before a result was recorded; not replayed")
		}
		d.jobs[j.ID] = j
	}
	return nil
}

func (d *Daemon) release(j *Job) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running == j {
		d.running = nil
	}
}

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/info", d.auth("", d.handleInfo))
	mux.HandleFunc("PUT /v0/jobs/{id}", d.auth(proto.ActionSubmit, d.handleSubmit))
	mux.HandleFunc("GET /v0/jobs/{id}", d.auth(proto.ActionReadOwn, d.handleStatus))
	mux.HandleFunc("GET /v0/jobs/{id}/logs", d.auth(proto.ActionReadOwn, d.handleLogs))
	mux.HandleFunc("POST /v0/jobs/{id}/signal", d.auth(proto.ActionKillOwn, d.handleSignal))
	mux.HandleFunc("POST /v0/jobs/{id}/kill", d.auth(proto.ActionKillOwn, d.handleKill))
	return mux
}

type handlerFunc func(http.ResponseWriter, *http.Request, Identity)

// auth resolves the caller and requires the given action ("" means any
// authorization suffices, e.g. for /v0/info). Fail closed.
func (d *Daemon) auth(action string, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localAddr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		id, err := d.identify(r.RemoteAddr, localAddr)
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
	busy := d.running != nil
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, proto.Info{
		Proto:   proto.ProtoVersion,
		Version: d.cfg.Version,
		Busy:    busy,
		Facts:   measureFacts(),
	})
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
	if d.running != nil {
		d.mu.Unlock()
		httpError(w, http.StatusTooManyRequests, "busy: one job at a time in v0")
		return
	}
	dir := filepath.Join(d.jobsDir(), jobID)
	tmpDir, err := os.MkdirTemp(d.jobsDir(), ".admission-"+jobID+"-")
	if err != nil {
		d.mu.Unlock()
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	j := &Job{
		ID: jobID, Dir: tmpDir, Spec: spec, RequestDigest: digest,
		Admission: proto.Admission{
			Time: time.Now(), UserID: id.UserID, UserLogin: id.Login,
			NodeID: id.NodeID, NodeName: id.Node,
			RemoteAddr: r.RemoteAddr, Method: id.Method, Facts: measureFacts(),
		},
		state:       proto.StateStaging,
		done:        make(chan struct{}),
		logReady:    make(chan struct{}),
		stagingDone: make(chan struct{}),
	}
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
	d.running = j
	d.mu.Unlock()

	if err := j.start(d, &stagingUpload{Reader: workspace, body: r.Body}, manifest); err != nil {
		j.event("start-rejected", err.Error())
		if cleanupErr := d.abortAdmission(j, err); cleanupErr != nil {
			httpError(w, http.StatusInternalServerError, errors.Join(err, cleanupErr).Error())
		} else {
			httpError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, j.Status())
}

type stagingUpload struct {
	io.Reader
	body io.ReadCloser
}

func (r *stagingUpload) Close() error { return r.body.Close() }

func (d *Daemon) abortAdmission(j *Job, startErr error) error {
	cleanupErr := removeOwnedTree(j.Dir)
	var receiptWriteErr error
	d.mu.Lock()
	if cleanupErr == nil && d.jobs[j.ID] == j {
		delete(d.jobs, j.ID)
	}
	if d.running == j {
		d.running = nil
	}
	d.mu.Unlock()
	if cleanupErr != nil {
		rollbackErr := fmt.Errorf("cleaning rejected admission: %w", cleanupErr)
		startMessage := "job was rejected before execution"
		if startErr != nil {
			startMessage = startErr.Error()
		}
		result := &proto.Result{
			State: proto.StateAmbiguous, StartError: startMessage,
			TransactionError: rollbackErr.Error(), OutputsOK: true,
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
	if s.V != proto.ProtoVersion {
		return fmt.Errorf("unsupported spec version %d", s.V)
	}
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return fmt.Errorf("spec has empty argv")
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
	l := s.Limits
	if l.MaxLogBytes <= 0 || l.MaxRuntimeSec <= 0 || l.MaxWorkspaceBytes <= 0 {
		return fmt.Errorf("spec limits must be positive")
	}
	if l.MaxLogBytes > maxLimits.MaxLogBytes ||
		l.MaxRuntimeSec > maxLimits.MaxRuntimeSec ||
		l.MaxWorkspaceBytes > maxLimits.MaxWorkspaceBytes {
		return fmt.Errorf("spec limits exceed this runner's ceiling")
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

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request, id Identity) {
	if j := d.lookup(w, r, id); j != nil {
		writeJSON(w, http.StatusOK, j.Status())
	}
}

// handleLogs streams framed log events over SSE, resumable via ?from= or
// Last-Event-ID, and terminates with a "status" event.
func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request, id Identity) {
	j := d.lookup(w, r, id)
	if j == nil {
		return
	}
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
	if err := j.Signal(sig); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "signaled"})
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
	if err := j.terminate("user-kill", sig); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "killed"})
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
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
