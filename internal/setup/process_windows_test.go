//go:build windows

package setup

import (
	"os"
	"strings"
	"testing"
)

func TestProcessHelpersDescribeThisProcessOnWindows(t *testing.T) {
	sid, err := currentUserSID()
	if err != nil || !strings.HasPrefix(sid, "S-1-") {
		t.Fatalf("currentUserSID = %q, %v", sid, err)
	}
	image, err := processImage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !(RealSystem{}).SameFile(image, os.Args[0]) {
		exe, _ := os.Executable()
		if !(RealSystem{}).SameFile(image, exe) {
			t.Fatalf("processImage = %q, executable %q", image, exe)
		}
	}
}
