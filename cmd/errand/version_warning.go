package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lydakis/errand/internal/client"
)

// Best effort and advisory. A peer's installed CLI is not identified by its
// daemon's version, so a remote mismatch alone cannot prescribe a restart.
func warnRunnerVersion(peerURL, label string) {
	info, err := client.ProbeInfo(context.Background(), peerURL, 2*time.Second)
	if err != nil {
		return
	}
	warnKnownRunnerVersion(info.Version, cmpOr(label, peerURL))
}

func warnKnownRunnerVersion(runnerVersion, label string) {
	if runnerVersion == version {
		return
	}
	fmt.Fprintf(os.Stderr, "errand: warning: CLI %s; runner %s on %s. On that runner, check `errand version`; if its installed version differs from the daemon, run `errand setup` when idle.\n",
		terminalSafeField(version), terminalSafeField(runnerVersion), terminalSafeField(label))
}
