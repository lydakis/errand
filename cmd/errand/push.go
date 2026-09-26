package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/termui"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func cmdPush(args []string) int { return cmdPushTo(args, os.Stdout, os.Stderr) }
func cmdPushTo(args []string, out, stderr io.Writer) int {
	return cmdPushToContext(context.Background(), args, out, stderr)
}

func cmdPushToContext(ctx context.Context, args []string, out, stderr io.Writer) int {
	con := newConsole(out, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand push", flag.ContinueOnError)
	var settings runConfigFlags
	fs.StringVar(&settings.on, "on", "", "peer name, or local")
	fs.StringVar(&settings.url, "url", "", "peer base URL")
	fs.StringVar(&settings.profile, "profile", "", "use a named configuration profile")
	fs.StringVar(&settings.root, "workspace-root", "", "snapshot root containing the current directory")
	fs.StringVar(&settings.workspace, "workspace", "", "persistent workspace name (overrides the selected profile)")
	watch := fs.Bool("watch", false, "repeat push when local source files change (Ctrl-C stops watching)")
	apply := fs.Bool("apply", false, "merge staged local changes into the runner workspace")
	conflicts := fs.Bool("conflicts", false, "materialize text conflicts and apply clean changes")
	includeAll := fs.Bool("include-all", false, "allow a broad snapshot (never a filesystem root)")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	var output outputFlags
	output.bind(fs, "show transfer sizes, timings and push ids")
	if ok, code := parseFlags(fs, args, "push", out, e); !ok {
		return code
	}
	if fs.NArg() > 1 {
		return usageError(e, "push takes at most one path")
	}
	if settings.workspace == "" && settings.profile == "" {
		return needPushWorkspace(e)
	}
	if *conflicts && !*apply {
		return usageError(e, "--conflicts only works with --apply")
	}
	overrides, err := settings.overrides(fs)
	if err != nil {
		return usageError(e, "%v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failWith(e, 1, err, errorScope{})
	}
	effective, err := config.ResolvePush(cwd, overrides)
	if err != nil {
		return failWith(e, 2, err, errorScope{peer: settings.on})
	}
	workspace := effective.Workspace
	if workspace == "" {
		return needPushWorkspace(e)
	}
	if effective.Where != "" {
		e.Errorf("a persistent workspace lives on one runner; use --on instead of --where")
		return 2
	}
	peer := effective.URL
	if settings.url == "" {
		peer = client.ConfigureSSHPeer(peer, effective.Peer, effective.RemoteCommand, effective.RemoteSocket)
	}
	// Apply is always explicit for push. Run profiles' automatic-apply preference
	// controls successful jobs, not remote workspace mutation.
	opts := client.PushOptions{PeerURL: peer, Workspace: workspace, Root: effective.Root, Path: fs.Arg(0), Apply: *apply, MaterializeConflicts: *conflicts, IncludeAll: *includeAll}
	label := cmpOr(effective.Peer, peer)
	view := pushView{con: con, workspace: workspace, peer: label, apply: *apply, quiet: output.quiet || *jsonOutput, verbose: output.verbose, json: *jsonOutput, out: out, args: args}
	if *watch {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() { <-ctx.Done(); stop() }() // a second interrupt can force exit
		watcher := newPushWatchDisplay(e, view.quiet)
		defer watcher.clear()
		if !view.quiet {
			mode := "staging only"
			if *apply {
				mode = "applying"
			}
			root := filepath.Base(effective.Root)
			e.Say(termui.Dot, "Watching "+e.B(root)+" → "+e.B(workspace)+" "+e.D("on "+label+" · "+mode+" · Ctrl-C stops"))
		}
		err := client.WatchPush(ctx, opts, func(event client.PushWatchEvent) error {
			if event.Result == nil {
				watcher.status(event.State, event.Err)
				return nil
			}
			watcher.clear()
			return view.report(*event.Result, event.Stats, event.Err, event.State)
		})
		watcher.clear()
		if err != nil {
			return failWith(e, client.ExitTransaction, fmt.Errorf("watch stopped: %w", err), errorScope{peer: label, workspace: workspace})
		}
		if !view.quiet {
			e.Print("Stopped watching. " + e.D("Jobs on "+label+" keep running."))
		}
		return 0
	}
	var stats client.TransferStats
	opts.Stats = &stats
	var spin *termui.Spinner
	if !view.quiet {
		spin = e.Spin("Syncing with " + e.B(workspace) + " on " + e.B(label) + "…")
	}
	result, err := client.PushChanges(opts)
	if spin != nil {
		spin.Stop()
	}
	if writeErr := view.report(result, stats, err, ""); writeErr != nil {
		e.Errorf("%v", writeErr)
		return 1
	}
	if err != nil {
		// Errors go to stderr even with --json, which carries them on stdout too.
		var conflict *changes.MergeConflictError
		if errors.As(err, &conflict) {
			reportConflicts(e, conflict, "errand push --apply --conflicts "+termui.ShellQuote(withoutFlag(args, "apply", "conflicts")))
		} else {
			failWith(e, client.ExitTransaction, err, errorScope{peer: label, workspace: workspace})
		}
		return client.ExitTransaction
	}
	return 0
}

// pushView renders push results.
type pushView struct {
	con                         *termui.Console
	workspace, peer             string
	apply, quiet, verbose, json bool
	out                         io.Writer
	args                        []string
}

func (v pushView) report(result proto.PushResult, stats client.TransferStats, err error, watchState string) error {
	e := v.con.Err
	action := "staged"
	if v.apply {
		action = "applied"
	}
	if result.Recovered {
		action = "recovered"
	}
	if watchState == "unchanged" {
		action = "unchanged"
	}
	if v.json {
		report := struct {
			proto.PushResult
			transferReport
		}{result, newTransferReport(action, stats, err)}
		return json.NewEncoder(v.out).Encode(report)
	}
	if err != nil {
		return nil
	}
	if !v.apply && !result.Recovered && (!v.con.Out.Interactive() || v.verbose || v.quiet) {
		fmt.Fprintln(v.out, result.ID)
	}
	if v.quiet {
		return nil
	}
	target := e.B(v.workspace) + " on " + v.peer
	detail := termui.Bytes(stats.TransferredBytes) + " · " + termui.Duration(msDuration(stats.ElapsedMillis))
	if watchState != "" {
		clock := e.D(time.Now().Format("15:04:05"))
		switch {
		case len(result.Paths) == 0 || action == "unchanged":
			e.Print(clock + "  " + e.G(termui.OK) + " in sync")
		case action == "recovered":
			e.Print(clock + "  " + e.G(termui.OK) + " finished an earlier push " + e.D("· "+inlinePaths(e, result.Paths)))
		default:
			e.Print(clock + "  " + e.G(termui.Up) + " " + inlinePaths(e, result.Paths) + " " + e.D("· "+detail))
		}
		return nil
	}
	switch {
	case action == "recovered":
		e.Say(termui.OK, "Finished an earlier push to "+target+" "+e.D("("+termui.Things(len(result.Paths), "file", "files")+")"))
		e.Hintf("push again to send your current changes")
	case len(result.Paths) == 0:
		e.Say(termui.OK, "Already up to date: "+target+".")
	case v.apply:
		e.Say(termui.OK, "Synced "+e.B(termui.Things(len(result.Paths), "file", "files"))+" to "+target+": "+inlinePaths(e, result.Paths)+detailSuffix(e, v.verbose, detail))
	default:
		e.Say(termui.OK, "Staged "+e.B(termui.Things(len(result.Paths), "changed file", "changed files"))+" for "+target+" "+e.D("· "+detail))
		for i, p := range result.Paths {
			if i == 20 {
				e.Print("    " + e.D(fmt.Sprintf("… and %d more", len(result.Paths)-20)))
				break
			}
			e.Print("    " + terminalSafeField(p))
		}
		e.Next("errand push --apply "+termui.ShellQuote(withoutFlag(v.args, "apply")), "apply them")
	}
	return nil
}

// inlinePaths names up to three paths: "a, b and 4 more".
func inlinePaths(e *termui.Stream, paths []string) string {
	const shown = 3
	var names []string
	for i, p := range paths {
		if i == shown {
			break
		}
		names = append(names, terminalSafeField(p))
	}
	text := strings.Join(names, ", ")
	if more := len(paths) - len(names); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}

// withoutFlag drops boolean flags from args, to rebuild a suggested command.
func withoutFlag(args []string, names ...string) []string {
	drop := map[string]bool{}
	for _, n := range names {
		drop["-"+n], drop["--"+n] = true, true
	}
	var out []string
	for _, a := range args {
		if !drop[a] {
			out = append(out, a)
		}
	}
	return out
}

func needPushWorkspace(e *termui.Stream) int {
	e.Errorf("push needs a persistent workspace")
	e.Hintf("pass --workspace NAME, or a --profile that sets run.workspace; errand workspaces lists them")
	return 2
}
