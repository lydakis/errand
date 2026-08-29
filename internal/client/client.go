// Package client implements the delegating side: snapshot, submit,
// stream, and translate the transaction into a faithful local exit code.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// ExitTransaction is the errand-level failure exit code: the transaction
// did not complete faithfully, whatever the remote process did.
const ExitTransaction = 120

const (
	controlRequestTimeout = 15 * time.Second
	submitRequestTimeout  = 31 * time.Minute
	streamIdleTimeout     = 2 * time.Minute
	streamDeadlineMargin  = 5 * time.Minute
)

var directTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ResponseHeaderTimeout = controlRequestTimeout
	return t
}()

var directHTTP = &http.Client{
	Transport: directTransport,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type RunOptions struct {
	PeerURL    string
	PeerName   string // config alias for handle printing; "" falls back to the host
	Root       string
	Argv       []string
	Env        map[string]string // literal values
	PassEnv    []string          // names copied from the local environment
	Workdir    string
	IncludeAll bool
	Detach     bool // return after admission, printing the handle on stdout
	Stdout     io.Writer
	Stderr     io.Writer
}

// Run performs one job transaction and returns the CLI exit code per the
// two-layer rule: transaction success mirrors the remote process; a transaction
// failure replaces only a successful remote exit with ExitTransaction.
func Run(opts RunOptions) int {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	detachCtx, stopDetach := context.WithCancel(context.Background())
	defer stopDetach()
	return runWithDetachNotifications(
		opts, sigCh,
		interruptNotifications{
			stop:   func() { signal.Stop(sigCh) },
			resume: func() { signal.Notify(sigCh, os.Interrupt) },
		},
		detachOnEOFContext(detachCtx, os.Stdin, isTerminalFile(os.Stdin)),
	)
}

func run(opts RunOptions, sigCh <-chan os.Signal, resetInterrupt func()) int {
	return runWithDetach(opts, sigCh, resetInterrupt, nil)
}

func runWithDetach(
	opts RunOptions,
	sigCh <-chan os.Signal,
	resetInterrupt func(),
	detach <-chan struct{},
) int {
	return runWithDetachNotifications(
		opts, sigCh, interruptNotifications{stop: resetInterrupt}, detach,
	)
}

func runWithDetachNotifications(
	opts RunOptions,
	sigCh <-chan os.Signal,
	interruptsControl interruptNotifications,
	detach <-chan struct{},
) int {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	errf := func(format string, args ...any) {
		fmt.Fprintf(opts.Stderr, "errand: "+format+"\n", args...)
	}

	jobID := proto.NewULID()
	handle := peerLabel(opts.PeerName, opts.PeerURL) + "/" + jobID
	interruptCtx, stopInterrupts := context.WithCancel(context.Background())
	defer stopInterrupts()
	interrupts := startInterruptHandoff(
		interruptCtx, sigCh, opts.PeerURL, jobID, handle, errf, interruptsControl,
	)

	prepared := make(chan snapshotPreparation, 1)
	go func() { prepared <- prepareSnapshot(opts.Root, opts.IncludeAll) }()
	var prep snapshotPreparation
	select {
	case <-interrupts.local:
		errf("interrupted before submission")
		return signalExit("interrupt", 2)
	case prep = <-prepared:
	}
	if prep.err != nil {
		errf("%s: %v", prep.stage, prep.err)
		return ExitTransaction
	}
	paths, gitInfo, manifest := prep.paths, prep.gitInfo, prep.manifest
	files, snapshotBytes := snapshotSize(manifest)
	fmt.Fprintf(opts.Stderr, "errand: snapshot contains %d files, %d bytes\n", files, snapshotBytes)

	env := map[string]string{}
	envSources := map[string]string{}
	for _, name := range opts.PassEnv {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
			envSources[name] = "passenv"
		}
	}
	for k, v := range opts.Env {
		env[k] = v
		envSources[k] = "literal"
	}
	if len(env) == 0 {
		env = nil
		envSources = nil
	}

	spec := proto.Spec{
		V:            proto.ProtoVersion,
		Argv:         opts.Argv,
		Env:          env,
		EnvSources:   envSources,
		Workdir:      opts.Workdir,
		ManifestRoot: manifest.RootHash(),
		Limits:       proto.DefaultLimits(),
		GitCommit:    gitInfo.Commit,
		GitDirty:     gitInfo.Dirty,
	}

	if !interrupts.beginAdmission(interruptCtx) {
		errf("interrupted before submission")
		return signalExit("interrupt", 2)
	}

	status, err := submit(opts, jobID, spec, manifest)
	if err != nil {
		errf("%v", err)
		errf("the job may have been admitted; handle %s", handle)
		return ExitTransaction
	}
	fmt.Fprintf(opts.Stderr, "errand: job %s (%d files, commit %s)\n",
		handle, len(paths), shortCommit(gitInfo))

	detachRequested := false
	select {
	case <-detach:
		detachRequested = true
	default:
	}
	if opts.Detach || detachRequested {
		// The handle is the only stdout output so scripts can capture it:
		//   handle=$(errand --detach -- ...)
		if opts.Detach {
			fmt.Fprintln(opts.Stdout, handle)
		}
		return completeDetach(interrupts, interruptCtx, errf, handle)
	}

	final, err, detached := streamUntilDetach(opts, jobID, status, detach)
	if detached {
		return completeDetach(interrupts, interruptCtx, errf, handle)
	}
	if err != nil {
		errf("%v", err)
		errf("the job may still be running; resume with handle %s", handle)
		return ExitTransaction
	}
	return exitCode(final, opts.Stderr, handle)
}

