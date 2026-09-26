package client

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
	"github.com/lydakis/errand/internal/termui"
)

// RunDisplay is the CLI-resolved context a run shows in its header line and
// how much it says.
type RunDisplay struct {
	UI       *termui.Console
	Quiet    bool
	Verbose  bool
	Project  string
	Workdir  string // relative to the workspace root; "" or "." is the root
	Bindings string // e.g. "2 caches, 1 artifact"; empty when none
	// Details are -v lines shown before any work starts (label, value).
	Details [][2]string
}

// runView renders one run: transient progress while preparing, a single
// header once the job is admitted, and a footer that says how it ended.
// runView is shared by the run's goroutines: upload progress arrives from the
// packer, interrupt narration from the signal controller, and queue lines
// from the queue watcher. mu serializes every method that touches its state.
type runView struct {
	mu      sync.Mutex
	con     *termui.Console
	ui      *termui.Stream
	quiet   bool
	verbose bool
	display RunDisplay
	spin    *termui.Spinner
	began   time.Time

	placement  string
	totalFiles int
	shipFiles  int
	shipBytes  int64 // file contents, as people count them
	streamSize int64 // the tar stream the upload counts, headers included
	upStart    time.Time
	upTime     time.Duration
	partial    bool
	queued     time.Time
	startedAt  time.Time
}

func newRunView(opts RunOptions) *runView {
	con := opts.Display.UI
	if con == nil {
		out, errOut := opts.Stdout, opts.Stderr
		if out == nil {
			out = os.Stdout
		}
		if errOut == nil {
			errOut = os.Stderr
		}
		con = termui.Plain(out, errOut)
	}
	// Project, directory, and binding labels come from the checkout and its
	// config, so they're quoted once here before any of them is drawn.
	display := opts.Display
	display.Project, display.Workdir, display.Bindings = termui.SafeText(display.Project), termui.SafeText(display.Workdir), termui.SafeText(display.Bindings)
	display.Details = make([][2]string, len(opts.Display.Details))
	for i, d := range opts.Display.Details {
		display.Details[i] = [2]string{d[0], termui.SafeText(d[1])}
	}
	return &runView{con: con, ui: con.Err, quiet: display.Quiet, verbose: display.Verbose, display: display, began: time.Now()}
}

func (v *runView) errf(format string, args ...any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.errLocked(format, args...)
}

func (v *runView) errLocked(format string, args ...any) {
	v.stopSpin()
	v.ui.Errorf(format, args...)
}

func (v *runView) warnf(format string, args ...any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.warnLocked(format, args...)
}

func (v *runView) warnLocked(format string, args ...any) {
	v.stopSpin()
	v.ui.Warnf(format, args...)
}

// report is the interrupt controller's narration.
func (v *runView) report(format string, args ...any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopSpin()
	v.ui.Say(termui.Warn, fmt.Sprintf(format, args...))
}

func (v *runView) detail(label, value string) {
	if v.verbose && !v.quiet {
		v.ui.Detail(label, value)
	}
}

func (v *runView) spinner(text string) {
	if v.quiet {
		return
	}
	if v.spin != nil && v.spin.Active() {
		v.spin.Set(text)
		return
	}
	v.spin = v.ui.Spin(text)
}

func (v *runView) stopSpin() {
	if v.spin != nil {
		v.spin.Stop()
	}
}

func (v *runView) peer(opts RunOptions) string { return peerLabel(opts.PeerName, opts.PeerURL) }

func (v *runView) preparing(opts RunOptions) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, d := range v.display.Details {
		v.detail(d[0], d[1])
	}
	switch {
	case opts.Workspace != "":
		v.spinner("Opening workspace " + v.ui.B(opts.Workspace) + "…")
	case opts.NoSnapshot:
		v.spinner("Preparing an empty workspace…")
	default:
		v.spinner("Snapshotting " + v.ui.B(cmpOrString(v.display.Project, "workspace")) + "…")
	}
}

