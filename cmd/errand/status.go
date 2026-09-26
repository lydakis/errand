package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

type statusJSON struct {
	Peer           string                       `json:"peer"`
	Handle         string                       `json:"handle"`
	AutomaticApply *client.AutomaticApplyStatus `json:"automatic_apply,omitempty"`
	*proto.JobDetails
}

func cmdStatus(args []string) int {
	return cmdStatusTo(args, os.Stdout, os.Stderr)
}

func cmdStatusTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand status", flag.ContinueOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "status", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return needHandle(e, "status")
	}
	peerURL, label, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		return failWith(e, handleErrorCode(err), err, handleScope(fs.Arg(0), label, *on))
	}
	label = cmpOr(label, peerURL)
	handle := label + "/" + jobID
	automaticApply, applyErr := client.GetAutomaticApplyStatus(peerURL, jobID)
	if applyErr != nil {
		e.Warnf("couldn't read the local apply state: %v", applyErr)
	}
	details, detailErr := client.GetJobDetails(peerURL, jobID)
	if detailErr != nil && automaticApply == nil {
		return failWith(e, 1, detailErr, errorScope{peer: label, job: jobID})
	}
	if *jsonOutput {
		var remote *proto.JobDetails
		if detailErr == nil {
			remote = &details
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(statusJSON{
			Peer: label, Handle: handle, AutomaticApply: automaticApply, JobDetails: remote,
		}); err != nil {
			e.Errorf("encoding job status: %v", err)
			return 1
		}
		if applyErr != nil || detailErr != nil {
			return 1
		}
		return 0
	}
	o := con.Out
	if output.quiet {
		if detailErr == nil {
			fmt.Fprintln(stdout, details.State)
		}
	} else if detailErr == nil {
		writeStatus(o, label, handle, details, automaticApply, output.verbose, time.Now())
	} else {
		o.Print(o.Paint(handle, termui.Bold, termui.Cyan))
		reason := "the runner is unavailable"
		if client.IsNotFound(detailErr) {
			reason = "the runner no longer keeps this job's record"
		}
		o.Print(o.G(termui.Warn) + " unknown: " + reason)
		o.Print("")
		o.Print("  " + o.D(padRight("Apply", 8)) + " " + formatAutomaticApply(*automaticApply))
		writeApplyRecoveryHint(o, handle, automaticApply)
	}
	if applyErr != nil || detailErr != nil {
		return 1
	}
	return 0
}

// statusLine is the one-line verdict under the handle.
func statusLine(s *termui.Stream, peer string, d proto.JobDetails, now time.Time) string {
	res := d.Result
	startedAt, durationMS := statusTiming(d)
	ran := time.Duration(durationMS) * time.Millisecond
	finished := ""
	if res != nil {
		at := res.FinishedAt
		if at == nil {
			at = res.SettledAt
		}
		if at != nil {
			finished = "finished " + termui.Ago(*at, now) + " on " + peer
		} else {
			finished = "on " + peer
		}
	}
	join := func(glyph termui.Glyph, head string, attrs []termui.Attr, rest ...string) string {
		line := s.G(glyph) + " " + s.Paint(head, attrs...)
		var tail []string
		for _, r := range rest {
			if r != "" {
				tail = append(tail, r)
			}
		}
		if len(tail) > 0 {
			line += " " + s.D("· "+strings.Join(tail, " · "))
		}
		return line
	}
	switch {
	case d.State == proto.StateAmbiguous && (res == nil || res.ExitCode == nil && res.Signal == ""):
		startErr := ""
		if res != nil && res.StartError != "" {
			startErr = "couldn't start: " + termui.SafeText(res.StartError)
		}
		return join(termui.Warn, "state unknown", []termui.Attr{termui.Yellow}, "the runner couldn't confirm how this job ended", startErr, finished)
	case res == nil && d.State == proto.StateRunning && startedAt != nil:
		return join(termui.Run, "running", []termui.Attr{termui.Green}, "for "+termui.Duration(ran), "started "+termui.Clock(*startedAt, now)+" on "+peer)
	case res == nil && d.State == proto.StateQueued:
		ahead := ""
		if d.QueueAhead != nil && *d.QueueAhead > 0 {
			ahead = termui.Things(*d.QueueAhead, "job", "jobs") + " ahead"
		}
		return join(termui.Wait, "queued", []termui.Attr{termui.Yellow}, "on "+peer, ahead, "admitted "+termui.Ago(d.AdmittedAt, now))
	case res == nil:
		return join(termui.Wait, d.State, []termui.Attr{termui.Yellow}, "on "+peer, "admitted "+termui.Ago(d.AdmittedAt, now))
	case res.StartError != "":
		return join(termui.Fail, "couldn't start: "+termui.SafeText(res.StartError), []termui.Attr{termui.Red}, finished)
	case res.Signal != "":
		head := "killed by " + client.SignalName(res.Signal, res.SignalNum)
		if !res.Started {
			return join(termui.Fail, head, []termui.Attr{termui.Red}, "before the command started", finished)
		}
		return join(termui.Fail, head, []termui.Attr{termui.Red}, "ran "+termui.Duration(ran), finished)
	case res.ExitCode != nil && *res.ExitCode == 0:
		return join(termui.OK, "exited 0", []termui.Attr{termui.Green}, "ran "+termui.Duration(ran), finished)
	case res.ExitCode != nil:
		return join(termui.Fail, fmt.Sprintf("exited %d", *res.ExitCode), []termui.Attr{termui.Red}, "ran "+termui.Duration(ran), finished)
	default:
		return join(termui.Warn, "no process outcome", []termui.Attr{termui.Yellow}, finished)
	}
}