type snapshotPreparation struct {
	paths    []string
	gitInfo  snapshot.GitInfo
	manifest proto.Manifest
	stage    string
	err      error
}

func prepareSnapshot(root string, includeAll bool) snapshotPreparation {
	paths, gitInfo, err := snapshot.SelectFilesWithOptions(root, snapshot.SelectOptions{IncludeAll: includeAll})
	if err != nil {
		return snapshotPreparation{stage: "selecting files", err: err}
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		return snapshotPreparation{stage: "building manifest", err: err}
	}
	return snapshotPreparation{paths: paths, gitInfo: gitInfo, manifest: manifest}
}

func snapshotSize(manifest proto.Manifest) (int, int64) {
	var files int
	var size int64
	for _, entry := range manifest.Entries {
		if entry.Type == proto.EntryFile {
			files++
			size += entry.Size
		}
	}
	return files, size
}

// interruptHandoff gives one goroutine ownership of sigCh. Before admission,
// the first interrupt is a purely local cancellation. beginAdmission is the
// linearization point after which the same signal becomes remote job control.
type interruptHandoff struct {
	begin     chan chan bool
	finish    chan chan bool
	local     chan struct{}
	remote    chan struct{}
	forwarded chan error
}

// interruptNotifications owns the process-wide os/signal registration. A
// successful detach first stops delivery, then drains the channel. If a
// signal was already in flight, delivery is resumed so the usual second
// Ctrl-C force-kill behavior remains available.
type interruptNotifications struct {
	stop   func()
	resume func()
}

func startInterruptHandoff(
	ctx context.Context,
	sigCh <-chan os.Signal,
	peerURL, jobID, handle string,
	errf func(string, ...any),
	interrupts interruptNotifications,
) *interruptHandoff {
	h := &interruptHandoff{
		begin: make(chan chan bool), finish: make(chan chan bool), local: make(chan struct{}),
		remote: make(chan struct{}), forwarded: make(chan error, 1),
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigCh:
				close(h.local)
				return
			case ack := <-h.begin:
				// Prefer an already-delivered interrupt over the handoff so a
				// queued pre-admission Ctrl-C cannot become remote execution.
				select {
				case <-sigCh:
					close(h.local)
					ack <- false
					return
				default:
					ack <- true
				}
				forwardInterruptsWithOutcome(
					ctx, sigCh, peerURL, jobID, handle, errf, interrupts,
					h.finish,
					func() { close(h.remote) },
					func(err error) { h.forwarded <- err },
				)
				return
			}
		}
	}()
	return h
}

