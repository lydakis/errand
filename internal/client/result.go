package client

import (
	"fmt"
	"io"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// exitCode applies the two-layer rule: preserve a nonzero remote outcome, but
// replace a successful remote outcome when the surrounding transaction failed.
func exitCode(st proto.JobStatus, stderr io.Writer, handle string) int {
	res := st.Result
	if res == nil {
		fmt.Fprintf(stderr, "errand: %s has no result (state %s)\n", handle, st.State)
		return ExitTransaction
	}

	ambiguous := st.State == proto.StateAmbiguous
	if ambiguous && res.ExitCode == nil && res.Signal == "" {
		details := []string{handle, "state=" + string(st.State)}
		if res.StartError != "" {
			details = append(details, "start error: "+res.StartError)
		}
		details = appendSecondaryResultFailures(details, res, true, "")
		writeTransactionIncomplete(stderr, details)
		return ExitTransaction
	}

	transactionOK := !ambiguous && res.CleanupOK && res.ChangesOK && res.LimitExceeded == "" &&
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
		details := make([]string, 0, 5)
		if ambiguous {
			details = append(details, "state ambiguous")
		}
		details = appendSecondaryResultFailures(details, res, true, "transaction error: ")
		for _, detail := range details {
			fmt.Fprintf(stderr, " (%s)", detail)
		}
		fmt.Fprintln(stderr)
		return signalExit(res.Signal, res.SignalNum)
	case res.ExitCode != nil && !transactionOK:
		details := make([]string, 0, 6)
		if ambiguous {
			details = append(details, "state=ambiguous")
		}
		details = append(details, fmt.Sprintf("remote_exit=%d", *res.ExitCode))
		details = appendSecondaryResultFailures(details, res, true, "")
		writeTransactionIncomplete(stderr, details)
		if *res.ExitCode == 0 {
			return ExitTransaction
		}
		return *res.ExitCode
	case res.ExitCode != nil:
		return *res.ExitCode
	case !transactionOK:
		details := []string{handle, "state=" + string(st.State)}
		details = appendSecondaryResultFailures(details, res, true, "")
		writeTransactionIncomplete(stderr, details)
		return ExitTransaction
	default:
		fmt.Fprintf(stderr, "errand: %s has no process outcome (state %s)\n", handle, st.State)
		return ExitTransaction
	}
}

// appendSecondaryResultFailures renders the transaction layer independently
// of the process outcome, including incomplete workspace change retention.
func appendSecondaryResultFailures(
	details []string,
	res *proto.Result,
	includeChanges bool,
	transactionPrefix string,
) []string {
	if includeChanges && !res.ChangesOK {
		details = append(details, "workspace changes incomplete")
	}
	if !res.CleanupOK {
		details = append(details, "cleanup failed")
	}
	if res.LimitExceeded != "" {
		details = append(details, "limit exceeded: "+res.LimitExceeded)
	}
	if !res.LogsComplete {
		details = append(details, "logs truncated")
	}
	if res.TransactionError != "" {
		details = append(details, transactionPrefix+res.TransactionError)
	}
	return details
}

func writeTransactionIncomplete(stderr io.Writer, details []string) {
	fmt.Fprintf(stderr, "errand: transaction incomplete (%s)\n", strings.Join(details, ", "))
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
