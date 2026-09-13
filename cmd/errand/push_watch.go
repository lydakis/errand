package main

import (
	"fmt"
	"io"
	"os"
)

type pushWatchDisplay struct {
	out         io.Writer
	interactive bool
	state       string
	visible     bool
}

func newPushWatchDisplay(out io.Writer, json bool) *pushWatchDisplay {
	f, ok := out.(*os.File)
	return &pushWatchDisplay{out: out, interactive: !json && ok && fileTerminalColumns(f.Fd()) > 0}
}

func (d *pushWatchDisplay) clear() {
	if d.visible {
		fmt.Fprint(d.out, "\r\x1b[2K")
		d.visible = false
	}
}

func (d *pushWatchDisplay) status(state string) {
	if state == d.state && (!d.interactive || d.visible) {
		return
	}
	d.state = state
	message := map[string]string{
		"watching": "watching for changes", "sending": "sending changes",
		"pending": "sending changes; more changes pending", "reconnecting": "reconnecting; pending changes retained",
		"resampling": "source changed during preparation; retrying",
		"stopping":   "stopping after the current push finishes", "stopped": "watch stopped; remote jobs left running",
	}[state]
	d.clear()
	if d.interactive && state != "stopped" {
		fmt.Fprint(d.out, "errand: ", message)
		d.visible = true
	} else {
		fmt.Fprintln(d.out, "errand:", message)
	}
}
