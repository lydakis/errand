package main

import (
	"context"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/termui"
)

// Best effort and advisory. A peer's installed CLI is not identified by its
// daemon's version, so a remote mismatch alone cannot prescribe a restart.
func warnRunnerVersion(s *termui.Stream, peerURL, label string) {
	info, err := client.ProbeInfo(context.Background(), peerURL, 2*time.Second)
	if err != nil {
		return
	}
	warnKnownRunnerVersion(s, info.Version, cmpOr(label, peerURL))
}

func warnKnownRunnerVersion(s *termui.Stream, runnerVersion, label string) {
	if runnerVersion == version {
		return
	}
	s.Warnf("%s runs errand %s; this CLI is %s. Run errand setup on %s when it's idle.",
		terminalSafeField(label), terminalSafeField(runnerVersion), terminalSafeField(version), terminalSafeField(label))
}
