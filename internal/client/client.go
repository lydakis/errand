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
	"unicode/utf8"

	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

// ExitTransaction is the errand-level failure exit code: the transaction
// did not complete faithfully, whatever the remote process did.
const ExitTransaction = 120

const (
	controlRequestTimeout = 15 * time.Second
	storageRequestTimeout = 2 * time.Minute
	maintenanceTimeout    = 30 * time.Minute
	submitRequestTimeout  = 31 * time.Minute
	streamIdleTimeout     = 2 * time.Minute
	streamDeadlineMargin  = 5 * time.Minute
	maxProjectLabelBytes  = 128
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

var maintenanceTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ResponseHeaderTimeout = maintenanceTimeout
	return t
}()

var maintenanceHTTP = &http.Client{
	Transport: maintenanceTransport,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type RunOptions struct {
	Artifacts      []string
	PeerURL        string
	PeerName       string // config alias for handle printing; "" falls back to the host
	Root           string
	Argv           []string
	Env            map[string]string // literal values
	PassEnv        []string          // names copied from the local environment
	Workdir        string
	Project        string
	IncludeAll     bool
	NoSnapshot     bool // use a fresh empty workspace without inspecting Root contents
	Detach         bool // return after admission, printing the handle on stdout
	ApplyOnSuccess bool // apply retained changes after successful completion, attached or detached
	Forwards       []PortForward
	changeClientID string
	selectionGuard *snapshot.SelectionGuard
	Stdout         io.Writer
	Stderr         io.Writer
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
		newInterruptNotifications(
			func() { signal.Stop(sigCh) },
			func() { signal.Notify(sigCh, os.Interrupt) },
		),
		detachOnEOFContext(detachCtx, os.Stdin, isTerminalFile(os.Stdin)),
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
	if opts.Detach && len(opts.Forwards) != 0 {
		errf("--detach and --forward are mutually exclusive")
		return ExitTransaction
	}
	if err := pathpolicy.ValidateArtifacts(opts.Artifacts); err != nil {
		errf("%v", err)
		return ExitTransaction
	}
	// Resolve required environment before opening forwards, preparing a
	// snapshot, contacting the runner, or creating local submission state.
	env := map[string]string{}
	envSources := map[string]string{}
	for _, name := range opts.PassEnv {
		if _, overridden := opts.Env[name]; overridden {
			continue
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			errf("required environment variable %q is not set", name)
			return ExitTransaction
		}
		env[name] = value
		envSources[name] = "passenv"
	}
	for name, value := range opts.Env {
		env[name] = value
		envSources[name] = "literal"
	}
	if len(env) == 0 {
		env, envSources = nil, nil
	}
	forwarding, err := bindPortForwards(opts.Forwards, opts.Stderr)
	if err != nil {
		errf("%v", err)
		return ExitTransaction
	}
	defer forwarding.Close()

	jobID := proto.NewULID()
	handle := peerLabel(opts.PeerName, opts.PeerURL) + "/" + jobID
	interruptCtx, stopInterrupts := context.WithCancel(context.Background())
	defer stopInterrupts()
	target := newInterruptTarget(opts.PeerURL, jobID, handle, errf, interruptsControl)

	prepared := make(chan snapshotPreparation, 1)
	go func() { prepared <- prepareSnapshot(opts.Root, opts.IncludeAll, opts.NoSnapshot) }()
	var prep snapshotPreparation
	select {
	case <-sigCh:
		errf("interrupted before submission")
		return signalExit("interrupt", 2)
	case prep = <-prepared:
	}
	if prep.err != nil {
		errf("%s: %v", prep.stage, prep.err)
		return ExitTransaction
	}
	opts.selectionGuard = prep.guard
	changeStateInitialized := true
	submissionStarted := false
	defer func() {
		if changeStateInitialized && !submissionStarted {
			if err := discardUnsubmittedChangeState(opts.PeerURL, jobID); err != nil {
				errf("removing unsubmitted change state: %v", err)
			}
		}
	}()
	changeInitCtx, cancelChangeInit := context.WithCancel(interruptCtx)
	changeInitialized := make(chan error, 1)
	go func() {
		changeInitialized <- initializeChangeState(changeInitCtx, &opts, jobID, prep.manifest.RootHash())
	}()
	select {
	case <-sigCh:
		cancelChangeInit()
		<-changeInitialized
		errf("interrupted before submission")
		return signalExit("interrupt", 2)
	case err := <-changeInitialized:
		cancelChangeInit()
		if err != nil {
			errf("%v", err)
			return ExitTransaction
		}
	}
	paths, gitInfo, manifest := prep.paths, prep.gitInfo, prep.manifest
	files, snapshotBytes := snapshotSize(manifest)
	if opts.NoSnapshot {
		fmt.Fprintln(opts.Stderr, "errand: no snapshot; using an empty remote workspace")
	} else {
		fmt.Fprintf(opts.Stderr, "errand: snapshot contains %d files, %d bytes\n", files, snapshotBytes)
	}

	spec := proto.Spec{
		Argv:           opts.Argv,
		Env:            env,
		EnvSources:     envSources,
		Workdir:        opts.Workdir,
		ManifestRoot:   manifest.RootHash(),
		Limits:         proto.DefaultLimits(),
		GitCommit:      gitInfo.Commit,
		GitDirty:       gitInfo.Dirty,
		NoSnapshot:     opts.NoSnapshot,
		ChangeClientID: opts.changeClientID,
		Selection:      prep.selection,
	}
	spec.Selection.Artifacts = opts.Artifacts

	type negotiationResult struct {
		plan shipPlan
		err  error
	}
	negotiationCtx, cancelNegotiation := context.WithCancel(interruptCtx)
	negotiated := make(chan negotiationResult, 1)
	go func() {
		plan, err := negotiateSnapshot(negotiationCtx, opts, manifest)
		negotiated <- negotiationResult{plan: plan, err: err}
	}()
	var negotiation negotiationResult
	select {
	case <-sigCh:
		cancelNegotiation()
		<-negotiated
		errf("interrupted before submission")
		return signalExit("interrupt", 2)
	case negotiation = <-negotiated:
		cancelNegotiation()
	}
	plan, negErr := negotiation.plan, negotiation.err
	if negErr != nil {
		errf("snapshot negotiation failed (%v); shipping everything", negErr)
		plan = shipPlan{}
	}
	if plan.partial {
		shipFiles, shipBytes := 0, int64(0)
		for _, e := range manifest.Entries {
			if e.Type == proto.EntryFile && plan.ships(e) {
				shipFiles++
				shipBytes += e.Size
			}
		}
		fmt.Fprintf(opts.Stderr, "errand: shipping %d of %d files (%d bytes; the rest is cached on the runner)\n",
			shipFiles, files, shipBytes)
	}

	if changeStateInitialized {
		if err := markLocalChangeSubmissionStarted(opts.PeerURL, jobID); err != nil {
			errf("recording change submission state: %v", err)
			return ExitTransaction
		}
	}
	controller := admitJobController(interruptCtx, sigCh, target)
	if controller == nil {
		errf("interrupted before submission")
		return signalExit("interrupt", 2)
	}

	submissionStarted = true
	status, admissionUncertain, err := submit(opts, jobID, spec, manifest, plan)
	if err != nil {
		errf("%v", err)
		if !admissionUncertain && submitDefinitelyRejected(err) {
			submissionStarted = false
			return ExitTransaction
		}
		if opts.ApplyOnSuccess {
			if workerErr := handoffAutomaticApply(opts.PeerURL, jobID); workerErr != nil {
				errf("automatic workspace change application could not continue: %v", workerErr)
			}
		}
		errf("the job may have been admitted; handle %s", handle)
		return ExitTransaction
	}
	if opts.ApplyOnSuccess {
		if err := confirmAutomaticApplyAdmission(opts.PeerURL, jobID); err != nil {
			errf("recording automatic apply admission: %v", err)
			errf("automatic apply is still pending; recover with: errand fetch --apply %s", handle)
		}
	}
	automaticWorkerStarted, _ := ensureAutomaticApplyWorker(opts, jobID, false)
	fmt.Fprintf(opts.Stderr, "errand: job %s (%d files, commit %s)\n",
		handle, len(paths), shortCommit(gitInfo))
	forwarding.Start(opts.PeerURL, jobID)

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
		return completeRunDetach(opts, jobID, handle, controller, interruptCtx, automaticWorkerStarted)
	}

	reportAdmissionState(status, opts.Stderr)
	final, err, detached := streamUntilDetach(opts, jobID, status, detach)
	if detached {
		return completeRunDetach(opts, jobID, handle, controller, interruptCtx, automaticWorkerStarted)
	}
	if err != nil {
		if _, workerErr := ensureAutomaticApplyWorker(opts, jobID, automaticWorkerStarted); workerErr != nil {
			errf("automatic workspace change application could not continue: %v", workerErr)
		}
		errf("%v", err)
		errf("the job may still be running; resume with handle %s", handle)
		return ExitTransaction
	}
	if !controller.releaseAtTerminal(interruptCtx) {
		forwarding.Close()
		if _, workerErr := ensureAutomaticApplyWorker(opts, jobID, automaticWorkerStarted); workerErr != nil {
			errf("automatic workspace change application could not continue: %v", workerErr)
		}
		stopInterrupts()
		<-controller.done
		errf("interrupted as the job completed; inspect or fetch workspace changes with handle %s", handle)
		return signalExit("interrupt", 2)
	}
	forwarding.Close()
	return finishTerminalChanges(opts, jobID, handle, final)
}

