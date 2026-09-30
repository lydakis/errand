package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const processScopeEnv = "ERRAND_PROCESS_SCOPE"

// scopeRecord is the persisted form of a job's scope, written to the job
// directory before the process starts so a restarted daemon can find and
// settle survivors during reconciliation.
type scopeRecord struct {
	Token           string              `json:"token"`
	SharedWorkspace bool                `json:"shared_workspace,omitempty"`
	Group           *processGroupRecord `json:"group,omitempty"`
}

func newProcessScope(workdir string, cacheDirs ...string) (*processScope, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	return newProcessScopeWithToken(hex.EncodeToString(raw[:]), workdir, cacheDirs...)
}

// resumeProcessScope reconstructs a scope from its persisted token, for
// reconciliation after a daemon restart.
func resumeProcessScope(token, workdir string, cacheDirs ...string) (*processScope, error) {
	if err := validateProcessScopeToken(token); err != nil {
		return nil, err
	}
	return newProcessScopeWithToken(token, workdir, cacheDirs...)
}

func validateProcessScopeToken(token string) error {
	if len(token) != hex.EncodedLen(16) {
		return fmt.Errorf("process scope token must be 32 lowercase hexadecimal characters")
	}
	raw, err := hex.DecodeString(token)
	if err != nil || hex.EncodeToString(raw) != token {
		return fmt.Errorf("process scope token must be 32 lowercase hexadecimal characters")
	}
	return nil
}

// A process group supplements the inherited marker for programs whose
// environment cannot be inspected (including macOS platform binaries).
// Birth identifies the original leader, so restart cannot claim a reused PID.
// Boot proves that every process from a previous machine boot is gone.
type processGroupRecord struct {
	PID   int    `json:"pid"`
	Birth string `json:"birth"`
	Boot  string `json:"boot,omitempty"`
}
