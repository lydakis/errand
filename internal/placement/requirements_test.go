package placement

import (
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestRequirements(t *testing.T) {
	f := proto.Facts{OS: "linux", Arch: "amd64", NumCPU: 8, KVM: true, Tools: map[string]string{"go": "/bin/go"}}
	for _, s := range []string{"*", "os=linux,arch=amd64,cpus>=4,kvm,go", "go,go"} {
		q, err := Parse(s)
		if err != nil || len(q.Missing(f)) != 0 {
			t.Fatalf("%q: %+v %v", s, q, err)
		}
	}
	if q, err := Parse("os=windows"); err != nil || len(q.Missing(proto.Facts{OS: "windows"})) != 0 || len(q.Missing(f)) == 0 {
		t.Errorf("os=windows: %+v %v", q, err)
	}
	for _, s := range []string{"", "os=plan9", "arch=x86_64", "cpus>=0", "cpus>=1.5", "go>=1", "os=linux,os=darwin", "*,go", "go,", "unknown"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	q, _ := Parse("os=darwin,cpus>=16,docker")
	if got := q.Missing(f); len(got) != 3 {
		t.Fatalf("missing: %v", got)
	}
}
func TestCapacityRanking(t *testing.T) {
	free := proto.Info{MaxJobs: 4, RunningJobs: 3}
	queued := proto.Info{MaxJobs: 2, RunningJobs: 2, QueuedJobs: 1}
	if !LessLoaded(free, queued) {
		t.Fatal("free slot must beat queued work")
	}
	if !LessLoaded(proto.Info{MaxJobs: 4, RunningJobs: 1}, proto.Info{MaxJobs: 2, RunningJobs: 1}) {
		t.Fatal("must normalize load by slots")
	}
	if !LessLoaded(proto.Info{MaxJobs: 2}, proto.Info{MaxJobs: 2, StagingJobs: 2}) {
		t.Fatal("staging reservations count")
	}
}

func TestGPURequirements(t *testing.T) {
	h100 := proto.GPU{Name: "NVIDIA H100 80GB HBM3", MemoryMiB: 81559}
	rtx := proto.GPU{Name: "NVIDIA GeForce RTX 4090", MemoryMiB: 24564}
	gb10 := proto.GPU{Name: "NVIDIA GB10"}
	f := proto.Facts{OS: "linux", GPUs: []proto.GPU{h100, h100, rtx}}
	for _, s := range []string{"gpu", "gpus>=3", "gpu=h100", "gpu=H100,gpus>=2,vram>=80", "gpu=rtx-4090", "gpu=rtx 4090", "vram>=24,gpus>=3"} {
		q, err := Parse(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if missing := q.Missing(f); len(missing) != 0 {
			t.Errorf("%q: %v", s, missing)
		}
	}
	for _, s := range []string{"gpus>=4", "gpu=a100", "gpu=h100,gpus>=3", "vram>=81", "gpu=4090,vram>=32"} {
		q, err := Parse(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if missing := q.Missing(f); len(missing) != 1 {
			t.Errorf("%q matched: %v", s, missing)
		}
	}
	q, _ := Parse("gpu=h100")
	if got := q.Missing(proto.Facts{}); len(got) != 1 || got[0] != "requires 1 GPU matching h100 (has none)" {
		t.Fatalf("no GPUs: %v", got)
	}
	q, _ = Parse("gpu=gb10")
	if got := q.Missing(proto.Facts{GPUs: []proto.GPU{gb10}}); len(got) != 0 {
		t.Fatalf("unified memory GPU: %v", got)
	}
	q, _ = Parse("gpu=gb10,vram>=1")
	if got := q.Missing(proto.Facts{GPUs: []proto.GPU{gb10}}); len(got) != 1 {
		t.Fatal("an unreported VRAM size must not satisfy vram>=N")
	}
	for _, s := range []string{"gpus>=0", "gpus>=x", "gpu=", "gpu=h100;rm", "vram>=0", "gpu=a,gpu=b", "gpus>=1,gpus>=2"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	if q, _ := Parse("*"); !q.Any() {
		t.Fatal("* must be the wildcard")
	}
	if q, _ := Parse("gpu"); q.Any() {
		t.Fatal("gpu is a requirement")
	}
	if got := DescribeGPUs(f.GPUs); got != "2x NVIDIA H100 80GB HBM3 (80 GiB), 1x NVIDIA GeForce RTX 4090 (24 GiB)" {
		t.Fatalf("describe: %q", got)
	}
}