func writeStatus(
	s *termui.Stream,
	peer, handle string,
	details proto.JobDetails,
	automaticApply *client.AutomaticApplyStatus,
	verbose bool,
	now time.Time,
) {
	s.Print(s.Paint(handle, termui.Bold, termui.Cyan))
	s.Print(statusLine(s, peer, details, now))
	if details.Result != nil {
		for _, problem := range statusProblems(details.Result) {
			s.Warnf("%s", problem)
		}
	}
	s.Print("")
	field := func(label, value string) {
		width := 8
		if verbose {
			width = 9
		}
		s.Print("  " + s.D(padRight(label, width)) + " " + value)
	}
	cont := func(value string) {
		width := 8
		if verbose {
			width = 9
		}
		s.Print("  " + strings.Repeat(" ", width) + " " + value)
	}
	spec := details.Spec
	field("Command", terminalSafeField(termui.ShellQuote(spec.Argv)))
	workdir := "workspace root"
	if spec.Workdir != "" && spec.Workdir != "." {
		workdir = "in " + spec.Workdir
	}
	if verbose {
		field("Project", terminalSafeField(cmpOr(details.Project, "-")))
		field("Workdir", terminalSafeField(workdir))
		switch {
		case spec.NoSnapshot:
			field("Source", "empty workspace")
		case spec.GitCommit != "":
			dirty := ""
			if spec.GitDirty {
				dirty = ", dirty"
			}
			field("Source", spec.GitCommit+dirty)
		}
		if spec.WorkspaceID != "" {
			field("Workspace", spec.WorkspaceID)
		}
		if spec.ManifestRoot != "" {
			field("Snapshot", truncateHash(spec.ManifestRoot)+" · ignores "+termui.Things(len(spec.Selection.Ignore), "pattern", "patterns"))
		}
		field("Admitted", termui.Timestamp(details.AdmittedAt))
		if startedAt, _ := statusTiming(details); startedAt != nil {
			wait := startedAt.Sub(details.AdmittedAt)
			field("Started", termui.Timestamp(*startedAt)+" · waited "+termui.Duration(wait))
		}
		if details.Result != nil && details.Result.FinishedAt != nil {
			field("Finished", termui.Timestamp(*details.Result.FinishedAt))
		}
		field("Logs", statusLogs(details))
		if details.Result != nil {
			cleanup := "ok"
			if !details.Result.CleanupOK {
				cleanup = "incomplete"
			}
			field("Cleanup", cleanup)
		}
		l := spec.Limits
		field("Limits", termui.Duration(time.Duration(l.MaxRuntimeSec)*time.Second)+" runtime · "+termui.Bytes(l.MaxLogBytes)+" logs · "+termui.Bytes(l.MaxWorkspaceBytes)+" workspace · "+termui.Bytes(l.MaxChangeBytes)+" changes")
	} else {
		parts := []string{}
		if details.Project != "" {
			parts = append(parts, details.Project)
		}
		parts = append(parts, workdir)
		switch {
		case spec.NoSnapshot:
			parts = append(parts, "empty workspace")
		case spec.WorkspaceID != "":
			parts = append(parts, "persistent workspace")
		case spec.GitCommit != "":
			source := truncateString(spec.GitCommit, 7)
			if spec.GitDirty {
				source += " +dirty"
			}
			parts = append(parts, source)
		case spec.ManifestRoot != "":
			parts = append(parts, "snapshot "+truncateHash(spec.ManifestRoot))
		}
		field("Project", terminalSafeField(strings.Join(parts, " · ")))
	}
	if automaticApply != nil {
		field("Apply", formatAutomaticApply(*automaticApply))
	}
	if res := details.Result; res != nil {
		switch {
		case res.Changes != nil:
			where := "still on " + peer
			if automaticApply != nil && automaticApply.State == "applied" {
				where = "applied here"
			}
			bundle := ""
			if verbose {
				bundle = " · bundle " + truncateHash(res.Changes.BundleRoot)
			}
			field("Changes", s.B(termui.Things(res.Changes.PathCount, "file", "files"))+s.D(" · "+termui.Bytes(res.Changes.Bytes)+", "+where+bundle))
			for _, path := range res.Changes.Paths {
				cont(terminalSafeField(path))
			}
			if res.Changes.PathsTruncated {
				cont(fmt.Sprintf("… and %d more", res.Changes.PathCount-len(res.Changes.Paths)))
			}
		case !res.ChangesOK:
			field("Changes", "not kept")
		default:
			field("Changes", "none")
		}
	}
	shortHandle := peer + "/" + termui.ShortID(details.ID)
	if !s.Interactive() {
		shortHandle = handle
	}
	var next [][2]string
	if details.Result != nil && details.Result.Changes != nil && (automaticApply == nil || automaticApply.State != "applied") {
		next = append(next, [2]string{"errand fetch --apply " + shortHandle, "bring the " + termui.Things(details.Result.Changes.PathCount, "file", "files") + " here"})
	}
	if startedAt, _ := statusTiming(details); startedAt != nil || details.Result == nil || details.State == proto.StateAmbiguous {
		why := "follow the logs"
		if details.Result != nil {
			why = "replay the logs"
		}
		next = append(next, [2]string{"errand attach " + shortHandle, why})
	}
	if details.Result == nil {
		next = append(next, [2]string{"errand kill " + shortHandle, "stop it"})
	}
	if len(next) > 0 {
		s.Print("")
		width := 0
		for _, n := range next {
			width = max(width, len(n[0]))
		}
		for _, n := range next {
			s.Next(padRight(n[0], width), n[1])
		}
	}
	writeApplyRecoveryHint(s, handle, automaticApply)
}

