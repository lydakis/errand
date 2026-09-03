package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/lydakis/errand/internal/archive"
	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/logio"
	"github.com/lydakis/errand/internal/proto"
)

const (
	maxListCommandBytes    = 512
	maxListWorkdirBytes    = 384
	maxListProjectBytes    = 128
	maxListDigestBytes     = 64
	maxListResponseBytes   = 1 << 20
	maxDetailResponseBytes = 1 << 20
	maxReceiptSpecBytes    = 256 << 10
	maxChangePreviewPaths  = 256
	maxChangePreviewBytes  = 32 << 10
	queuedMarkerName       = "queued.json"
)

type queuedRecord struct {
	State string `json:"state"`
}

// Job is one admitted transaction. Its directory is the receipt.
type Job struct {
	ID            string
	Dir           string
	Spec          proto.Spec
	Admission     proto.Admission
	RequestDigest string
	baseline      proto.Manifest

	mu                  sync.Mutex
	state               string
	result              *proto.Result
	cmd                 *exec.Cmd
	logw                *logio.Writer
	scope               *processScope
	killed              string // limit name or signal that terminated the job, if any
	killSignal          syscall.Signal
	started             bool
	startedAt           time.Time
	reaped              bool
	startRejected       bool
	deleting            bool
	logReaders          int
	changeReaders       int
	forwardReaders      int
	done                chan struct{}
	logReady            chan struct{}
	logReadyOnce        sync.Once
	executionDone       chan struct{}
	executionDoneOnce   sync.Once
	endpoint            JobEndpoint
	stagingCancel       func()
	stagingDone         chan struct{}
	stagingOnce         sync.Once
	changeCancel        context.CancelFunc
	changeCancelRequest string
}

func newJob(id, dir string) *Job {
	return &Job{
		ID: id, Dir: dir,
		done:          make(chan struct{}),
		logReady:      make(chan struct{}),
		executionDone: make(chan struct{}),
		endpoint:      hostJobEndpoint{},
		stagingDone:   make(chan struct{}),
	}
}

func (j *Job) Status() proto.JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.statusLocked()
}

func (j *Job) statusLocked() proto.JobStatus {
	return proto.JobStatus{ID: j.ID, State: j.state, Result: j.result}
}

