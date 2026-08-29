package daemon

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const processScopeEnv = "ERRAND_PROCESS_SCOPE"

// processScope tags every job descendant with an unguessable inherited
// marker. This lets the daemon find descendants that create a new session or
// process group. It is lifecycle containment for same-user cooperative jobs,
// not a security boundary against a process that deliberately scrubs its
// environment.
type processScope struct {
	token    string
	psPath   string
	workdir  string
	lsofPath string
	procRoot string
}

// scopeRecord is the persisted form of a job's scope, written to the job
// directory before the process starts so a restarted daemon can find and
// settle survivors during reconciliation.
type scopeRecord struct {
	Token string `json:"token"`
}

func newProcessScope(workdir string) (*processScope, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	return newProcessScopeWithToken(hex.EncodeToString(raw[:]), workdir)
}

// resumeProcessScope reconstructs a scope from its persisted token, for
// reconciliation after a daemon restart.
func resumeProcessScope(token, workdir string) (*processScope, error) {
	if err := validateProcessScopeToken(token); err != nil {
		return nil, err
	}
	return newProcessScopeWithToken(token, workdir)
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

func newProcessScopeWithToken(token, workdir string) (*processScope, error) {
	s := &processScope{token: token, workdir: workdir, procRoot: "/proc"}
	if runtime.GOOS != "linux" {
		psPath, err := exec.LookPath("ps")
		if err != nil {
			return nil, fmt.Errorf("process scope requires ps: %w", err)
		}
		s.psPath = psPath
	}
	if runtime.GOOS == "darwin" {
		lsofPath, err := exec.LookPath("lsof")
		if err != nil {
			return nil, fmt.Errorf("process scope requires lsof on macOS: %w", err)
		}
		s.lsofPath = lsofPath
	}
	if _, err := s.pids(); err != nil {
		return nil, fmt.Errorf("inspecting process scope: %w", err)
	}
	return s, nil
}

func (s *processScope) env() string {
	return processScopeEnv + "=" + s.token
}

func (s *processScope) pids() ([]int, error) {
	if runtime.GOOS == "linux" {
		return s.linuxPIDs()
	}
	// On BSD ps, e includes the environment and ww prevents truncation.
	out, err := exec.Command(s.psPath, "eww", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	marker := s.env()
	seen := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err == nil && pid > 1 && pid != os.Getpid() {
			seen[pid] = true
		}
	}
	cwdPIDs, err := s.cwdPIDs()
	if err != nil {
		return nil, err
	}
	for _, pid := range cwdPIDs {
		if pid > 1 && pid != os.Getpid() {
			seen[pid] = true
		}
	}
	pids := make([]int, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	return pids, nil
}

func (s *processScope) linuxPIDs() ([]int, error) {
	procRoot := s.procRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	marker := []byte(s.env())
	seen := map[int]bool{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		pidDir := filepath.Join(procRoot, entry.Name())
		if environ, err := os.ReadFile(filepath.Join(pidDir, "environ")); err == nil && hasEnvEntry(environ, marker) {
			seen[pid] = true
		}
		if cwd, err := os.Readlink(filepath.Join(pidDir, "cwd")); err == nil && withinDir(s.workdir, cwd) {
			seen[pid] = true
		}
	}
	pids := make([]int, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	return pids, nil
}

func hasEnvEntry(environ, want []byte) bool {
	for _, entry := range bytes.Split(environ, []byte{0}) {
		if bytes.Equal(entry, want) {
			return true
		}
	}
	return false
}

// cwdPIDs is weaker on macOS than on Linux: lsof matches processes whose
// cwd is exactly the workspace directory, while the Linux /proc scan
// matches any cwd within it. A darwin job that chdirs into a subdirectory
// and scrubs the env marker evades this scan where its Linux twin would
// not. Do not assume platform parity when reasoning about scope coverage.
func (s *processScope) cwdPIDs() ([]int, error) {
	if runtime.GOOS == "darwin" && s.lsofPath != "" {
		out, err := exec.Command(s.lsofPath, "-a", "-d", "cwd", "-Fp", "--", s.workdir).Output()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				return nil, nil
			}
			return nil, err
		}
		var pids []int
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(line, "p") {
				continue
			}
			if pid, err := strconv.Atoi(strings.TrimPrefix(line, "p")); err == nil {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	return nil, nil
}

func withinDir(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *processScope) signalEscaped(sig syscall.Signal, originalPGID int) error {
	pids, err := s.pids()
	if err != nil {
		return err
	}
	var joined error
	for _, pid := range pids {
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == originalPGID {
			continue
		}
		if err := syscall.Kill(pid, sig); err != nil && err != syscall.ESRCH {
			joined = errors.Join(joined, fmt.Errorf("signal scoped pid %d: %w", pid, err))
		}
	}
	return joined
}

// cleanup SIGKILLs every pid still in scope until none remain or the
// deadline passes. It returns the set of pids it killed so the caller can
// record them in the receipt: the scan can catch an innocent same-user
// process that merely chdir'd into the workspace, and when that happens
// the receipt should explain it.
func (s *processScope) cleanup(timeout time.Duration) ([]int, error) {
	deadline := time.Now().Add(timeout)
	killed := map[int]bool{}
	collect := func() []int {
		pids := make([]int, 0, len(killed))
		for pid := range killed {
			pids = append(pids, pid)
		}
		return pids
	}
	for {
		pids, err := s.pids()
		if err != nil {
			return collect(), err
		}
		if len(pids) == 0 {
			return collect(), nil
		}
		var joined error
		for _, pid := range pids {
			err := syscall.Kill(pid, syscall.SIGKILL)
			if err == nil || err == syscall.ESRCH {
				killed[pid] = true
				continue
			}
			joined = errors.Join(joined, fmt.Errorf("kill scoped pid %d: %w", pid, err))
		}
		if joined != nil {
			return collect(), joined
		}
		if time.Now().After(deadline) {
			return collect(), fmt.Errorf("process scope still contains pids %v", pids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
