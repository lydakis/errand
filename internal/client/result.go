package client

import (
	"github.com/lydakis/errand/internal/proto"
)

// resultCode applies the two-layer rule: preserve a nonzero remote outcome,
// but replace a successful remote outcome when the surrounding transaction
// failed. The run view reports why; this only decides the exit code.
func resultCode(st proto.JobStatus) int {
	res := st.Result
	if res == nil {
		return ExitTransaction
	}
	ambiguous := st.State == proto.StateAmbiguous
	transactionOK := !ambiguous && res.CleanupOK && res.ChangesOK && res.LimitExceeded == "" &&
		res.StartError == "" && res.TransactionError == "" && res.LogsComplete
	switch {
	case ambiguous && res.ExitCode == nil && res.Signal == "":
		return ExitTransaction
	case res.StartError != "":
		return ExitTransaction
	case res.Signal != "":
		return signalExit(res.Signal, res.SignalNum)
	case res.ExitCode != nil && !transactionOK:
		if *res.ExitCode == 0 {
			return ExitTransaction
		}
		return *res.ExitCode
	case res.ExitCode != nil:
		return *res.ExitCode
	default:
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
