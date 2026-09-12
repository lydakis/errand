// Package placement matches runner requirements and ranks available capacity.
package placement

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

type Requirements struct {
	OS, Arch string
	CPUs     int
	KVM      bool
	Tools    []string
}

func Parse(s string) (Requirements, error) {
	var q Requirements
	if s == "*" {
		return q, nil
	}
	if len(s) == 0 || len(s) > 1024 {
		return q, fmt.Errorf("where requires a selector, or * for any configured runner")
	}
	seen := map[string]bool{}
	for _, term := range strings.Split(s, ",") {
		term = strings.TrimSpace(term)
		if seen[term] {
			continue
		}
		seen[term] = true
		switch {
		case strings.HasPrefix(term, "os="):
			v := strings.TrimPrefix(term, "os=")
			if q.OS != "" || (v != "linux" && v != "darwin") {
				return q, fmt.Errorf("invalid or conflicting where OS %q", term)
			}
			q.OS = v
		case strings.HasPrefix(term, "arch="):
			v := strings.TrimPrefix(term, "arch=")
			if q.Arch != "" || (v != "amd64" && v != "arm64") {
				return q, fmt.Errorf("invalid or conflicting where architecture %q", term)
			}
			q.Arch = v
		case strings.HasPrefix(term, "cpus>="):
			n, err := strconv.Atoi(strings.TrimPrefix(term, "cpus>="))
			if err != nil || n < 1 || n > 1048576 || q.CPUs != 0 {
				return q, fmt.Errorf("invalid or conflicting CPU requirement %q", term)
			}
			q.CPUs = n
		case term == "kvm":
			q.KVM = true
		case term == "git" || term == "nix" || term == "docker" || term == "podman" || term == "python3" || term == "go" || term == "cargo" || term == "node":
			q.Tools = append(q.Tools, term)
		default:
			return q, fmt.Errorf("unknown where requirement %q", term)
		}
	}
	return q, nil
}

func (q Requirements) Missing(f proto.Facts) []string {
	var missing []string
	if q.OS != "" && q.OS != f.OS {
		missing = append(missing, "requires os="+q.OS)
	}
	if q.Arch != "" && q.Arch != f.Arch {
		missing = append(missing, "requires arch="+q.Arch)
	}
	if q.CPUs > f.NumCPU {
		missing = append(missing, fmt.Sprintf("requires cpus>=%d (has %d)", q.CPUs, f.NumCPU))
	}
	if q.KVM && !f.KVM {
		missing = append(missing, "requires usable kvm")
	}
	for _, tool := range q.Tools {
		if f.Tools[tool] == "" {
			reason := "requires usable " + tool
			if detail := f.ToolErrors[tool]; detail != "" {
				reason += ": " + detail
			}
			missing = append(missing, reason)
		}
	}
	return missing
}

// Counts include upload reservations. Equal scores are deliberately left equal
// so callers can shuffle ties instead of sending every client to the first alias.
func LessLoaded(a, b proto.Info) bool {
	count := func(i proto.Info) int { return i.StagingJobs + i.StartingJobs + i.RunningJobs + i.QueuedJobs }
	ac, bc := count(a), count(b)
	af, bf := ac < a.MaxJobs, bc < b.MaxJobs
	if af != bf {
		return af
	}
	return float64(ac)/float64(a.MaxJobs) < float64(bc)/float64(b.MaxJobs)
}
