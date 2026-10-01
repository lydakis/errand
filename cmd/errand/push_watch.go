package main

import (
	"github.com/lydakis/errand/internal/termui"
)

// pushWatchDisplay keeps one transient line saying what watch is doing;
// finished syncs print as permanent lines above it.
type pushWatchDisplay struct {
	e     *termui.Stream
	quiet bool
	spin  *termui.Spinner
	state string
}

func newPushWatchDisplay(e *termui.Stream, quiet bool) *pushWatchDisplay {
	return &pushWatchDisplay{e: e, quiet: quiet}
}

func (d *pushWatchDisplay) clear() {
	if d.spin != nil {
		d.spin.Stop()
		d.spin = nil
	}
	d.state = ""
}

func (d *pushWatchDisplay) status(state string, err error) {
	if d.quiet || state == d.state {
		return
	}
	d.state = state
	if state == "reconnecting" && !d.e.Interactive() {
		if err != nil {
			d.e.Warnf("lost the runner (%v); reconnecting, pending changes kept", err)
		}
		return
	}
	message := map[string]string{
		"watching":     d.e.D("watching for changes…"),
		"sending":      "Sending changes…",
		"pending":      "Sending changes " + d.e.D("· more changes waiting"),
		"reconnecting": d.e.Paint("Reconnecting", termui.Yellow) + d.e.D(" · pending changes kept"),
		"resampling":   "Files changed while preparing; trying again…",
		"stopping":     "Stopping after this push finishes…",
	}[state]
	if message == "" {
		d.clear()
		return
	}
	if !d.e.Interactive() {
		return
	}
	if d.spin == nil || !d.spin.Active() {
		d.spin = d.e.Spin(message)
		return
	}
	d.spin.Set(message)
}