func reportAdmissionState(status proto.JobStatus, stderr io.Writer) {
	if status.State == proto.StateQueued {
		fmt.Fprintln(stderr, "errand: queued on the runner; logs follow when it starts (Ctrl-C cancels)")
	}
}

func completeRunDetach(
	opts RunOptions,
	jobID, handle string,
	controller *admittedJobController,
	ctx context.Context,
	automaticWorkerStarted bool,
) int {
	_, workerErr := ensureAutomaticApplyWorker(opts, jobID, automaticWorkerStarted)
	code := controller.completeDetach(ctx)
	if code != 0 {
		return code
	}
	if workerErr != nil {
		fmt.Fprintf(opts.Stderr, "errand: automatic workspace change application could not continue: %v\n", workerErr)
		fmt.Fprintf(opts.Stderr, "errand: fetch and apply manually with: errand fetch --apply %s\n", handle)
		return ExitTransaction
	}
	return 0
}

func ensureAutomaticApplyWorker(opts RunOptions, jobID string, alreadyStarted bool) (bool, error) {
	if !opts.ApplyOnSuccess || alreadyStarted {
		return alreadyStarted, nil
	}
	if err := handoffAutomaticApply(opts.PeerURL, jobID); err != nil {
		return false, err
	}
	return true, nil
}

