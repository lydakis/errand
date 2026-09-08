package setup

import (
	"context"
	"testing"
)

func TestManagedServicePID(t *testing.T) {
	for _, tt := range []struct {
		goos, output string
		want         int
	}{
		{"darwin", "gui/501/dev.lydakis.errand = {\n state = running\n pid = 81683\n}", 81683},
		{"darwin", "state = spawn scheduled\nlast exit code = 1", 0},
		{"darwin", "pid = invalid", 0},
		{"linux", "42\n", 42},
		{"linux", "0\n", 0},
	} {
		f := newFake(t, tt.goos)
		cmd := "launchctl print gui/501/dev.lydakis.errand"
		if tt.goos == "linux" {
			cmd = "systemctl --user show errand.service --property=MainPID --value"
		}
		f.cmdOutput[cmd] = tt.output
		pid, err := managedServicePID(context.Background(), f)
		if pid != tt.want || (err != nil) != (tt.want == 0) {
			t.Fatalf("%s %q: %d, %v", tt.goos, tt.output, pid, err)
		}
	}
}