func (j *Job) Details() proto.JobDetails {
	j.mu.Lock()
	defer j.mu.Unlock()
	details := proto.JobDetails{
		JobStatus:  j.statusLocked(),
		Spec:       proto.NewReceiptSpec(j.Spec),
		AdmittedAt: j.Admission.Time,
		Project:    j.Admission.Project,
	}
	if !j.startedAt.IsZero() {
		startedAt := j.startedAt
		details.StartedAt = &startedAt
		elapsed := time.Since(startedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		details.DurationMS = elapsed.Milliseconds()
	}
	if j.result != nil {
		details.StartedAt = j.result.StartedAt
		details.DurationMS = j.result.DurationMS
	}
	return details
}

func summarizeChangeBundle(bundle proto.ChangeBundle) *proto.ChangeSummary {
	previewCapacity := len(bundle.Paths)
	if previewCapacity > maxChangePreviewPaths {
		previewCapacity = maxChangePreviewPaths
	}
	paths := make([]string, 0, previewCapacity)
	used := 0
	for _, changePath := range bundle.Paths {
		if len(paths) >= maxChangePreviewPaths || len(changePath) > maxChangePreviewBytes-used {
			break
		}
		paths = append(paths, changePath)
		used += len(changePath)
	}
	return &proto.ChangeSummary{
		Paths: paths, PathsTruncated: len(paths) != len(bundle.Paths), PathCount: len(bundle.Paths),
		BundleRoot: bundle.RootHash(), Bytes: bundle.Bytes,
	}
}

// summary is the job's listing row. Spec and result fields are read under
// the job lock because launch mutates Spec.Env after admission.
func (j *Job) summary() proto.JobListEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	command, truncated := boundedCommand(j.Spec.Argv, maxListCommandBytes)
	manifestRoot, manifestRootTruncated := boundedListField(j.Spec.ManifestRoot, maxListDigestBytes)
	gitCommit, gitCommitTruncated := boundedListField(j.Spec.GitCommit, maxListDigestBytes)
	workdir, workdirTruncated := boundedListField(j.Spec.Workdir, maxListWorkdirBytes)
	project, projectTruncated := boundedListField(j.Admission.Project, maxListProjectBytes)
	projectTruncated = projectTruncated || j.Admission.ProjectTruncated
	e := proto.JobListEntry{
		ID: j.ID, State: j.state, Command: command, CommandTruncated: truncated,
		AdmittedAt:   j.Admission.Time,
		ManifestRoot: manifestRoot, ManifestRootTruncated: manifestRootTruncated,
		GitCommit: gitCommit, GitCommitTruncated: gitCommitTruncated, GitDirty: j.Spec.GitDirty,
		Workdir: workdir, WorkdirTruncated: workdirTruncated,
		Project: project, ProjectTruncated: projectTruncated,
	}
	if !j.startedAt.IsZero() {
		startedAt := j.startedAt
		e.StartedAt = &startedAt
		elapsed := time.Since(j.startedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		e.DurationMS = elapsed.Milliseconds()
	}
	if j.result != nil {
		e.ExitCode = j.result.ExitCode
		e.Signal = j.result.Signal
		if j.result.StartedAt != nil {
			e.StartedAt = j.result.StartedAt
			e.DurationMS = j.result.DurationMS
		}
		e.FinishedAt = j.result.FinishedAt
	}
	return e
}

func boundedListField(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	return markCommandTruncated(value, limit), true
}

func boundedCommand(argv []string, limit int) (string, bool) {
	if limit <= 0 {
		return "", len(argv) > 0
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = strconv.Quote(arg)
	}
	command := strings.Join(quoted, " ")
	if len(command) > limit {
		return markCommandTruncated(command, limit), true
	}
	return command, false
}

func markCommandTruncated(s string, limit int) string {
	const marker = "…"
	budget := limit - len(marker)
	if budget < 0 {
		return ""
	}
	if len(s) > budget {
		s = s[:budget]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s + marker
}

func (j *Job) event(name, detail string) {
	e := proto.Event{TUnixMS: time.Now().UnixMilli(), Event: name, Detail: detail}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(filepath.Join(j.Dir, "events.ndjson"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

func (j *Job) writeJSON(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dest := filepath.Join(j.Dir, name)
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(j.Dir, "."+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
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

func (j *Job) markLogReady() {
	j.logReadyOnce.Do(func() {
		if j.logReady != nil {
			close(j.logReady)
		}
	})
}

func (j *Job) acquireLogReader() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.deleting {
		return false
	}
	j.logReaders++
	return true
}

func (j *Job) releaseLogReader() {
	j.mu.Lock()
	j.logReaders--
	j.mu.Unlock()
}

func (j *Job) acquireChangeReader() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.deleting {
		return false
	}
	j.changeReaders++
	return true
}

func (j *Job) releaseChangeReader() {
	j.mu.Lock()
	j.changeReaders--
	j.mu.Unlock()
}

func (j *Job) acquireForwardReader() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.deleting {
		return false
	}
	j.forwardReaders++
	return true
}

func (j *Job) releaseForwardReader() {
	j.mu.Lock()
	j.forwardReaders--
	j.mu.Unlock()
}

func (j *Job) markExecutionDone() {
	j.executionDoneOnce.Do(func() {
		if j.executionDone != nil {
			close(j.executionDone)
		}
	})
}

// stage extracts the workspace. settled means a concurrent kill finalized it.
func (j *Job) stage(d *Daemon, workspaceTar io.ReadCloser, manifest proto.Manifest) (settled bool, retErr error) {
	j.event("admitted", j.Admission.Method)
	stagingCtx, cancelStaging := j.beginStaging(workspaceTar)
	defer cancelStaging()
	defer j.markStagingDone()

	workspace := filepath.Join(j.Dir, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		return false, err
	}
	cachedPaths := map[string]bool{}
	extractOpts := archive.ExtractOptions{}
	if d.cache != nil {
		extractOpts.ResolveMissing = func(dest string, entry proto.ManifestEntry) (bool, error) {
			hit, err := d.cache.Materialize(stagingCtx, dest, entry)
			if hit {
				cachedPaths[entry.Path] = true
			}
			return hit, err
		}
	}
	extractErr := archive.ExtractWith(&contextReader{ctx: stagingCtx, r: workspaceTar}, workspace, manifest, j.Spec.Limits.MaxWorkspaceBytes, extractOpts)
	if extractErr != nil {
		j.markStagingDone()
		if res := j.cancelledBeforeStart(); res != nil {
			j.finalize(d, res, true)
			return true, nil
		}
		return false, extractErr
	}
	totalFiles := 0
	for _, e := range manifest.Entries {
		if e.Type == proto.EntryFile {
			totalFiles++
		}
	}
	j.event("workspace-extracted", fmt.Sprintf("root=%s cached=%d/%d", manifest.RootHash(), len(cachedPaths), totalFiles))
	if err := changeops.CaptureWorkspaceBaseContext(stagingCtx, workspace, j.Dir, manifest); err != nil {
		return false, fmt.Errorf("capturing submitted workspace for change merging: %w", err)
	}
	j.event("change-base-captured", manifest.RootHash())
	if d.cache != nil {
		for _, e := range manifest.Entries {
			if e.Type != proto.EntryFile || cachedPaths[e.Path] {
				continue
			}
			src := filepath.Join(workspace, filepath.FromSlash(e.Path))
			if err := d.cache.Insert(stagingCtx, src, e.SHA256, e.Size); err != nil {
				if stagingCtx.Err() == nil {
					j.event("cache-insert-failed", e.Path+": "+err.Error())
				}
				break
			}
		}
	}
	j.markStagingDone()
	if res := j.cancelledBeforeStart(); res != nil {
		j.finalize(d, res, true)
		return true, nil
	}
	return false, nil
}

// launch runs a staged job: log writer, process scope, exec, and the wait
// goroutine that finalizes. The scheduler settles any launch error durably.
func (j *Job) launch(d *Daemon) error {
	workspace := filepath.Join(j.Dir, "workspace")
	var (
		logw *logio.Writer
		err  error
	)
	logw, err = logio.NewWriter(filepath.Join(j.Dir, "io.log"), j.Spec.Limits.MaxLogBytes, func() {
		reason := "log_bytes"
		if logw != nil && logw.Err() != nil {
			reason = "log_io"
		}
		_ = j.terminate(reason, syscall.SIGKILL)
	})
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.logw = logw
	j.mu.Unlock()
	j.markLogReady()

	workdir := workspace
	if j.Spec.Workdir != "" {
		wd := filepath.Clean(j.Spec.Workdir)
		if filepath.IsAbs(wd) || wd == ".." || strings.HasPrefix(wd, ".."+string(filepath.Separator)) {
			logw.Close()
			return fmt.Errorf("unsafe workdir %q", j.Spec.Workdir)
		}
		workdir = filepath.Join(workspace, wd)
	}

	jobEnv := j.buildEnv()
	scope, err := newProcessScope(workspace)
	if err != nil {
		logw.Close()
		return err
	}
	// Persisted before the process can exist, so a restarted daemon never
	// faces a started job it cannot find during reconciliation.
	if err := j.writeJSON("scope.json", scopeRecord{Token: scope.token}); err != nil {
		logw.Close()
		return fmt.Errorf("persisting process scope: %w", err)
	}
	if err := removeQueuedMarker(j.Dir); err != nil {
		logw.Close()
		return fmt.Errorf("consuming queued marker: %w", err)
	}
	jobEnv = append(jobEnv, scope.env())
	executable, err := resolveExecutable(j.Spec.Argv[0], envValue(jobEnv, "PATH"), workdir)
	if err != nil {
		logw.Close()
		return err
	}
	cmd := exec.Command(executable, j.Spec.Argv[1:]...)
	cmd.Args[0] = j.Spec.Argv[0]
	cmd.Dir = workdir
	cmd.Env = jobEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	execution := proto.ExecutionContext{Argv: j.Spec.Argv}
	if _, declaredPATH := j.Spec.Env["PATH"]; !declaredPATH {
		execution.Path = cmd.Path
	}
	if err := j.writeJSON("execution.json", execution); err != nil {
		logw.Close()
		return fmt.Errorf("writing execution context: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		logw.Close()
		return fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		logw.Close()
		return fmt.Errorf("creating stderr pipe: %w", err)
	}
	closePipes := func() {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	j.mu.Lock()
	j.scope = scope
	if j.killed != "" {
		j.mu.Unlock()
		closePipes()
		logw.Close()
		j.finalize(d, j.cancelledBeforeStart(), true)
		return nil
	}
	var startedAt time.Time
	err = cmd.Start()
	if err == nil {
		startedAt = time.Now()
		j.cmd = cmd
		j.started = true
		j.startedAt = startedAt
		j.state = proto.StateRunning
		j.Spec.Env = nil // values now belong to the child; retain only the digest and metadata
	}
	j.mu.Unlock()
	if err != nil {
		closePipes()
		logw.Close()
		return sanitizeProcessStartError(err)
	}
	stdoutW.Close()
	stderrW.Close()
	pipeErrs := make(chan error, 2)
	copyPipe := func(r *os.File, stream string) {
		_, copyErr := io.Copy(logw.StreamWriter(stream), r)
		r.Close()
		pipeErrs <- copyErr
	}
	go copyPipe(stdoutR, "stdout")
	go copyPipe(stderrR, "stderr")
	j.event("started", fmt.Sprintf("pid=%d", cmd.Process.Pid))

	timer := time.AfterFunc(time.Duration(j.Spec.Limits.MaxRuntimeSec)*time.Second, func() {
		j.terminate("runtime", syscall.SIGKILL)
	})

	go func() {
		waitErr := cmd.Wait()
		j.markExecutionDone()
		finishedAt := time.Now()
		durationMS := finishedAt.Sub(startedAt).Milliseconds()
		changeCtx, changeCancel := changeCollectionContext(finishedAt)
		timer.Stop()
		j.transitionAfterProcessExit(changeCancel)

		scopeKilled, scopeErr := scope.cleanup(2 * time.Second)
		processCleanupOK := scopeErr == nil
		if len(scopeKilled) > 0 {
			j.event("scope-killed", fmt.Sprintf("pids=%v", scopeKilled))
		}
		if scopeErr != nil {
			j.event("process-cleanup-failed", scopeErr.Error())
		}
		pipeErr := waitForPipeCopies([]*os.File{stdoutR, stderrR}, pipeErrs, 2*time.Second)
		if pipeErr != nil {
			j.event("log-pipe-drain-failed", pipeErr.Error())
		}
		logw.Close()

		res := &proto.Result{
			Started: true, StartedAt: &startedAt, FinishedAt: &finishedAt, DurationMS: durationMS,
			ChangesOK: true, CleanupOK: processCleanupOK && pipeErr == nil,
		}
		res.LogsComplete = logw.Complete() && pipeErr == nil
		res.LimitExceeded = j.limitExceeded(logw)
		if scopeErr != nil {
			res.TransactionError = appendTransactionError(res.TransactionError, "process scope cleanup: "+scopeErr.Error())
		}
		if err := errors.Join(logw.Err(), pipeErr); err != nil {
			res.TransactionError = appendTransactionError(res.TransactionError, "persisting logs: "+err.Error())
		}

		if waitErr == nil {
			code := 0
			res.ExitCode = &code
		} else if ee, ok := waitErr.(*exec.ExitError); ok {
			ws := ee.Sys().(syscall.WaitStatus)
			if ws.Signaled() {
				res.Signal = ws.Signal().String()
				res.SignalNum = int(ws.Signal())
			} else {
				code := ws.ExitStatus()
				res.ExitCode = &code
			}
		} else {
			res.StartError = waitErr.Error()
		}

		{
			var bundle proto.ChangeBundle
			var collected bool
			var collectErr error
			if !processCleanupOK {
				collectErr = errors.New("process scope cleanup incomplete")
			} else {
				bundle, collected, collectErr = changeops.CollectWorkspaceChangesContext(
					changeCtx, workspace, j.Dir, j.baseline, j.Spec.Selection, j.Spec.Limits.MaxChangeBytes)
			}
			j.mu.Lock()
			ctxErr := changeCtx.Err()
			j.changeCancel = nil
			j.mu.Unlock()
			changeCancel()
			collected, collectErr = settleChangeCollection(j.Dir, collected, collectErr, ctxErr)
			if collectErr != nil {
				res.ChangesOK = false
				if res.LimitExceeded == "" {
					res.LimitExceeded = changeCollectionLimit(collectErr)
				}
				res.TransactionError = appendTransactionError(res.TransactionError,
					"collecting changes: "+collectErr.Error())
				j.event("change-collection-failed", collectErr.Error())
			} else if collected {
				res.Changes = summarizeChangeBundle(bundle)
				j.event("workspace-changes-retained", fmt.Sprintf("root=%s paths=%d bytes=%d",
					res.Changes.BundleRoot, len(bundle.Paths), bundle.Bytes))
			}
		}
		j.finalizeWithScopeOutcome(d, res, false, processCleanupOK)
	}()
	return nil
}

func settleChangeCollection(jobDir string, collected bool, collectErr, ctxErr error) (bool, error) {
	if collected {
		// Publication is the collection commit point. Later cleanup or deadline
		// errors must not destroy an already durable bundle.
		return true, nil
	}
	collectErr = errors.Join(collectErr, ctxErr)
	return collected, collectErr
}

func changeCollectionLimit(err error) string {
	switch {
	case errors.Is(err, changeops.ErrEntryLimitExceeded):
		return "change_entries"
	case errors.Is(err, changeops.ErrByteLimitExceeded), errors.Is(err, changeops.ErrLimitExceeded):
		return "change_bytes"
	case errors.Is(err, context.DeadlineExceeded):
		return "change_deadline"
	default:
		return ""
	}
}

// settleStartFailure closes the race between start returning an error and
// admission rollback. A kill already accepted becomes a durable terminal
// result; otherwise future control requests must reject the doomed job.
func (j *Job) settleStartFailure() *proto.Result {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.started && j.killed != "" {
		return &proto.Result{
			Signal: j.killSignal.String(), SignalNum: int(j.killSignal),
			ChangesOK: true, LogsComplete: true,
		}
	}
	j.startRejected = true
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	return n, err
}

func (j *Job) beginStaging(workspaceTar io.ReadCloser) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	cancelRead := func() {
		cancel()
		_ = workspaceTar.Close()
	}
	j.mu.Lock()
	if j.stagingDone == nil {
		j.stagingDone = make(chan struct{})
	}
	j.stagingCancel = cancelRead
	alreadyKilled := j.killed != ""
	j.mu.Unlock()
	if alreadyKilled {
		cancelRead()
	}
	return ctx, cancel
}

func (j *Job) markStagingDone() {
	j.stagingOnce.Do(func() {
		j.mu.Lock()
		j.stagingCancel = nil
		done := j.stagingDone
		j.mu.Unlock()
		if done != nil {
			close(done)
		}
	})
}

func sanitizeProcessStartError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("starting process: %v", pathErr.Err)
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return fmt.Errorf("starting process: %v", execErr.Err)
	}
	return errors.New("starting process failed")
}

func (j *Job) limitExceeded(logw *logio.Writer) string {
	// The writer owns the authoritative log-cap state. Its asynchronous
	// termination callback can lose the race with process reaping.
	if logw != nil && logw.LimitHit() {
		return "log_bytes"
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	switch j.killed {
	case "log_bytes", "runtime":
		return j.killed
	default:
		return ""
	}
}

const changeCollectionTimeout = 5 * time.Minute

func changeCollectionContext(finishedAt time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), finishedAt.Add(changeCollectionTimeout))
}

func waitForPipeCopies(readers []*os.File, errs <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var joined error
	for completed := 0; completed < len(readers); {
		select {
		case err := <-errs:
			completed++
			if err != nil && !errors.Is(err, os.ErrClosed) {
				joined = errors.Join(joined, err)
			}
		case <-timer.C:
			for _, r := range readers {
				_ = r.Close()
			}
			return errors.Join(joined, fmt.Errorf("timed out draining inherited log pipes"))
		}
	}
	return joined
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix)
		}
	}
	return ""
}