type snapshotPreparation struct {
	paths     []string
	gitInfo   snapshot.GitInfo
	selection proto.SelectionPolicy
	manifest  proto.Manifest
	guard     *snapshot.SelectionGuard
	stage     string
	err       error
}

func prepareSnapshot(root string, includeAll, noSnapshot bool) snapshotPreparation {
	if noSnapshot {
		return snapshotPreparation{}
	}
	paths, gitInfo, selection, guard, err := snapshot.SelectFilesGuarded(root, snapshot.SelectOptions{IncludeAll: includeAll})
	if err != nil {
		return snapshotPreparation{stage: "selecting files", err: err}
	}
	manifest, err := snapshot.Build(root, paths)
	if err != nil {
		return snapshotPreparation{stage: "building manifest", err: err}
	}
	return snapshotPreparation{paths: paths, gitInfo: gitInfo, selection: selection, manifest: manifest, guard: guard}
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
	Forwards []PortForward
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
		newInterruptNotifications(
			func() { signal.Stop(sigCh) },
			func() { signal.Notify(sigCh, os.Interrupt) },
		),
		detachOnEOFContext(detachCtx, os.Stdin, isTerminalFile(os.Stdin)),
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
	forwarding, err := bindPortForwards(opts.Forwards, opts.Stderr)
	if err != nil {
		errf("%v", err)
		return ExitTransaction
	}
	defer forwarding.Close()

	status, err := getStatus(opts.PeerURL, opts.JobID)
	if err != nil {
		errf("%v", err)
		return ExitTransaction
	}
	if len(opts.Forwards) != 0 && status.Result != nil {
		errf("cannot forward a terminal job")
		return ExitTransaction
	}
	forwarding.Start(opts.PeerURL, opts.JobID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The job is already admitted, so signals are remote control from the
	// first Ctrl-C; there is no pre-admission local-cancel phase here.
	controller := startAdmittedJobController(
		ctx, sigCh,
		newInterruptTarget(opts.PeerURL, opts.JobID, handle, errf, interruptsControl),
	)

	runOpts := RunOptions{PeerURL: opts.PeerURL, Stdout: opts.Stdout, Stderr: opts.Stderr}
	final, err, detached := streamUntilDetach(runOpts, opts.JobID, status, detach)
	if detached {
		return controller.completeDetach(ctx)
	}
	if err != nil {
		errf("%v", err)
		errf("the job may still be running; resume with handle %s", handle)
		return ExitTransaction
	}
	if !controller.releaseAtTerminal(ctx) {
		forwarding.Close()
		cancel()
		<-controller.done
		errf("interrupted as the job completed; inspect with handle %s", handle)
		return signalExit("interrupt", 2)
	}
	forwarding.Close()
	return finishTerminalChanges(runOpts, opts.JobID, handle, final)
}

func finishTerminalChanges(opts RunOptions, jobID, handle string, final proto.JobStatus) int {
	changeErr := markLocalChangeTerminal(opts.PeerURL, jobID)
	code := exitCode(final, opts.Stderr, handle)
	if changeErr != nil {
		fmt.Fprintf(opts.Stderr, "errand: recording terminal workspace state failed: %v\n", changeErr)
		if code == 0 {
			return ExitTransaction
		}
	}
	automatic, automaticRequested, automaticErr := automaticApplyForJob(opts.PeerURL, jobID)
	if opts.ApplyOnSuccess && changeErr == nil {
		automaticRequested = true
		automatic, automaticErr = applyTerminalAutomatically(opts.PeerURL, jobID, final)
	}
	if automaticErr != nil {
		label := "reading automatic apply state"
		if opts.ApplyOnSuccess {
			label = "automatic workspace change application failed"
		}
		fmt.Fprintf(opts.Stderr, "errand: %s: %v\n", label, automaticErr)
		if automatic.staged != "" {
			fmt.Fprintf(opts.Stderr, "errand: workspace changes remain staged at %s\n", automatic.staged)
		}
		if code == 0 {
			return ExitTransaction
		}
	}
	if final.Result == nil || final.Result.Changes == nil {
		return code
	}
	changes := final.Result.Changes
	if automaticRequested {
		switch automatic.state {
		case automaticApplyApplied:
			fmt.Fprintf(opts.Stderr, "errand: workspace changes applied from %s\n", automatic.staged)
		case automaticApplyFailed:
			fmt.Fprintf(opts.Stderr, "errand: automatic workspace change application failed: %s\n", automatic.err)
			if automatic.staged != "" {
				fmt.Fprintf(opts.Stderr, "errand: workspace changes remain staged at %s\n", automatic.staged)
			}
			if code == 0 {
				return ExitTransaction
			}
		case automaticApplyPending, automaticApplyRunning:
			fmt.Fprintln(opts.Stderr, "errand: automatic workspace change application is pending")
		default:
			writeRetainedChanges(opts.Stderr, changes, handle)
		}
	} else {
		writeRetainedChanges(opts.Stderr, changes, handle)
	}
	return code
}

func writeRetainedChanges(stderr io.Writer, changes *proto.ChangeSummary, handle string) {
	fmt.Fprintf(stderr,
		"errand: %d workspace changes retained (%d bytes); fetch with errand fetch %s\n",
		changes.PathCount, changes.Bytes, handle)
}

func getStatus(peerURL, jobID string) (proto.JobStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	return getStatusContext(ctx, peerURL, jobID)
}

func getStatusContext(ctx context.Context, peerURL, jobID string) (proto.JobStatus, error) {
	details, err := getJobDetailsContext(ctx, peerURL, jobID)
	return details.JobStatus, err
}

// GetJobDetails returns the owner-visible, non-secret description of one job.
func GetJobDetails(peerURL, jobID string) (proto.JobDetails, error) {
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	return getJobDetailsContext(ctx, peerURL, jobID)
}

func getJobDetailsContext(ctx context.Context, peerURL, jobID string) (proto.JobDetails, error) {
	var details proto.JobDetails
	err := getJSONContext(ctx, peerURL+"/v0/jobs/"+jobID, 1<<20, "job lookup", &details)
	return details, err
}

func StorageStats(peerURL string) (proto.StorageStats, error) {
	var stats proto.StorageStats
	ctx, cancel := context.WithTimeout(context.Background(), storageRequestTimeout)
	defer cancel()
	err := getJSONWithClientContext(ctx, maintenanceHTTP, peerURL+"/v0/storage", 1<<20, "storage stats", &stats)
	return stats, err
}

func CacheGC(peerURL string, dryRun bool) (proto.CacheGCResult, error) {
	var result proto.CacheGCResult
	err := postJSONResultContextTimeout(
		context.Background(), maintenanceHTTP, maintenanceTimeout,
		peerURL+"/v0/cache/gc", proto.CacheGCRequest{DryRun: dryRun}, "cache gc", &result,
	)
	return result, err
}

func JobGC(peerURL string, request proto.JobGCRequest) (proto.JobGCResult, error) {
	var result proto.JobGCResult
	err := postJSONResultContextTimeout(
		context.Background(), maintenanceHTTP, maintenanceTimeout,
		peerURL+"/v0/jobs/gc", request, "job gc", &result,
	)
	return result, err
}

// List fetches a runner's job listing (the caller's own jobs).
func List(peerURL string) ([]proto.JobListEntry, error) {
	return list(peerURL, false)
}

// ListActive fetches the runner's bounded active-job window without letting
// retained terminal receipts consume it.
func ListActive(peerURL string) ([]proto.JobListEntry, error) {
	return list(peerURL, true)
}

func list(peerURL string, activeOnly bool) ([]proto.JobListEntry, error) {
	var entries []proto.JobListEntry
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	url := peerURL + "/v0/jobs"
	if activeOnly {
		url += "?active=1"
	}
	err := getJSONContext(ctx, url, 1<<20, "job listing", &entries)
	return entries, err
}

func getJSONContext(ctx context.Context, url string, maxBytes int64, label string, dst any) error {
	return getJSONWithClientContext(ctx, directHTTP, url, maxBytes, label, dst)
}

func getJSONWithClientContext(ctx context.Context, httpClient *http.Client, url string, maxBytes int64, label string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := readBoundedBody(resp.Body, maxBytes, label)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &controlHTTPError{
			statusCode: resp.StatusCode,
			err:        fmt.Errorf("%s: %s: %s", label, resp.Status, apiError(body)),
		}
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

type shipPlan struct {
	partial bool
	hashes  map[string]bool
}

func (p shipPlan) ships(entry proto.ManifestEntry) bool {
	return !p.partial || p.hashes[entry.SHA256]
}

func negotiateSnapshot(ctx context.Context, opts RunOptions, manifest proto.Manifest) (shipPlan, error) {
	refs := make([]proto.BlobRef, 0, len(manifest.Entries))
	for _, e := range manifest.Entries {
		if e.Type == proto.EntryFile {
			refs = append(refs, proto.BlobRef{SHA256: e.SHA256, Size: e.Size})
		}
	}
	if len(refs) == 0 {
		return shipPlan{}, nil
	}
	body, err := json.Marshal(proto.SnapshotDiffRequest{Blobs: refs})
	if err != nil {
		return shipPlan{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, controlRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.PeerURL+"/v0/snapshot/diff", bytes.NewReader(body))
	if err != nil {
		return shipPlan{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := directHTTP.Do(req)
	if err != nil {
		return shipPlan{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return shipPlan{}, nil
	default:
		return shipPlan{}, fmt.Errorf("snapshot negotiation: %s: %s", resp.Status, apiError(raw))
	}
	var diff proto.SnapshotDiffResponse
	if err := json.Unmarshal(raw, &diff); err != nil {
		return shipPlan{}, err
	}
	ship := make(map[string]bool, len(diff.Missing))
	for _, h := range diff.Missing {
		ship[h] = true
	}
	return shipPlan{partial: true, hashes: ship}, nil
}

func submit(opts RunOptions, jobID string, spec proto.Spec, manifest proto.Manifest, plan shipPlan) (proto.JobStatus, bool, error) {
	status, admissionUncertain, err := submitAttempts(opts, jobID, spec, manifest, plan)
	var responseErr *submitHTTPError
	if err == nil || !plan.partial || !errors.As(err, &responseErr) || responseErr.code != proto.ErrorCodeSnapshotCacheMiss {
		return status, admissionUncertain, err
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	fmt.Fprintln(stderr, "errand: runner evicted negotiated blobs; re-shipping the full snapshot")
	status, fallbackUncertain, err := submitAttempts(opts, jobID, spec, manifest, shipPlan{})
	return status, admissionUncertain || fallbackUncertain, err
}

func submitAttempts(opts RunOptions, jobID string, spec proto.Spec, manifest proto.Manifest, plan shipPlan) (proto.JobStatus, bool, error) {
	var lastErr error
	admissionUncertain := false
	for attempt := 0; attempt < 3; attempt++ {
		status, retryable, err := submitOnce(opts, jobID, spec, manifest, plan)
		if err == nil {
			return status, false, nil
		}
		lastErr = err
		if !retryable {
			return status, admissionUncertain, err
		}
		// A transport failure after the PUT began cannot distinguish a request
		// that never arrived from an admitted job whose response was lost.
		admissionUncertain = true
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
	}
	return proto.JobStatus{}, admissionUncertain, lastErr
}

func submitOnce(opts RunOptions, jobID string, spec proto.Spec, manifest proto.Manifest, plan shipPlan) (proto.JobStatus, bool, error) {
	var status proto.JobStatus
	if err := opts.selectionGuard.Verify(); err != nil {
		return status, false, &submitNotStartedError{err: err}
	}
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
			var shipFile func(proto.ManifestEntry) bool
			if plan.partial {
				shipFile = plan.ships
			}
			if err := snapshot.PackPartial(part, opts.Root, manifest, shipFile); err != nil {
				return err
			}
			if err := opts.selectionGuard.Verify(); err != nil {
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
	if opts.Project != "" {
		encoded, truncated := encodeProjectMetadata(opts.Project)
		req.Header.Set("X-Errand-Project-B64", encoded)
		if truncated {
			req.Header.Set("X-Errand-Project-Truncated", "1")
		}
	}
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
		payload := decodeAPIError(body)
		return status, false, &submitHTTPError{
			statusCode: resp.StatusCode, status: resp.Status, code: payload.Code,
			message: payload.Error, capacity: true,
		}
	default:
		payload := decodeAPIError(body)
		return status, false, &submitHTTPError{
			statusCode: resp.StatusCode, status: resp.Status, code: payload.Code, message: payload.Error,
		}
	}
}

func encodeProjectMetadata(project string) (string, bool) {
	project = strings.ToValidUTF8(project, "�")
	truncated := false
	if len(project) > maxProjectLabelBytes {
		truncated = true
		const marker = "…"
		project = project[:maxProjectLabelBytes-len(marker)]
		for !utf8.ValidString(project) {
			project = project[:len(project)-1]
		}
		project += marker
	}
	return base64.RawURLEncoding.EncodeToString([]byte(project)), truncated
}

type submitHTTPError struct {
	statusCode int
	status     string
	code       string
	message    string
	capacity   bool
}

type submitNotStartedError struct {
	err error
}

func (e *submitNotStartedError) Error() string { return e.err.Error() }
func (e *submitNotStartedError) Unwrap() error { return e.err }

func (e *submitHTTPError) Error() string {
	if e.capacity {
		if e.message == "" {
			return "runner capacity is full"
		}
		return "runner capacity is full: " + e.message
	}
	return fmt.Sprintf("submit rejected: %s: %s", e.status, e.message)
}

func submitDefinitelyRejected(err error) bool {
	var notStarted *submitNotStartedError
	if errors.As(err, &notStarted) {
		return true
	}
	var rejected *submitHTTPError
	if !errors.As(err, &rejected) {
		return false
	}
	return rejected.statusCode >= 400 && rejected.statusCode < 500 && rejected.statusCode != http.StatusRequestTimeout
}

type streamResult struct {
	status proto.JobStatus
	err    error
}

type streamDeadlineTracker struct {
	deadline time.Time
	phase    string
}

func newStreamDeadlineTracker(now time.Time, status proto.JobStatus) streamDeadlineTracker {
	t := streamDeadlineTracker{deadline: now.Add(time.Duration(proto.DefaultLimits().MaxRuntimeSec)*time.Second + streamDeadlineMargin)}
	t.observe(now, status)
	return t
}

func (t *streamDeadlineTracker) observe(now time.Time, status proto.JobStatus) {
	window := time.Duration(proto.DefaultLimits().MaxRuntimeSec)*time.Second + streamDeadlineMargin
	switch {
	case status.Result != nil:
		t.phase = "terminal"
	case status.State == proto.StateStaging || status.State == proto.StateQueued:
		t.phase = status.State
		t.deadline = now.Add(window)
	case status.State == proto.StateRunning && t.phase != proto.StateRunning:
		t.phase = proto.StateRunning
		t.deadline = now.Add(window)
	}
}

func streamUntilDetach(
	opts RunOptions,
	jobID string,
	initial proto.JobStatus,
	detach <-chan struct{},
) (proto.JobStatus, error, bool) {
	if detach == nil {
		status, err := streamContext(context.Background(), opts, jobID, initial)
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
	tracker := newStreamDeadlineTracker(time.Now(), initial)
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
		now := time.Now()
		if tracker.phase == proto.StateStaging || tracker.phase == proto.StateQueued {
			statusCtx, cancelStatus := context.WithTimeout(ctx, controlRequestTimeout)
			status, statusErr := getStatusContext(statusCtx, opts.PeerURL, jobID)
			cancelStatus()
			if statusErr == nil {
				now = time.Now()
				tracker.observe(now, status)
				terminalReplay = status.Result != nil
			}
		}
		if now.After(tracker.deadline) {
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

func followOnceContext(
	ctx context.Context,
	opts RunOptions,
	jobID string,
	last *int64,
) (proto.JobStatus, error) {
	url := fmt.Sprintf("%s/v0/jobs/%s/logs?from=%d", opts.PeerURL, jobID, *last)
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
				if err := json.Unmarshal(data.Bytes(), &remote); err != nil || remote.Message == "" {
					return proto.JobStatus{}, streamIntegrity(fmt.Errorf("invalid remote log error event"))
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

type streamIdleError struct {
	timeout time.Duration
}

func (e *streamIdleError) Error() string {
	return fmt.Sprintf("stream idle timeout after %s", e.timeout)
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

// detachOnEOFContext converts terminal EOF (normally Ctrl-D on an empty line)
// into a local detach request. Non-terminal EOF is ignored so scripts and
// background jobs do not silently change from attached to detached mode.
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
		return 0, &streamIdleError{timeout: r.timeout}
	}
}

// Info fetches a runner's measured facts.
func Info(peerURL string) (proto.Info, error) {
	var info proto.Info
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	err := getJSONContext(ctx, peerURL+"/v0/info", 1<<20, "runner info", &info)
	return info, err
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
	return postJSONResultContext(parent, url, v, "control request", nil)
}

func postJSONResultContext(parent context.Context, url string, v any, label string, dst any) error {
	return postJSONResultContextTimeout(parent, directHTTP, controlRequestTimeout, url, v, label, dst)
}

func postJSONResultContextTimeout(
	parent context.Context,
	httpClient *http.Client,
	timeout time.Duration,
	url string,
	v any,
	label string,
	dst any,
) error {
	var body io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
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
	if dst != nil {
		body, err := readBoundedBody(resp.Body, 1<<20, label)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("%s: decoding response: %w", label, err)
		}
	}
	return nil
}

func readBoundedBody(r io.Reader, maxBytes int64, label string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: reading response: %w", label, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%s: response exceeds %d bytes", label, maxBytes)
	}
	return body, nil
}

func apiError(body []byte) string {
	return decodeAPIError(body).Error
}

func decodeAPIError(body []byte) proto.APIError {
	var e proto.APIError
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e
	}
	return proto.APIError{Error: strings.TrimSpace(string(body))}
}
