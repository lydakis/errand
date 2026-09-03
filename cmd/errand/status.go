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
)

type statusJSON struct {
	Peer           string                       `json:"peer"`
	Handle         string                       `json:"handle"`
	AutomaticApply *client.AutomaticApplyStatus `json:"automatic_apply,omitempty"`
	proto.JobDetails
}

func cmdStatus(args []string) int {
	return cmdStatusTo(args, os.Stdout, os.Stderr)
}

func cmdStatusTo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "errand status: exactly one HANDLE (peer/ULID) is required")
		return 2
	}
	peerURL, label, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 2
	}
	details, err := client.GetJobDetails(peerURL, jobID)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 1
	}
	label = cmpOr(label, peerURL)
	handle := label + "/" + jobID
	automaticApply, applyErr := client.GetAutomaticApplyStatus(peerURL, jobID)
	if applyErr != nil {
		fmt.Fprintf(stderr, "errand: reading automatic apply state: %v\n", applyErr)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(statusJSON{
			Peer: label, Handle: handle, AutomaticApply: automaticApply, JobDetails: details,
		}); err != nil {
			fmt.Fprintf(stderr, "errand: encoding job status: %v\n", err)
			return 1
		}
		if applyErr != nil {
			return 1
		}
		return 0
	}
	writeStatus(stdout, label, handle, details, automaticApply)
	if applyErr != nil {
		return 1
	}
	return 0
}

func writeStatus(
	w io.Writer,
	peer, handle string,
	details proto.JobDetails,
	automaticApply *client.AutomaticApplyStatus,
) {
	writeStatusField(w, "Job", handle)
	writeStatusField(w, "State", details.State)
	writeStatusField(w, "Runner", peer)
	if details.Project != "" {
		writeStatusField(w, "Project", details.Project)
	}
	writeStatusField(w, "Command", quoteArgv(details.Spec.Argv))
	workdir := details.Spec.Workdir
	if workdir == "" {
		workdir = "."
	}
	writeStatusField(w, "Workdir", workdir)
	writeStatusField(w, "Source", detailSource(details.Spec))
	writeStatusField(w, "Admitted", formatLocalTime(details.AdmittedAt))
	startedAt, durationMS := statusTiming(details)
	if startedAt != nil {
		writeStatusField(w, "Started", formatLocalTime(*startedAt))
		writeStatusField(w, "Duration", shortDuration(time.Duration(durationMS)*time.Millisecond))
	}
	if details.Result != nil && details.Result.FinishedAt != nil {
		writeStatusField(w, "Finished", formatLocalTime(*details.Result.FinishedAt))
	}
	if details.Result != nil {
		writeStatusField(w, "Result", statusResult(details.Result))
		writeStatusField(w, "Transaction", statusTransaction(details.Result))
	}
	writeStatusField(w, "Logs", statusLogs(details))
	if automaticApply != nil {
		writeStatusField(w, "Automatic apply", formatAutomaticApply(*automaticApply))
	}

	if details.Result != nil && details.Result.Changes != nil {
		fmt.Fprintln(w, "Workspace changes:")
		for _, path := range details.Result.Changes.Paths {
			fmt.Fprintf(w, "  %s\n", terminalSafeField(path))
		}
		if details.Result.Changes.PathsTruncated {
			fmt.Fprintf(w, "  … %d more paths\n", details.Result.Changes.PathCount-len(details.Result.Changes.Paths))
		}
		fmt.Fprintf(w, "  %d bytes\n", details.Result.Changes.Bytes)
	} else if details.Result != nil && !details.Result.ChangesOK {
		writeStatusField(w, "Workspace changes", "unknown (retention incomplete)")
	} else if details.Result != nil {
		writeStatusField(w, "Workspace changes", "none")
	}

	next := make([]string, 0, 3)
	if startedAt != nil || details.Result == nil || details.State == proto.StateAmbiguous {
		next = append(next, "errand attach "+terminalSafeField(handle))
	}
	if details.Result != nil && details.Result.Changes != nil {
		next = append(next, "errand fetch "+terminalSafeField(handle))
	}
	if details.Result == nil {
		next = append(next, "errand kill "+terminalSafeField(handle))
	}
	if len(next) > 0 {
		fmt.Fprintln(w, "Next:")
		for _, command := range next {
			fmt.Fprintf(w, "  %s\n", command)
		}
	}
}

func formatAutomaticApply(status client.AutomaticApplyStatus) string {
	switch status.State {
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

func writeStatusField(w io.Writer, label, value string) {
	if value == "" {
		value = "-"
	}
	fmt.Fprintf(w, "%s: %s\n", label, terminalSafeField(value))
}

func quoteArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ")
}

func detailSource(spec proto.ReceiptSpec) string {
	if spec.NoSnapshot {
		return "empty workspace"
	}
	if spec.GitCommit != "" {
		source := spec.GitCommit
		if spec.GitDirty {
			source += "+dirty"
		}
		return source
	}
	return "snapshot:" + spec.ManifestRoot
}

func statusResult(result *proto.Result) string {
	switch {
	case result.StartError != "":
		return "start failed: " + result.StartError
	case result.Signal != "":
		return "signal " + result.Signal
	case result.ExitCode != nil:
		return fmt.Sprintf("exit %d", *result.ExitCode)
	default:
		return "no process outcome"
	}
}

func statusTransaction(result *proto.Result) string {
	issues := make([]string, 0, 5)
	if !result.LogsComplete {
		issues = append(issues, "logs incomplete")
	}
	if !result.ChangesOK {
		issues = append(issues, "workspace changes not retained")
	}
	if !result.CleanupOK {
		issues = append(issues, "cleanup incomplete")
	}
	if result.LimitExceeded != "" {
		issues = append(issues, "limit "+result.LimitExceeded)
	}
	if result.TransactionError != "" {
		issues = append(issues, result.TransactionError)
	}
	if len(issues) == 0 {
		return "complete"
	}
	return "incomplete: " + strings.Join(issues, "; ")
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