func (v *runView) prepared(opts RunOptions, files int, bytes int64, elapsed time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.totalFiles = files
	switch {
	case opts.Workspace != "":
		v.detail("workspace", opts.Workspace+" (persistent; local files aren't uploaded)")
	case opts.NoSnapshot:
		v.detail("snapshot", "none; the job starts in an empty directory")
	default:
		v.detail("snapshot", termui.Things(files, "file", "files")+" · "+termui.Bytes(bytes)+" · hashed in "+termui.Duration(elapsed))
		v.spinner("Snapshotting " + v.ui.B(cmpOrString(v.display.Project, "workspace")) + " " + v.ui.D("· "+termui.Things(files, "file", "files")+" · "+termui.Bytes(bytes)))
	}
}

func (v *runView) selected(opts RunOptions) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.placement = opts.placement
	if opts.Workspace == "" && !opts.NoSnapshot {
		v.spinner("Syncing with " + v.ui.B(v.peer(opts)) + "…")
	} else {
		v.spinner("Starting on " + v.ui.B(v.peer(opts)) + "…")
	}
}

func (v *runView) negotiationFailed(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.warnLocked("couldn't check what the runner already has (%v); uploading everything", err)
}

func (v *runView) planned(opts RunOptions, plan shipPlan, manifest proto.Manifest, files int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.partial = plan.partial
	v.shipFiles, v.shipBytes = 0, 0
	var ships func(proto.ManifestEntry) bool
	if plan.partial {
		ships = plan.ships
	}
	for _, e := range manifest.Entries {
		if e.Type == proto.EntryFile && (ships == nil || ships(e)) {
			v.shipFiles++
			v.shipBytes += e.Size
		}
	}
	if opts.Workspace != "" || opts.NoSnapshot {
		return
	}
	v.streamSize = tarStreamSize(manifest, ships)
	peer := v.ui.B(v.peer(opts))
	if v.shipFiles == 0 {
		v.spinner("Syncing with " + peer + " " + v.ui.D("· all "+termui.Count(files)+" files already there"))
		return
	}
	v.upStart = time.Now()
	v.progressLocked(opts, 0)
}

func (v *runView) progress(opts RunOptions, done int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.progressLocked(opts, done)
}

func (v *runView) progressLocked(opts RunOptions, done int64) {
	if v.quiet || v.shipFiles == 0 || v.streamSize <= 0 {
		return
	}
	// The bar moves with the tar stream; its labels speak in file bytes
	// unless every shipped file is empty.
	done, total := min(done, v.streamSize), v.streamSize
	if v.shipBytes > 0 {
		done, total = int64(float64(done)/float64(v.streamSize)*float64(v.shipBytes)), v.shipBytes
	}
	cached := v.totalFiles - v.shipFiles
	suffix := " · " + termui.Things(v.shipFiles, "file", "files")
	if cached > 0 {
		suffix += " (" + termui.Count(cached) + " already there)"
	}
	if v.spin == nil || !v.spin.Active() {
		v.spin = v.ui.Spin("")
	}
	v.spin.Progress("Uploading to "+v.ui.B(v.peer(opts)), done, total, func(done, total int64) string {
		return termui.Bytes(done) + " of " + termui.Bytes(total) + v.ui.D(suffix)
	})
}

func (v *runView) reshipping() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.warnLocked("the runner couldn't restore its cached files; uploading everything")
}

func (v *runView) uploaded() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.uploadedLocked()
}

func (v *runView) uploadedLocked() {
	if !v.upStart.IsZero() && v.upTime == 0 {
		v.upTime = time.Since(v.upStart)
	}
}