func startAdmittedInterruptHandoff(
	ctx context.Context,
	sigCh <-chan os.Signal,
	peerURL, jobID, handle string,
	errf func(string, ...any),
	interrupts interruptNotifications,
) *interruptHandoff {
	h := &interruptHandoff{
		finish: make(chan chan bool), remote: make(chan struct{}), forwarded: make(chan error, 1),
	}
	go forwardInterruptsWithOutcome(
		ctx, sigCh, peerURL, jobID, handle, errf, interrupts,
		h.finish,
		func() { close(h.remote) },
		func(err error) { h.forwarded <- err },
	)
	return h
}

// finishDetach is the success linearization point for detached submission.
// The interrupt owner either confirms no signal was pending and restores
// normal local SIGINT handling, or reports that remote control has begun.
func (h *interruptHandoff) finishDetach(ctx context.Context) bool {
	ack := make(chan bool, 1)
	select {
	case h.finish <- ack:
	case <-h.remote:
		return false
	case <-ctx.Done():
		return false
	}
	select {
	case ok := <-ack:
		return ok
	case <-h.remote:
		return false
	case <-ctx.Done():
		return false
	}
}

func completeDetach(
	interrupts *interruptHandoff,
	ctx context.Context,
	errf func(string, ...any),
	handle string,
) int {
	if interrupts.finishDetach(ctx) {
		errf("detached; reattach with: errand attach %s", handle)
		return 0
	}
	timer := time.NewTimer(controlRequestTimeout)
	defer timer.Stop()
	select {
	case forwardErr := <-interrupts.forwarded:
		if forwardErr != nil {
			errf("SIGINT delivery is uncertain: %v; inspect or control job %s", forwardErr, handle)
			return ExitTransaction
		}
		errf("SIGINT forwarded to %s", handle)
		return signalExit("interrupt", 2)
	case <-timer.C:
		errf("SIGINT delivery is uncertain; inspect or control job %s", handle)
		return ExitTransaction
	}
}

func (h *interruptHandoff) beginAdmission(ctx context.Context) bool {
	ack := make(chan bool, 1)
	select {
	case h.begin <- ack:
	case <-h.local:
		return false
	case <-ctx.Done():
		return false
	}
	select {
	case ok := <-ack:
		return ok
	case <-h.local:
		return false
	case <-ctx.Done():
		return false
	}
}

// forwardInterrupts starts before submit so an admitted job can be controlled
// even while its submit response is delayed or being retried. A 404/409 can be
// a pre-start race, so the first signal is retried until accepted or Run ends.
func forwardInterrupts(
	ctx context.Context,
	sigCh <-chan os.Signal,
	peerURL, jobID, handle string,
	errf func(string, ...any),
	resetInterrupt func(),
) {
	forwardInterruptsWithOutcome(
		ctx, sigCh, peerURL, jobID, handle, errf,
		interruptNotifications{stop: resetInterrupt}, nil, nil, nil,
	)
}

