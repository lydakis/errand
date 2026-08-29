package daemon

import (
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// measureFacts reports what this runner can actually do right now —
// measured, not claimed. KVM is "openable by this user", not "exists".
func measureFacts() proto.Facts {
	f := proto.Facts{
		ObservedAt: time.Now(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		Tools:      map[string]string{},
	}
	if kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		kvm.Close()
		f.KVM = true
	}
	for _, tool := range []string{"git", "nix", "docker", "podman", "python3", "go", "cargo", "node"} {
		if p, err := exec.LookPath(tool); err == nil {
			f.Tools[tool] = p
		}
	}
	return f
}