// admitted prints the header: where the command runs and its job id.
func (v *runView) admitted(opts RunOptions, jobID string, gitInfo snapshot.GitInfo, files int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.uploadedLocked()
	v.stopSpin()
	if v.quiet {
		return
	}
	peer := v.peer(opts)
	handle := peer + "/" + jobID
	if v.verbose {
		switch {
		case opts.Workspace != "" || opts.NoSnapshot:
		case v.shipFiles == 0:
			v.detail("sync", peer+" already had all "+termui.Things(files, "file", "files"))
		default:
			v.detail("sync", "uploaded "+termui.Count(v.shipFiles)+" of "+termui.Things(files, "file", "files")+" · "+termui.Bytes(v.shipBytes)+" · "+termui.Duration(v.upTime))
		}
		source := "no git commit"
		if gitInfo.Commit != "" {
			source = "commit " + truncateString(gitInfo.Commit, 12)
			if gitInfo.Dirty {
				source += ", dirty"
			}
		}
		if opts.Workspace != "" {
			source = "workspace " + opts.Workspace
		}
		if v.ui.Interactive() {
			v.ui.Print(v.ui.D("· "+padLabel("job")+" ") + v.ui.ID(handle) + v.ui.D(" · "+source))
		} else {
			v.ui.Detail("job", handle+" · "+source)
		}
		if v.placement != "" {
			v.detail("placement", v.placement)
		}
		return
	}
	parts := []string{}
	if v.placement != "" {
		parts = append(parts, v.placement)
	}
	switch {
	case opts.Workspace != "":
		parts = append(parts, "workspace "+opts.Workspace)
	case opts.NoSnapshot:
		parts = append(parts, "empty workspace")
	default:
		source := v.display.Project
		if gitInfo.Commit != "" {
			commit := truncateString(gitInfo.Commit, 7)
			if gitInfo.Dirty {
				commit += " +dirty"
			}
			if source == "" {
				source = "commit " + commit
			} else {
				source += " @ " + commit
			}
		}
		if source != "" {
			parts = append(parts, source)
		}
	}
	if wd := v.display.Workdir; wd != "" && wd != "." {
		parts = append(parts, "in "+wd)
	}
	if v.display.Bindings != "" {
		parts = append(parts, v.display.Bindings)
	}
	parts = append(parts, "job")
	if v.ui.Interactive() {
		v.ui.Print(v.ui.G(termui.Here) + " " + v.ui.B(peer) + " " + v.ui.D("· "+strings.Join(parts, " · ")) + " " + v.ui.ID(termui.ShortID(jobID)))
		return
	}
	line := peer + " · " + strings.Join(parts, " · ") + " " + handle
	if v.shipFiles > 0 && opts.Workspace == "" && !opts.NoSnapshot {
		line += " · uploaded " + termui.Things(v.shipFiles, "file", "files") + " (" + termui.Bytes(v.shipBytes) + ")"
	}
	v.ui.Print("errand: " + line)
}

// waiting shows the queue while the job hasn't started.
func (v *runView) waiting(opts RunOptions, status proto.JobStatus) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.waitingLocked(opts, status)
}

func (v *runView) waitingLocked(opts RunOptions, status proto.JobStatus) {
	if v.quiet || status.State != proto.StateQueued {
		return
	}
	if v.queued.IsZero() {
		v.queued = time.Now()
		if !v.ui.Interactive() {
			// Logs can't show a live line; say once that the job is waiting.
			ahead := ""
			if status.QueueAhead != nil && *status.QueueAhead > 0 {
				ahead = " (" + termui.Things(*status.QueueAhead, "job", "jobs") + " ahead)"
			}
			v.ui.Print("errand: queued on " + v.peer(opts) + ahead + "; output follows when it starts")
			return
		}
	}
	text := v.ui.Paint("Queued", termui.Yellow) + " on " + v.ui.B(v.peer(opts))
	if status.QueueAhead != nil && *status.QueueAhead > 0 {
		text += " " + v.ui.D("· "+termui.Things(*status.QueueAhead, "job", "jobs")+" ahead")
	}
	text += " " + v.ui.D("· Ctrl-C cancels, Ctrl-D detaches")
	v.spinner(text)
}

