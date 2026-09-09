package daemon

import "fmt"

// A process group supplements the inherited marker for programs whose
// environment cannot be inspected (including macOS platform binaries).
// Birth identifies the original leader, so restart cannot claim a reused PID.
// Boot proves that every process from a previous machine boot is gone.
type processGroupRecord struct {
	PID   int    `json:"pid"`
	Birth string `json:"birth"`
	Boot  string `json:"boot,omitempty"`
}

type processGroupSnapshot struct {
	pids        []int
	leaderBirth string
}

func captureProcessGroup(pid int) (*processGroupRecord, error) {
	boot, err := processBootID()
	if err != nil {
		return nil, err
	}
	snapshot, err := inspectProcessGroup(pid)
	if err != nil {
		return nil, err
	}
	if snapshot.leaderBirth == "" {
		return nil, fmt.Errorf("process group leader identity is unavailable")
	}
	return &processGroupRecord{PID: pid, Birth: snapshot.leaderBirth, Boot: boot}, nil
}

func (g *processGroupRecord) members(owned bool) ([]int, error) {
	if g.PID <= 1 || g.Birth == "" {
		return nil, fmt.Errorf("invalid process group identity")
	}
	if g.Boot != "" {
		boot, err := processBootID()
		if err != nil {
			return nil, err
		}
		if boot != g.Boot {
			return nil, nil
		}
	}
	snapshot, err := inspectProcessGroup(g.PID)
	if err != nil {
		return nil, err
	}
	if snapshot.leaderBirth != "" && snapshot.leaderBirth != g.Birth {
		return nil, fmt.Errorf("process group leader identity changed; cleanup requires inspection")
	}
	if len(snapshot.pids) != 0 && snapshot.leaderBirth == "" && !owned {
		return nil, fmt.Errorf("process group leader is gone; surviving group ownership cannot be verified after restart")
	}
	return snapshot.pids, nil
}