// statusProblems lists what went wrong around the process outcome.
func statusProblems(result *proto.Result) []string {
	var issues []string
	if !result.ChangesOK {
		issues = append(issues, "changed files weren't kept")
	}
	if !result.CleanupOK {
		issues = append(issues, "cleanup on the runner didn't finish")
	}
	if result.LimitExceeded != "" {
		issues = append(issues, "hit the "+termui.SafeText(result.LimitExceeded)+" limit")
	}
	if !result.LogsComplete {
		issues = append(issues, "logs are incomplete")
	}
	if result.TransactionError != "" {
		issues = append(issues, termui.SafeText(result.TransactionError))
	}
	return issues
}

func formatAutomaticApply(status client.AutomaticApplyStatus) string {
	status.Error = termui.SafeText(status.Error)
	switch status.State {
	case client.AutomaticApplyNeedsRecovery:
		if status.Error != "" {
			return "needs recovery; no active worker: " + status.Error
		}
		return "needs recovery; no active worker"
	case "pending":
		if status.Error != "" {
			return "pending retry: " + status.Error
		}
		return "pending in background"
	case "applying":
		return "applying in background"
	case "applied":
		return "applied"
	case "no_changes":
		return "complete; no workspace changes"
	case "skipped":
		return "not applied; job did not complete successfully"
	case "failed":
		if status.Error != "" {
			return "failed: " + status.Error
		}
		return "failed"
	default:
		return status.State
	}
}

func statusTiming(details proto.JobDetails) (*time.Time, int64) {
	if details.StartedAt != nil {
		return details.StartedAt, details.DurationMS
	}
	if details.Result != nil && details.Result.StartedAt != nil {
		return details.Result.StartedAt, details.Result.DurationMS
	}
	return nil, 0
}

func quoteArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ")
}

func statusLogs(details proto.JobDetails) string {
	if details.Result == nil {
		if details.StartedAt == nil {
			return "not available until the job starts"
		}
		return "streaming and retained"
	}
	if !details.Result.Started {
		if details.State == proto.StateAmbiguous {
			return "availability unknown; attach to inspect retained logs"
		}
		return "none; process did not start"
	}
	if details.Result.LogsComplete {
		return "retained (complete)"
	}
	return "retained (incomplete)"
}