// started marks the end of any queue wait.
func (v *runView) started() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.startedAt.IsZero() {
		return
	}
	v.startedAt = time.Now()
	v.stopSpin()
	if !v.queued.IsZero() {
		v.detail("started", termui.Clock(v.startedAt, v.startedAt)+" · waited "+termui.Duration(v.startedAt.Sub(v.queued))+" in the queue")
	}
}

// detachedBackground reports a -d submission.
func (v *runView) detachedBackground(handle, jobID, peer string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopSpin()
	if v.quiet {
		return
	}
	if v.ui.Interactive() {
		short := peer + "/" + termui.ShortID(jobID)
		v.ui.Say(termui.OK, "Started "+v.ui.ID(short)+" in the background")
		v.ui.Next("errand attach "+short, "follow the logs")
		return
	}
	v.ui.Print("errand: started " + handle + " in the background")
}

// detachedLive reports Ctrl-D while attached.
func (v *runView) detachedLive(handle, jobID, peer string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopSpin()
	if v.ui.Interactive() {
		short := peer + "/" + termui.ShortID(jobID)
		v.ui.Say(termui.OK, "Detached; "+v.ui.ID(short)+" keeps running")
		v.ui.Next("errand attach "+short, "follow it again")
		return
	}
	v.ui.Print("errand: detached; reattach with: errand attach " + handle)
}

// outcome is what the footer reports about workspace changes.
type footerChanges struct {
	summary     *proto.ChangeSummary
	applied     bool
	appliedList []PathChange
	pending     bool
}

// finished prints the footer line for a terminal status.
func (v *runView) finished(st proto.JobStatus, handle, peer, jobID string, changes footerChanges) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopSpin()
	res := st.Result
	if res == nil {
		v.errLocked("%s has no result (state %s)", handle, st.State)
		return
	}
	tty := v.ui.Interactive()
	ran := time.Duration(res.DurationMS) * time.Millisecond
	var line string
	failed := true
	switch {
	case res.StartError != "":
		line = "couldn't start: " + termui.SafeText(res.StartError)
	case res.Signal != "":
		line = "killed by " + signalName(res.Signal, res.SignalNum)
		if res.Started {
			line += " after " + termui.Duration(ran)
		} else {
			line += " before the command started"
		}
	case res.ExitCode != nil:
		line = fmt.Sprintf("exited %d in %s", *res.ExitCode, termui.Duration(ran))
		failed = *res.ExitCode != 0
	default:
		line = "finished without a process outcome"
	}
	problems := resultProblems(st)
	if v.quiet {
		// Quiet drops the verdict, not the reason a command never ran.
		if res.StartError != "" || res.ExitCode == nil && res.Signal == "" {
			v.ui.Errorf("%s", line)
		}
		for _, p := range problems {
			v.ui.Warnf("%s", p)
		}
		return
	}
	total := time.Since(v.began)
	extra := ""
	if v.upTime > time.Second && v.upTime*2 > total {
		extra = termui.Duration(total) + " total, " + termui.Duration(v.upTime) + " of it uploading"
	} else if v.verbose {
		extra = termui.Duration(total) + " total"
	}
	changeText := ""
	var next string
	if sum := changes.summary; sum != nil && sum.PathCount > 0 {
		switch {
		case changes.applied:
			changeText = "applied " + termui.Things(sum.PathCount, "file", "files") + " here: " + changeList(v.ui, changes.appliedList, sum)
		case changes.pending:
			changeText = "applying " + termui.Things(sum.PathCount, "changed file", "changed files") + " in the background"
		default:
			changeText = termui.Things(sum.PathCount, "file", "files") + " changed on " + peer + ": " + changeList(v.ui, nil, sum)
			next = "errand fetch --apply " + displayJob(v.ui, peer, jobID)
		}
	} else if v.verbose && res.ChangesOK {
		changeText = "no files changed"
	}
	if tty {
		glyph := v.ui.G(termui.OK)
		head := line
		if failed {
			glyph = v.ui.G(termui.Fail)
			head = v.ui.Paint(line, termui.Red)
		}
		out := glyph + " " + head
		if extra != "" {
			out += " " + v.ui.D("· "+extra)
		}
		if changeText != "" {
			out += " " + v.ui.D("·") + " " + changeText
		}
		v.ui.Print(out)
	} else {
		out := "errand: " + line
		if extra != "" {
			out += " (" + extra + ")"
		}
		if changeText != "" {
			out += "; " + termui.StripANSI(changeText)
		}
		v.ui.Print(out)
	}
	for _, p := range problems {
		v.ui.Warnf("%s", p)
	}
	if next != "" {
		v.ui.Next(next, "bring them here")
	}
}