func forwardInterruptsWithOutcome(
	ctx context.Context,
	sigCh <-chan os.Signal,
	peerURL, jobID, handle string,
	errf func(string, ...any),
	interrupts interruptNotifications,
	finish <-chan chan bool,
	onFirst func(),
	onForwarded func(error),
) {

firstSignal:
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigCh:
			break firstSignal
		case ack := <-finish:
			// Stop delivery before the final drain. signal.Stop guarantees that
			// no send remains in flight when it returns, closing the check-then-stop
			// race that could otherwise abandon a queued Ctrl-C.
			if interrupts.stop != nil {
				interrupts.stop()
			}
			select {
			case <-sigCh:
				if interrupts.resume != nil {
					interrupts.resume()
				}
				ack <- false
				break firstSignal
			default:
				ack <- true
				return
			}
		}
	}
	if onFirst != nil {
		onFirst()
	}

	errf("forwarding SIGINT to %s (Ctrl-C again to force-kill)", handle)
	forwardCtx, cancelForward := context.WithCancel(ctx)
	defer cancelForward()
	go func() {
		err := retryJobControl(forwardCtx, peerURL+"/v0/jobs/"+jobID+"/signal", map[string]string{"signal": "SIGINT"}, true)
		if onForwarded != nil {
			onForwarded(err)
		}
		if err != nil && ctx.Err() == nil {
			errf("forwarding SIGINT failed: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
		return
	case <-sigCh:
	}
	cancelForward()
	errf("force-killing %s", handle)
	// Further Ctrl-Cs must recover their normal local behavior even if the
	// force-kill control request loses contact with the peer.
	if interrupts.stop != nil {
		interrupts.stop()
	}
	killCtx, cancelKill := context.WithTimeout(ctx, controlRequestTimeout)
	defer cancelKill()
	if err := retryJobControl(killCtx, peerURL+"/v0/jobs/"+jobID+"/kill?force=1", nil, false); err != nil && ctx.Err() == nil {
		errf("force-kill failed: %v; process may still be running; handle %s", err, handle)
	}
}

func retryJobControl(ctx context.Context, url string, v any, retryConflict bool) error {
	for {
		err := postJSONContext(ctx, url, v)
		if err == nil {
			return nil
		}
		var responseErr *controlHTTPError
		if errors.As(err, &responseErr) && !responseErr.retryableDuringAdmission(retryConflict) {
			return err
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func peerLabel(alias, url string) string {
	if alias != "" {
		return alias
	}
	return strings.TrimSuffix(url, "/")
}

// AttachOptions identifies an existing job to reattach to.
type AttachOptions struct {
	PeerURL  string
	PeerName string
	JobID    string
	Stdout   io.Writer
	Stderr   io.Writer
}

// Attach resumes following an existing job: it streams the log from the
// beginning, forwards Ctrl-C (twice force-kills), and exits per the same
// two-layer rule as an attached run.
func Attach(opts AttachOptions) int {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	detachCtx, stopDetach := context.WithCancel(context.Background())
	defer stopDetach()
	return attachWithDetachNotifications(
		opts, sigCh,
		interruptNotifications{
			stop:   func() { signal.Stop(sigCh) },
			resume: func() { signal.Notify(sigCh, os.Interrupt) },
		},
		detachOnEOFContext(detachCtx, os.Stdin, isTerminalFile(os.Stdin)),
	)
}

func attachWithDetach(
	opts AttachOptions,
	sigCh <-chan os.Signal,
	resetInterrupt func(),
	detach <-chan struct{},
) int {
	return attachWithDetachNotifications(
		opts, sigCh, interruptNotifications{stop: resetInterrupt}, detach,
	)
}

func attachWithDetachNotifications(
	opts AttachOptions,
	sigCh <-chan os.Signal,
	interruptsControl interruptNotifications,
	detach <-chan struct{},
) int {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	errf := func(format string, args ...any) {
		fmt.Fprintf(opts.Stderr, "errand: "+format+"\n", args...)
	}
	handle := peerLabel(opts.PeerName, opts.PeerURL) + "/" + opts.JobID

	status, err := getStatus(opts.PeerURL, opts.JobID)
	if err != nil {
		errf("%v", err)
		return ExitTransaction
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The job is already admitted, so signals are remote control from the
	// first Ctrl-C; there is no pre-admission local-cancel phase here.
	interrupts := startAdmittedInterruptHandoff(
		ctx, sigCh, opts.PeerURL, opts.JobID, handle, errf, interruptsControl,
	)

	runOpts := RunOptions{PeerURL: opts.PeerURL, Stdout: opts.Stdout, Stderr: opts.Stderr}
	final, err, detached := streamUntilDetach(runOpts, opts.JobID, status, detach)
	if detached {
		return completeDetach(interrupts, ctx, errf, handle)
	}
	if err != nil {
		errf("%v", err)
		errf("the job may still be running; resume with handle %s", handle)
		return ExitTransaction
	}
	return exitCode(final, opts.Stderr, handle)
}

func getStatus(peerURL, jobID string) (proto.JobStatus, error) {
	var status proto.JobStatus
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	return status, getJSONContext(ctx, peerURL+"/v0/jobs/"+jobID, 1<<20, "job lookup", &status)
}

// List fetches a runner's job listing (the caller's own jobs).
func List(peerURL string) ([]proto.JobListEntry, error) {
	var entries []proto.JobListEntry
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	return entries, getJSONContext(ctx, peerURL+"/v0/jobs", 1<<20, "job listing", &entries)
}

func getJSONContext(ctx context.Context, url string, maxBytes int64, label string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := directHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("%s: reading response: %w", label, err)
	}
	if int64(len(body)) > maxBytes {
		return fmt.Errorf("%s: response exceeds %d bytes", label, maxBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s: %s", label, resp.Status, apiError(body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("%s: decoding response: %w", label, err)
	}
	return nil
}

// Kill asks the runner to terminate a job (SIGTERM, or SIGKILL with force).
func Kill(peerURL, jobID string, force bool) error {
	url := peerURL + "/v0/jobs/" + jobID + "/kill"
	if force {
		url += "?force=1"
	}
	return postJSONContext(context.Background(), url, nil)
}

func shortCommit(gi snapshot.GitInfo) string {
	if gi.Commit == "" {
		return "none"
	}
	c := gi.Commit
	if len(c) > 12 {
		c = c[:12]
	}
	if gi.Dirty {
		c += "-dirty"
	}
	return c
}

// submit PUTs the spec, manifest, and workspace tar as one streaming
// multipart request. The job ID and digest make it safe to retry.
func submit(opts RunOptions, jobID string, spec proto.Spec, manifest proto.Manifest) (proto.JobStatus, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		status, retryable, err := submitOnce(opts, jobID, spec, manifest)
		if err == nil {
			return status, nil
		}
		lastErr = err
		if !retryable {
			return status, err
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
	}
	return proto.JobStatus{}, lastErr
}

func submitOnce(opts RunOptions, jobID string, spec proto.Spec, manifest proto.Manifest) (proto.JobStatus, bool, error) {
	var status proto.JobStatus
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			part, err := mw.CreateFormField("spec")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(spec); err != nil {
				return err
			}
			part, err = mw.CreateFormField("manifest")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(manifest); err != nil {
				return err
			}
			part, err = mw.CreateFormFile("workspace", "workspace.tar")
			if err != nil {
				return err
			}
			if err := snapshot.Pack(part, opts.Root, manifest); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), submitRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, opts.PeerURL+"/v0/jobs/"+jobID, pr)
	if err != nil {
		return status, false, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Errand-Digest", spec.Digest())
	resp, err := directHTTP.Do(req)
	if err != nil {
		return status, true, fmt.Errorf("submitting to %s: %w", opts.PeerURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		if err := json.Unmarshal(body, &status); err != nil {
			return status, false, fmt.Errorf("parsing submit response: %w", err)
		}
		return status, false, nil
	case http.StatusTooManyRequests:
		return status, false, fmt.Errorf("runner is busy (one job at a time)")
	default:
		return status, false, fmt.Errorf("submit rejected: %s: %s", resp.Status, apiError(body))
	}
}

// stream follows the SSE log stream, printing frames to the local
// stdout/stderr, resuming after transient disconnects, until the terminal
// status event arrives.
func stream(opts RunOptions, jobID string, initial proto.JobStatus) (proto.JobStatus, error) {
	return streamContext(context.Background(), opts, jobID, initial)
}

type streamResult struct {
	status proto.JobStatus
	err    error
}

func streamUntilDetach(
	opts RunOptions,
	jobID string,
	initial proto.JobStatus,
	detach <-chan struct{},
) (proto.JobStatus, error, bool) {
	if detach == nil {
		status, err := stream(opts, jobID, initial)
		return status, err, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan streamResult, 1)
	go func() {
		status, err := streamContext(ctx, opts, jobID, initial)
		done <- streamResult{status: status, err: err}
	}()
	select {
	case result := <-done:
		cancel()
		return result.status, result.err, false
	case <-detach:
		// Prefer an outcome already published before detachment linearized.
		select {
		case result := <-done:
			cancel()
			return result.status, result.err, false
		default:
		}
		cancel()
		// Cancellation stops the HTTP follower, but a decoded frame may already
		// be inside a caller-supplied Writer. Do not report successful detach or
		// return from the library until that follower has actually stopped.
		<-done
		return proto.JobStatus{}, nil, true
	}
}

func streamContext(
	ctx context.Context,
	opts RunOptions,
	jobID string,
	initial proto.JobStatus,
) (proto.JobStatus, error) {
	terminalReplay := initial.State != proto.StateRunning && initial.Result != nil
	var last int64
	deadline := time.Now().Add(time.Duration(proto.DefaultLimits().MaxRuntimeSec)*time.Second + streamDeadlineMargin)
	for attempt := 0; ; attempt++ {
		final, err := followOnceContext(ctx, opts, jobID, &last)
		if err == nil {
			return final, nil
		}
		if ctx.Err() != nil {
			return proto.JobStatus{}, ctx.Err()
		}
		var integrityErr *streamIntegrityError
		if errors.As(err, &integrityErr) {
			return proto.JobStatus{}, err
		}
		var permanentErr *streamHTTPError
		if errors.As(err, &permanentErr) && permanentErr.permanent() {
			return proto.JobStatus{}, err
		}
		if time.Now().After(deadline) {
			kind := "log stream"
			if terminalReplay {
				kind = "terminal log replay"
			}
			return proto.JobStatus{}, fmt.Errorf("%s failed through transaction deadline: %w", kind, err)
		}
		timer := time.NewTimer(min(time.Duration(attempt+1)*time.Second, 5*time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return proto.JobStatus{}, ctx.Err()
		case <-timer.C:
		}
	}
}

type streamIntegrityError struct{ err error }

type streamHTTPError struct {
	statusCode int
	err        error
}

func (e *streamHTTPError) Error() string { return e.err.Error() }
func (e *streamHTTPError) Unwrap() error { return e.err }
func (e *streamHTTPError) permanent() bool {
	switch e.statusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return false
	default:
		return true
	}
}

func (e *streamIntegrityError) Error() string { return e.err.Error() }
func (e *streamIntegrityError) Unwrap() error { return e.err }

func streamIntegrity(err error) error {
	return &streamIntegrityError{err: err}
}

func followOnce(opts RunOptions, jobID string, last *int64) (proto.JobStatus, error) {
	return followOnceContext(context.Background(), opts, jobID, last)
}

func followOnceContext(
	ctx context.Context,
	opts RunOptions,
	jobID string,
	last *int64,
) (proto.JobStatus, error) {
	url := fmt.Sprintf("%s/v0/jobs/%s/logs?follow=1&from=%d", opts.PeerURL, jobID, *last)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return proto.JobStatus{}, err
	}
	resp, err := directHTTP.Do(req)
	if err != nil {
		return proto.JobStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return proto.JobStatus{}, &streamHTTPError{
			statusCode: resp.StatusCode,
			err:        fmt.Errorf("log stream: %s: %s", resp.Status, apiError(body)),
		}
	}

	var event string
	var data bytes.Buffer
	sc := bufio.NewScanner(&idleReadCloser{ReadCloser: resp.Body, timeout: streamIdleTimeout})
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			switch event {
			case "log":
				var f proto.LogFrame
				if err := json.Unmarshal(data.Bytes(), &f); err != nil {
					return proto.JobStatus{}, streamIntegrity(err)
				}
				if f.Seq != *last+1 {
					return proto.JobStatus{}, streamIntegrity(fmt.Errorf("log sequence %d, expected %d", f.Seq, *last+1))
				}
				if f.Stream != "stdout" && f.Stream != "stderr" {
					return proto.JobStatus{}, streamIntegrity(fmt.Errorf("unknown log stream %q", f.Stream))
				}
				raw, err := base64.StdEncoding.DecodeString(f.DataB64)
				if err != nil {
					return proto.JobStatus{}, streamIntegrity(err)
				}
				var target io.Writer = opts.Stdout
				if f.Stream == "stderr" {
					target = opts.Stderr
				}
				if target == nil {
					target = io.Discard
				}
				n, err := target.Write(raw)
				if err != nil {
					return proto.JobStatus{}, streamIntegrity(err)
				}
				if n != len(raw) {
					return proto.JobStatus{}, streamIntegrity(io.ErrShortWrite)
				}
				*last = f.Seq
			case "status":
				var st proto.JobStatus
				if err := json.Unmarshal(data.Bytes(), &st); err != nil {
					return proto.JobStatus{}, streamIntegrity(err)
				}
				return st, nil
			case "error":
				var remote proto.LogStreamError
				decodeErr := json.Unmarshal(data.Bytes(), &remote)
				if decodeErr != nil || remote.Message == "" {
					var legacyMessage string
					if legacyErr := json.Unmarshal(data.Bytes(), &legacyMessage); legacyErr != nil || legacyMessage == "" {
						return proto.JobStatus{}, streamIntegrity(fmt.Errorf("invalid remote log error event"))
					}
					remote.Message = legacyMessage
				}
				remoteErr := fmt.Errorf("remote log replay: %s", remote.Message)
				if remote.Retryable {
					return proto.JobStatus{}, remoteErr
				}
				return proto.JobStatus{}, streamIntegrity(remoteErr)
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return proto.JobStatus{}, ctx.Err()
		}
		return proto.JobStatus{}, err
	}
	return proto.JobStatus{}, fmt.Errorf("stream ended without a terminal status")
}

type idleReadCloser struct {
	io.ReadCloser
	timeout time.Duration
}

// detachOnEOF converts terminal EOF (normally Ctrl-D on an empty line) into
// a local detach request. Non-terminal EOF is ignored so scripts and
// background jobs do not silently change from attached to detached mode.
func detachOnEOF(r io.Reader, interactive bool) <-chan struct{} {
	return detachOnEOFContext(context.Background(), r, interactive)
}

type terminalEOFOps struct {
	foreground   func() bool
	waitReadable func(time.Duration) (bool, error)
	read         func([]byte) (int, error)
}

func watchTerminalEOF(ctx context.Context, ops terminalEOFOps) <-chan struct{} {
	detach := make(chan struct{})
	go func() {
		var one [1]byte
		for {
			if !ops.foreground() {
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			ready, err := ops.waitReadable(100 * time.Millisecond)
			if err != nil {
				return
			}
			if !ready || !ops.foreground() {
				continue
			}
			_, err = ops.read(one[:])
			if errors.Is(err, io.EOF) {
				close(detach)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return detach
}

func detachOnEOFContext(ctx context.Context, r io.Reader, interactive bool) <-chan struct{} {
	if !interactive || r == nil {
		return nil
	}
	if f, ok := r.(*os.File); ok {
		return watchTerminalEOF(ctx, terminalEOFOps{
			foreground: func() bool { return isTerminalFile(f) },
			waitReadable: func(timeout time.Duration) (bool, error) {
				return waitTerminalReadable(f, timeout)
			},
			read: f.Read,
		})
	}
	detach := make(chan struct{})
	go func() {
		var one [1]byte
		for {
			_, err := r.Read(one[:])
			if errors.Is(err, io.EOF) {
				close(detach)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return detach
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := r.ReadCloser.Read(p)
		ch <- result{n: n, err: err}
	}()
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case got := <-ch:
		return got.n, got.err
	case <-timer.C:
		_ = r.ReadCloser.Close()
		return 0, fmt.Errorf("stream idle timeout after %s", r.timeout)
	}
}

// exitCode applies the two-layer rule.
func exitCode(st proto.JobStatus, stderr io.Writer, handle string) int {
	res := st.Result
	if res == nil {
		fmt.Fprintf(stderr, "errand: %s has no result (state %s)\n", handle, st.State)
		return ExitTransaction
	}
	ambiguous := st.State == proto.StateAmbiguous
	if ambiguous && res.ExitCode == nil && res.Signal == "" {
		fmt.Fprintf(stderr, "errand: transaction incomplete (%s, state=%s", handle, st.State)
		if res.StartError != "" {
			fmt.Fprintf(stderr, ", start error: %s", res.StartError)
		}
		if !res.OutputsOK {
			fmt.Fprint(stderr, ", outputs incomplete")
		}
		if !res.CleanupOK {
			fmt.Fprint(stderr, ", cleanup failed")
		}
		if res.LimitExceeded != "" {
			fmt.Fprintf(stderr, ", limit exceeded: %s", res.LimitExceeded)
		}
		if !res.LogsComplete {
			fmt.Fprint(stderr, ", logs truncated")
		}
		if res.TransactionError != "" {
			fmt.Fprintf(stderr, ", %s", res.TransactionError)
		}
		fmt.Fprintln(stderr, ")")
		return ExitTransaction
	}
	transactionOK := !ambiguous && res.CleanupOK && res.OutputsOK && res.LimitExceeded == "" &&
		res.StartError == "" && res.TransactionError == "" && res.LogsComplete
	switch {
	case res.StartError != "":
		if ambiguous {
			fmt.Fprintf(stderr, "errand: job failed to start (state ambiguous): %s\n", res.StartError)
		} else {
			fmt.Fprintf(stderr, "errand: job failed to start: %s\n", res.StartError)
		}
		return ExitTransaction
	case res.Signal != "":
		fmt.Fprintf(stderr, "errand: remote process killed by %s", res.Signal)
		if ambiguous {
			fmt.Fprint(stderr, " (state ambiguous)")
		}
		if !res.CleanupOK {
			fmt.Fprintf(stderr, " (cleanup failed)")
		}
		if res.LimitExceeded != "" {
			fmt.Fprintf(stderr, " (limit exceeded: %s)", res.LimitExceeded)
		}
		if !res.LogsComplete {
			fmt.Fprintf(stderr, " (logs truncated)")
		}
		if res.TransactionError != "" {
			fmt.Fprintf(stderr, " (transaction error: %s)", res.TransactionError)
		}
		fmt.Fprintln(stderr)
		return signalExit(res.Signal, res.SignalNum)
	case res.ExitCode != nil && !transactionOK:
		fmt.Fprint(stderr, "errand: transaction incomplete (")
		if ambiguous {
			fmt.Fprint(stderr, "state=ambiguous, ")
		}
		fmt.Fprintf(stderr, "remote_exit=%d", *res.ExitCode)
		if !res.CleanupOK {
			fmt.Fprintf(stderr, ", cleanup failed")
		}
		if res.LimitExceeded != "" {
			fmt.Fprintf(stderr, ", limit exceeded: %s", res.LimitExceeded)
		}
		if !res.LogsComplete {
			fmt.Fprintf(stderr, ", logs truncated")
		}
		if res.TransactionError != "" {
			fmt.Fprintf(stderr, ", %s", res.TransactionError)
		}
		fmt.Fprintln(stderr, ")")
		if *res.ExitCode == 0 {
			return ExitTransaction
		}
		return *res.ExitCode
	case res.ExitCode != nil:
		return *res.ExitCode
	case !transactionOK:
		fmt.Fprintf(stderr, "errand: transaction incomplete (%s, state=%s", handle, st.State)
		if !res.OutputsOK {
			fmt.Fprint(stderr, ", outputs incomplete")
		}
		if !res.CleanupOK {
			fmt.Fprint(stderr, ", cleanup failed")
		}
		if res.LimitExceeded != "" {
			fmt.Fprintf(stderr, ", limit exceeded: %s", res.LimitExceeded)
		}
		if !res.LogsComplete {
			fmt.Fprint(stderr, ", logs truncated")
		}
		if res.TransactionError != "" {
			fmt.Fprintf(stderr, ", %s", res.TransactionError)
		}
		fmt.Fprintln(stderr, ")")
		return ExitTransaction
	default:
		fmt.Fprintf(stderr, "errand: %s has no process outcome (state %s)\n", handle, st.State)
		return ExitTransaction
	}
}

func signalExit(sig string, signalNum int) int {
	if signalNum > 0 && signalNum < 128 {
		return 128 + signalNum
	}
	switch sig {
	case "hangup":
		return 129
	case "interrupt":
		return 130
	case "quit":
		return 131
	case "aborted":
		return 134
	case "terminated":
		return 143
	case "killed":
		return 137
	case "segmentation fault":
		return 139
	case "broken pipe":
		return 141
	default:
		return ExitTransaction
	}
}

// Info fetches a runner's measured facts.
func Info(peerURL string) (proto.Info, error) {
	var info proto.Info
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	return info, getJSONContext(ctx, peerURL+"/v0/info", 1<<20, "runner info", &info)
}

type controlHTTPError struct {
	statusCode int
	err        error
}

func (e *controlHTTPError) Error() string { return e.err.Error() }
func (e *controlHTTPError) Unwrap() error { return e.err }
func (e *controlHTTPError) retryableDuringAdmission(retryConflict bool) bool {
	switch e.statusCode {
	case http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooEarly,
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusConflict:
		return retryConflict
	default:
		return false
	}
}

func postJSONContext(parent context.Context, url string, v any) error {
	var body io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(parent, controlRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := directHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return &controlHTTPError{
			statusCode: resp.StatusCode,
			err:        fmt.Errorf("%s: %s", resp.Status, apiError(body)),
		}
	}
	return nil
}

func apiError(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(body))
}