func resolveExecutable(name, pathEnv, workdir string) (string, error) {
	check := func(candidate string) (string, bool) {
		fi, err := os.Stat(candidate)
		return candidate, err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
	}
	if strings.ContainsRune(name, filepath.Separator) {
		candidate := name
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(workdir, candidate)
		}
		if resolved, ok := check(candidate); ok {
			return resolved, nil
		}
		return "", fmt.Errorf("executable %q not found or not executable", name)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = workdir
		} else if !filepath.IsAbs(dir) {
			dir = filepath.Join(workdir, dir)
		}
		if resolved, ok := check(filepath.Join(dir, name)); ok {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in effective PATH", name)
}

func (j *Job) cancelledBeforeStart() *proto.Result {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.started || j.killed == "" {
		return nil
	}
	return &proto.Result{
		Signal: j.killSignal.String(), SignalNum: int(j.killSignal),
		ChangesOK: true, LogsComplete: true,
	}
}

// buildEnv composes the job environment: a small allowlist from the
// daemon's own environment, then declared values, then errand's own
// variables. Nothing ambient is forwarded from the caller.
func (j *Job) buildEnv() []string {
	var env []string
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	for k, v := range j.Spec.Env {
		env = append(env, k+"="+v)
	}
	env = append(env, "ERRAND_JOB_ID="+j.ID, "TERM=dumb")
	if j.Spec.GitCommit != "" {
		env = append(env, "ERRAND_GIT_COMMIT="+j.Spec.GitCommit)
		env = append(env, fmt.Sprintf("ERRAND_GIT_DIRTY=%v", j.Spec.GitDirty))
	}
	return env
}

