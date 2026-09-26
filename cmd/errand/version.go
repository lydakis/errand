package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/termui"
)

// Set at release with -ldflags "-X main.commit=... -X main.buildDate=...".
var (
	commit    = ""
	buildDate = ""
)

func cmdVersion(args []string) int { return cmdVersionTo(args, os.Stdout, os.Stderr) }

func cmdVersionTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	fs := flag.NewFlagSet("errand version", flag.ContinueOnError)
	var output outputFlags
	output.bind(fs, "also show the build and your runners' versions")
	if ok, code := parseFlags(fs, args, "version", stdout, con.Err); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(con.Err, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if !output.verbose {
		fmt.Fprintln(stdout, "errand", version)
		return 0
	}
	o := con.Out
	fmt.Fprintln(stdout, "errand "+version+" "+o.D("· "+strings.Join(buildFacts(), " · ")))
	spin := con.Err.Spin("Asking runners…")
	versions := runnerVersions()
	spin.Stop()
	if len(versions) > 0 {
		var parts []string
		for _, rv := range versions {
			mark := o.G(termui.OK)
			switch {
			case rv.err != nil:
				parts = append(parts, rv.name+" "+o.D("unreachable"))
				continue
			case rv.version != version:
				mark = o.G(termui.Warn)
			}
			parts = append(parts, rv.name+" "+rv.version+" "+mark)
		}
		fmt.Fprintln(stdout, o.D("runners:")+" "+strings.Join(parts, "  "))
	}
	return 0
}

// buildFacts are the commit, build date and toolchain, from ldflags or the
// module's embedded VCS stamp.
func buildFacts() []string {
	rev, date, dirty := commit, buildDate, false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if rev == "" {
					rev = s.Value
				}
			case "vcs.time":
				if date == "" {
					date = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	var facts []string
	if rev != "" {
		if len(rev) > 7 {
			rev = rev[:7]
		}
		if dirty {
			rev += " +dirty"
		}
		facts = append(facts, rev)
	}
	if date != "" {
		if t, err := time.Parse(time.RFC3339, date); err == nil {
			date = t.Local().Format("Jan 2 2006")
		}
		facts = append(facts, "built "+date)
	}
	return append(facts, runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH)
}

type runnerVersion struct {
	name, version string
	err           error
}

func runnerVersions() []runnerVersion {
	targets, _, err := peerTargets("", "")
	if err != nil {
		return nil
	}
	out := make([]runnerVersion, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := client.ProbeInfo(context.Background(), t.url, 2*time.Second)
			out[i] = runnerVersion{name: t.name, version: info.Version, err: err}
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
