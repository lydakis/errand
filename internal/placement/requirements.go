// Package placement matches runner requirements and ranks available capacity.
package placement

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

type Requirements struct {
	OS, Arch string
	CPUs     int
	KVM      bool
	Tools    []string
	// GPU is set by any GPU term. GPUs, GPUModel and VRAMGiB narrow which
	// GPUs count; at least max(GPUs, 1) of them must satisfy both filters.
	GPU      bool
	GPUs     int
	GPUModel string
	VRAMGiB  int
}

// Any reports whether the selector is the bare wildcard, which accepts every
// eligible runner and must never cause capacity to be rented.
func (q Requirements) Any() bool {
	return q.OS == "" && q.Arch == "" && q.CPUs == 0 && !q.KVM && len(q.Tools) == 0 && !q.GPU
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
			if q.OS != "" || (v != "linux" && v != "darwin" && v != "windows") {
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
		case term == "gpu":
			q.GPU = true
		case strings.HasPrefix(term, "gpus>="):
			n, err := strconv.Atoi(strings.TrimPrefix(term, "gpus>="))
			if err != nil || n < 1 || n > 1024 || q.GPUs != 0 {
				return q, fmt.Errorf("invalid or conflicting GPU count %q", term)
			}
			q.GPU, q.GPUs = true, n
		case strings.HasPrefix(term, "gpu="):
			v := normalizeGPUName(strings.TrimPrefix(term, "gpu="))
			if v == "" || len(v) > 64 || q.GPUModel != "" || strings.Trim(v, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
				return q, fmt.Errorf("invalid or conflicting GPU model %q", term)
			}
			q.GPU, q.GPUModel = true, v
		case strings.HasPrefix(term, "vram>="):
			n, err := strconv.Atoi(strings.TrimPrefix(term, "vram>="))
			if err != nil || n < 1 || n > 65536 || q.VRAMGiB != 0 {
				return q, fmt.Errorf("invalid or conflicting VRAM requirement %q (GiB per GPU)", term)
			}
			q.GPU, q.VRAMGiB = true, n
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
	if q.GPU {
		if reason := q.missingGPUs(f.GPUs); reason != "" {
			missing = append(missing, reason)
		}
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

func (q Requirements) missingGPUs(gpus []proto.GPU) string {
	need := max(q.GPUs, 1)
	matched := 0
	for _, g := range gpus {
		if q.GPUModel != "" && !strings.Contains(normalizeGPUName(g.Name), q.GPUModel) {
			continue
		}
		if q.VRAMGiB > 0 && GiB(g.MemoryMiB) < q.VRAMGiB {
			continue
		}
		matched++
	}
	if matched >= need {
		return ""
	}
	want := fmt.Sprintf("%d GPU", need)
	if need != 1 {
		want += "s"
	}
	if q.GPUModel != "" {
		want += " matching " + q.GPUModel
	}
	if q.VRAMGiB > 0 {
		want += fmt.Sprintf(" with %d GiB", q.VRAMGiB)
	}
	return fmt.Sprintf("requires %s (has %s)", want, DescribeGPUs(gpus))
}

// GiB rounds to the nearest GiB: an "80 GB" H100 reports 81559 MiB.
func GiB(mib int) int { return int(math.Round(float64(mib) / 1024)) }

// DescribeGPUs summarizes identical GPUs as "2x NVIDIA H100 80GB HBM3 (80 GiB)".
func DescribeGPUs(gpus []proto.GPU) string {
	if len(gpus) == 0 {
		return "none"
	}
	var parts []string
	counts := map[proto.GPU]int{}
	for _, g := range gpus {
		counts[g]++
	}
	seen := map[proto.GPU]bool{}
	for _, g := range gpus {
		if seen[g] {
			continue
		}
		seen[g] = true
		part := fmt.Sprintf("%dx %s", counts[g], g.Name)
		if g.MemoryMiB > 0 {
			part += fmt.Sprintf(" (%d GiB)", GiB(g.MemoryMiB))
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// Spaces, hyphens and underscores vary between vendors' names and what people
// type: "RTX 4090", "rtx-4090" and "rtx4090" all match.
func normalizeGPUName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '-' || r == '_':
			return -1
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return r
	}, s)
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

// CheaperOffer orders offers for renting: an offer with a price, even a
// price of zero, before one without, then the lower price.
func CheaperOffer(a, b proto.Offer) bool {
	if (a.PricePerHour != nil) != (b.PricePerHour != nil) {
		return a.PricePerHour != nil
	}
	return a.PricePerHour != nil && *a.PricePerHour < *b.PricePerHour
}