func displayJob(s *termui.Stream, peer, jobID string) string {
	if s.Interactive() {
		return peer + "/" + termui.ShortID(jobID)
	}
	return peer + "/" + jobID
}

// changeList names up to three changed files, bold on a terminal.
func changeList(s *termui.Stream, kinds []PathChange, sum *proto.ChangeSummary) string {
	const shown = 3
	var names []string
	if len(kinds) > 0 {
		for i, c := range kinds {
			if i == shown {
				break
			}
			names = append(names, c.Letter(s)+" "+s.B(termui.SafeText(c.Path)))
		}
	} else {
		for i, p := range sum.Paths {
			if i == shown {
				break
			}
			names = append(names, s.B(termui.SafeText(p)))
		}
	}
	text := strings.Join(names, ", ")
	if more := sum.PathCount - len(names); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}

// resultProblems lists transaction failures around the process outcome.
func resultProblems(st proto.JobStatus) []string {
	res := st.Result
	var problems []string
	if st.State == proto.StateAmbiguous {
		problems = append(problems, "the runner couldn't confirm how this job ended")
	}
	if !res.ChangesOK {
		problems = append(problems, "changed files weren't kept")
	}
	if !res.CleanupOK {
		problems = append(problems, "cleanup on the runner didn't finish")
	}
	if res.LimitExceeded != "" {
		problems = append(problems, "hit the "+termui.SafeText(res.LimitExceeded)+" limit")
	}
	if !res.LogsComplete {
		problems = append(problems, "logs are incomplete")
	}
	if res.TransactionError != "" {
		problems = append(problems, termui.SafeText(res.TransactionError))
	}
	return problems
}

// signalName prefers SIGTERM-style names over descriptions like "terminated".
func signalName(description string, number int) string {
	names := map[int]string{1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 6: "SIGABRT", 9: "SIGKILL", 11: "SIGSEGV", 13: "SIGPIPE", 15: "SIGTERM"}
	if name, ok := names[number]; ok {
		return name
	}
	byDescription := map[string]string{"hangup": "SIGHUP", "interrupt": "SIGINT", "quit": "SIGQUIT", "aborted": "SIGABRT", "killed": "SIGKILL", "segmentation fault": "SIGSEGV", "broken pipe": "SIGPIPE", "terminated": "SIGTERM"}
	if name, ok := byDescription[description]; ok {
		return name
	}
	return description
}

// SignalName is signalName for the CLI.
func SignalName(description string, number int) string { return signalName(description, number) }

func padLabel(label string) string {
	if len(label) < 10 {
		return label + strings.Repeat(" ", 10-len(label))
	}
	return label
}

