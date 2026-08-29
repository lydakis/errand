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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/logio"
	"github.com/lydakis/errand/internal/proto"
)

// Job is one admitted transaction. Its directory is the receipt.
type Job struct {
	ID            string
	Dir           string
	Spec          proto.Spec
	Admission     proto.Admission
	RequestDigest string

	mu            sync.Mutex
	state         string
	result        *proto.Result
	cmd           *exec.Cmd
	logw          *logio.Writer
	scope         *processScope
	killed        string // limit name or signal that terminated the job, if any
	killSignal    syscall.Signal
	started       bool
	reaped        bool
	startRejected bool
	done          chan struct{}
	logReady      chan struct{}
	logReadyOnce  sync.Once
	stagingCancel func()
	stagingDone   chan struct{}
	stagingOnce   sync.Once
}

func (j *Job) Status() proto.JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return proto.JobStatus{ID: j.ID, State: j.state, Result: j.result}
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

// start extracts the workspace and launches the process. Pre-execution
// failures are returned so admission can be rolled back safely; the wait and
// terminal finalize happen in a goroutine after a successful launch.
func (j *Job) start(d *Daemon, workspaceTar io.ReadCloser, manifest proto.Manifest) (retErr error) {
	j.event("admitted", j.Admission.Method)
	stagingCtx, cancelStaging := j.beginStaging(workspaceTar)
	defer cancelStaging()
	defer j.markStagingDone()
	defer func() {
		if retErr == nil {
			return
		}
		if res := j.settleStartFailure(); res != nil {
			j.finalize(d, res, true)
			retErr = nil
		}
	}()

	workspace := filepath.Join(j.Dir, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		return err
	}
	extractErr := archive.Extract(&contextReader{ctx: stagingCtx, r: workspaceTar}, workspace, manifest, j.Spec.Limits.MaxWorkspaceBytes)
	j.markStagingDone()
	if extractErr != nil {
		if res := j.cancelledBeforeStart(); res != nil {
			j.finalize(d, res, true)
			return nil
		}
		return extractErr
	}
	if err := os.Mkdir(filepath.Join(j.Dir, "out"), 0o700); err != nil {
		return err
	}
	j.event("workspace-extracted", manifest.RootHash())
	if res := j.cancelledBeforeStart(); res != nil {
		j.finalize(d, res, true)
		return nil
	}

	var logw *logio.Writer
	logw, err := logio.NewWriter(filepath.Join(j.Dir, "io.log"), j.Spec.Limits.MaxLogBytes, func() {
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
	err = cmd.Start()
	if err == nil {
		j.cmd = cmd
		j.started = true
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
		timer.Stop()
		j.mu.Lock()
		j.cmd = nil
		j.reaped = true
		j.mu.Unlock()

		scopeErr := scope.cleanup(2 * time.Second)
		processCleanupOK := scopeErr == nil
		if scopeErr != nil {
			j.event("process-cleanup-failed", scopeErr.Error())
		}
		pipeErr := waitForPipeCopies([]*os.File{stdoutR, stderrR}, pipeErrs, 2*time.Second)
		if pipeErr != nil {
			j.event("log-pipe-drain-failed", pipeErr.Error())
		}
		logw.Close()

		res := &proto.Result{Started: true, OutputsOK: true, CleanupOK: processCleanupOK && pipeErr == nil}
		res.LogsComplete = logw.Complete() && pipeErr == nil
		res.LimitExceeded = j.limitExceeded(logw)
		if err := errors.Join(logw.Err(), pipeErr); err != nil {
			res.TransactionError = "persisting logs: " + err.Error()
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
		j.finalize(d, res, false)
	}()
	return nil
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
			OutputsOK: true, LogsComplete: true,
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
		OutputsOK: true, LogsComplete: true,
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
	if j.result != nil || j.reaped || j.startRejected || j.state == proto.StateExited ||
		j.state == proto.StateKilled || j.state == proto.StateAmbiguous {
		j.mu.Unlock()
		return fmt.Errorf("job %s is not running", j.ID)
	}
	cmd := j.cmd
	scope := j.scope
	stagingCancel := j.stagingCancel
	stagingDone := j.stagingDone
	staging := j.state == proto.StateStaging && cmd == nil
	if j.killed == "" {
		j.killed = reason
		j.killSignal = sig
	}
	j.mu.Unlock()
	var groupErr, scopeErr, stagingErr error
	if staging {
		if stagingCancel != nil {
			stagingCancel()
		}
		if stagingDone != nil {
			select {
			case <-stagingDone:
			case <-time.After(5 * time.Second):
				stagingErr = fmt.Errorf("timed out stopping job %s staging", j.ID)
			}
		}
	}
	if cmd != nil && cmd.Process != nil {
		groupErr = syscall.Kill(-cmd.Process.Pid, sig)
		if groupErr == syscall.ESRCH {
			groupErr = nil
		}
		if scope != nil {
			scopeErr = scope.signalEscaped(sig, cmd.Process.Pid)
		}
	}
	j.event("terminated", reason)
	return errors.Join(groupErr, scopeErr, stagingErr)
}

// Signal forwards a signal to the job's process group.
func (j *Job) Signal(sig syscall.Signal) error {
	j.mu.Lock()
	cmd := j.cmd
	scope := j.scope
	running := j.state == proto.StateRunning
	j.mu.Unlock()
	if !running || cmd == nil || cmd.Process == nil {
		return fmt.Errorf("job %s is not running", j.ID)
	}
	j.event("signal", sig.String())
	groupErr := syscall.Kill(-cmd.Process.Pid, sig)
	if groupErr == syscall.ESRCH {
		groupErr = nil
	}
	var scopeErr error
	if scope != nil {
		scopeErr = scope.signalEscaped(sig, cmd.Process.Pid)
	}
	return errors.Join(groupErr, scopeErr)
}

// finalize writes the once-only result, cleans up the workspace, and releases
// the runner slot. A persistence failure publishes an in-memory ambiguous
// result rather than claiming durable terminal success.
func (j *Job) finalize(d *Daemon, res *proto.Result, neverRan bool) {
	workspaceCleanupOK := removeOwnedTree(filepath.Join(j.Dir, "workspace")) == nil
	if neverRan {
		res.CleanupOK = workspaceCleanupOK
	} else {
		res.CleanupOK = res.CleanupOK && workspaceCleanupOK
	}
	j.markLogReady()

	state := proto.StateExited
	j.mu.Lock()
	if res.Signal != "" && j.killed != "" {
		state = proto.StateKilled
	}
	j.mu.Unlock()
	res.State = state

	if err := j.writeJSON("result.json", res); err != nil {
		j.event("result-write-failed", err.Error())
		res.TransactionError = "persisting result: " + err.Error()
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