// terminate kills the whole process group, recording why.
func (j *Job) terminate(reason string, sig syscall.Signal) error {
	j.mu.Lock()
	if j.result == nil && j.reaped && j.changeCancel != nil {
		if reason == "runtime" {
			j.mu.Unlock()
			return nil
		}
		j.changeCancel()
		j.mu.Unlock()
		j.event("change-collection-cancelled", reason)
		return nil
	}
	if j.result != nil || j.reaped || j.startRejected || j.state == proto.StateExited ||
		j.state == proto.StateKilled || j.state == proto.StateAmbiguous {
		j.mu.Unlock()
		return fmt.Errorf("job %s is not running", j.ID)
	}
	cmd := j.cmd
	scope := j.scope
	if !j.started || cmd == nil || cmd.Process == nil {
		j.mu.Unlock()
		return fmt.Errorf("job %s is not running", j.ID)
	}
	if j.killed == "" {
		j.killed = reason
		j.killSignal = sig
	}
	j.mu.Unlock()
	groupErr := syscall.Kill(-cmd.Process.Pid, sig)
	processExited := groupErr == syscall.ESRCH
	if processExited {
		groupErr = nil
	}
	var scopeErr error
	if scope != nil {
		scopeErr = scope.signalEscaped(sig, cmd.Process.Pid)
	}
	if processExited {
		j.requestChangeCollectionCancellation(reason)
	}
	j.event("terminated", reason)
	return errors.Join(groupErr, scopeErr)
}

