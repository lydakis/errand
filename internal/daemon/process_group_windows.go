//go:build windows

package daemon

// captureProcessGroup records the job leader for the scope record. Windows
// needs no group identity: the job's Job Object dies with the daemon.
func captureProcessGroup(pid int) (*processGroupRecord, error) {
	return &processGroupRecord{PID: pid, Birth: "job-object"}, nil
}

// members is always empty: processes from a previous daemon were killed with
// that daemon's Job Objects.
func (g *processGroupRecord) members(bool) ([]int, error) {
	return nil, nil
}