func truncateString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func cmpOrString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// tarStreamSize is the size of the tar stream an upload sends: the headers
// snapshot.PackPartial writes for every entry, padded contents of the files
// it ships, and the trailer.
func tarStreamSize(m proto.Manifest, ships func(proto.ManifestEntry) bool) int64 {
	var headers countingDiscard
	tw := tar.NewWriter(&headers)
	var contents int64
	for _, e := range m.Entries {
		hdr := &tar.Header{Name: e.Path, Mode: int64(e.Mode)}
		switch e.Type {
		case proto.EntryDir:
			hdr.Typeflag, hdr.Name = tar.TypeDir, e.Path+"/"
		case proto.EntrySymlink:
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, e.Target
		case proto.EntryFile:
			if ships != nil && !ships(e) {
				continue
			}
			// Written with no size so the next header needs no contents;
			// the padded contents are added separately.
			hdr.Typeflag = tar.TypeReg
			contents += (e.Size + 511) &^ 511
		default:
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return 0
		}
	}
	if err := tw.Close(); err != nil {
		return 0
	}
	return int64(headers) + contents
}

type countingDiscard int64

func (c *countingDiscard) Write(p []byte) (int, error) {
	*c += countingDiscard(len(p))
	return len(p), nil
}

// countingWriter reports bytes as they pass through, for upload progress.
type countingWriter struct {
	w     io.Writer
	add   func(int64)
	total int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.total += int64(n)
	if c.add != nil {
		c.add(c.total)
	}
	return n, err
}

// watchQueue shows the queue position while an admitted job waits for a
// slot. It polls only while the job is queued and stops at the first sign
// that it started.
func watchQueue(opts RunOptions, v *runView, jobID string, initial proto.JobStatus) func() {
	if initial.State != proto.StateQueued || v.quiet || opts.Detach {
		v.started()
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		// Most admissions pass through the queue for a moment; only a job
		// that is still waiting after a beat gets a queue line.
		timer := time.NewTimer(400 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
		}
		// The job may have started, and printed, during the grace period.
		status := initial
		if latest, err := getStatus(opts.PeerURL, jobID); err == nil {
			status = latest
		}
		for {
			if status.State != proto.StateQueued {
				v.started()
				return
			}
			if !v.showQueue(opts, status) {
				return
			}
			select {
			case <-done:
				return
			case <-time.After(time.Second):
			}
			if latest, err := getStatus(opts.PeerURL, jobID); err == nil {
				status = latest
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		v.started()
	}
}

// showQueue draws the queue line unless command output already took over
// the terminal, which means the job has started.
func (v *runView) showQueue(opts RunOptions, status proto.JobStatus) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.spin != nil && !v.spin.Active() && !v.queued.IsZero() {
		return false
	}
	v.waitingLocked(opts, status)
	return true
}

// attached prints attach's header: live or replay, and how old.
func (v *runView) attached(peer, jobID string, details proto.JobDetails) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.quiet {
		return
	}
	now := time.Now()
	short := displayJob(v.ui, peer, jobID)
	var line string
	switch {
	case details.Result != nil:
		when := "on " + peer
		if details.Result.FinishedAt != nil {
			when = termui.Ago(*details.Result.FinishedAt, now) + " on " + peer
		} else if details.Result.SettledAt != nil {
			when = termui.Ago(*details.Result.SettledAt, now) + " on " + peer
		}
		line = v.ui.D("▸ replaying") + " " + v.ui.ID(short) + " " + v.ui.D("· finished "+when)
	case details.StartedAt != nil:
		line = v.ui.D("▸ attached to") + " " + v.ui.ID(short) + " " + v.ui.D("· running for "+termui.Duration(now.Sub(*details.StartedAt))+" · Ctrl-D detaches, Ctrl-C interrupts")
	default:
		line = v.ui.D("▸ attached to") + " " + v.ui.ID(short) + " " + v.ui.D("· waiting to start · Ctrl-D detaches, Ctrl-C cancels")
	}
	if v.ui.Interactive() {
		v.ui.Print(line)
		return
	}
	v.ui.Print("errand: " + strings.TrimPrefix(termui.StripANSI(line), "▸ "))
}