// Signal forwards a signal to the job's process group.
func (j *Job) Signal(sig syscall.Signal) error {
	j.mu.Lock()
	if j.result == nil && j.reaped && j.changeCancel != nil {
		j.changeCancel()
		j.mu.Unlock()
		j.event("change-collection-cancelled", sig.String())
		return nil
	}
	if j.result != nil || j.reaped || j.startRejected || j.state == proto.StateExited ||
		j.state == proto.StateKilled || j.state == proto.StateAmbiguous {
		j.mu.Unlock()
		return fmt.Errorf("job %s is not running", j.ID)
	}
	cmd := j.cmd
	scope := j.scope
	running := j.state == proto.StateRunning
	j.mu.Unlock()
	if !running || cmd == nil || cmd.Process == nil {
		return fmt.Errorf("job %s is not running", j.ID)
	}
	j.event("signal", sig.String())
	groupErr := syscall.Kill(-cmd.Process.Pid, sig)
	processExited := groupErr == syscall.ESRCH
	if processExited {
		groupErr = nil
	}
	var scopeErr error
	if scope != nil {
		scopeErr = scope.signalEscaped(sig, cmd.Process.Pid)
	}
	if processExited {
		j.requestChangeCollectionCancellation(sig.String())
	}
	return errors.Join(groupErr, scopeErr)
}

