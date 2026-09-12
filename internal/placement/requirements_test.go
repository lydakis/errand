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