func (j *Job) requestChangeCollectionCancellation(reason string) bool {
	if reason == "runtime" {
		return false
	}
	j.mu.Lock()
	if j.result != nil {
		j.mu.Unlock()
		return false
	}
	if !j.reaped {
		if j.changeCancelRequest == "" {
			j.changeCancelRequest = reason
		}
		j.mu.Unlock()
		return true
	}
	cancel := j.changeCancel
	j.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	j.event("change-collection-cancelled", reason)
	return true
}

func (j *Job) transitionAfterProcessExit(changeCancel context.CancelFunc) {
	j.mu.Lock()
	j.cmd = nil
	j.reaped = true
	j.changeCancel = changeCancel
	reason := j.changeCancelRequest
	j.changeCancelRequest = ""
	j.mu.Unlock()
	if changeCancel != nil && reason != "" {
		changeCancel()
		j.event("change-collection-cancelled", reason)
	}
}

// finalize writes the once-only result, cleans up the workspace, and releases
// the runner slot. A persistence failure publishes an in-memory ambiguous
// result rather than claiming durable terminal success.
func (j *Job) finalize(d *Daemon, res *proto.Result, neverRan bool) {
	j.finalizeWithScopeOutcome(d, res, neverRan, true)
}

func (j *Job) finalizeWithScopeOutcome(d *Daemon, res *proto.Result, neverRan, scopeCleanupOK bool) {
	j.markExecutionDone()
	// Runtime values are no longer needed once settlement begins. The request
	// digest and redacted receipt retain idempotency without retaining secrets.
	j.mu.Lock()
	j.Spec.Env = nil
	j.baseline = proto.Manifest{}
	j.mu.Unlock()

	var workspaceErr error
	if scopeCleanupOK {
		workspaceErr = removeOwnedTree(filepath.Join(j.Dir, "workspace"))
	} else {
		res.TransactionError = appendTransactionError(res.TransactionError, "workspace retained for process recovery")
	}
	baseErr := removeOwnedTree(filepath.Join(j.Dir, "change-base"))
	// The scope record is runtime state, not receipt: once the job is
	// settled there is nothing left for reconciliation to find. Retain it
	// after failed scope cleanup so a restart can still locate survivors.
	var scopeRecordErr error
	if scopeCleanupOK && workspaceErr == nil {
		scopeRecordErr = removeScopeRecord(filepath.Join(j.Dir, "scope.json"))
	} else {
		res.TransactionError = appendTransactionError(res.TransactionError, "process scope cleanup incomplete; recovery record retained")
	}
	if workspaceErr != nil {
		j.event("workspace-remove-failed", workspaceErr.Error())
		res.TransactionError = appendTransactionError(res.TransactionError, "removing workspace: "+workspaceErr.Error())
	}
	if baseErr != nil {
		j.event("change-base-remove-failed", baseErr.Error())
		res.TransactionError = appendTransactionError(res.TransactionError, "removing submitted change base: "+baseErr.Error())
	}
	if scopeRecordErr != nil {
		j.event("scope-record-remove-failed", scopeRecordErr.Error())
		res.TransactionError = appendTransactionError(res.TransactionError, "removing process scope record: "+scopeRecordErr.Error())
	}
	// A queued marker on a never-started job is receipt evidence. Retaining it
	// closes the crash gap between cleanup and the durable terminal result.
	cleanupOK := workspaceErr == nil && baseErr == nil && scopeCleanupOK && scopeRecordErr == nil
	if neverRan {
		res.CleanupOK = cleanupOK
	} else {
		res.CleanupOK = res.CleanupOK && cleanupOK
	}
	j.markLogReady()

	state := proto.StateExited
	j.mu.Lock()
	if res.Signal != "" && j.killed != "" {
		state = proto.StateKilled
	}
	j.mu.Unlock()
	res.State = state
	if res.SettledAt == nil {
		settledAt := time.Now()
		res.SettledAt = &settledAt
	}

	if err := j.writeJSON("result.json", res); err != nil {
		j.event("result-write-failed", err.Error())
		res.TransactionError = appendTransactionError(res.TransactionError, "persisting result: "+err.Error())
		state = proto.StateAmbiguous
		res.State = state
	}
	j.event("finished", state)

	j.mu.Lock()
	j.state = state
	j.result = res
	j.mu.Unlock()
	close(j.done)
	d.release(j)
}

func removeScopeRecord(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func removeQueuedMarker(dir string) error {
	err := os.Remove(filepath.Join(dir, queuedMarkerName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func appendTransactionError(existing, detail string) string {
	if existing == "" {
		return detail
	}
	if detail == "" {
		return existing
	}
	return existing + "; " + detail
}

// removeOwnedTree restores owner traversal permissions on directories before
// removing a tree created and exclusively owned by errand. WalkDir invokes the
// callback before reading a directory, so even mode 000 directories can be
// made traversable without following symlinks.
func removeOwnedTree(root string) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode&0o700 == 0o700 {
			return nil
		}
		return os.Chmod(path, mode|0o700)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}
