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
	"syscall"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func cmdPush(args []string) int { return cmdPushTo(args, os.Stdout, os.Stderr) }
func cmdPushTo(args []string, out, stderr io.Writer) int {
	return cmdPushToContext(context.Background(), args, out, stderr)
}

func cmdPushToContext(ctx context.Context, args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("errand push", flag.ContinueOnError)
	fs.SetOutput(stderr)
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
	setFlagUsage(fs, "errand push [--workspace NAME] [--profile NAME] [options] [PATH]")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if settings.workspace == "" && settings.profile == "" || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "errand push: select a workspace with --workspace or --profile, and at most one changed PATH")
		return 2
	}
	if *conflicts && !*apply {
		fmt.Fprintln(stderr, "errand push: --conflicts requires --apply")
		return 2
	}
	overrides, err := settings.overrides(fs)
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return 1
	}
	effective, err := config.ResolvePush(cwd, overrides)
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return 2
	}
	workspace := effective.Workspace
	if workspace == "" {
		fmt.Fprintln(stderr, "errand push: --workspace NAME or a profile with run.workspace is required")
		return 2
	}
	if effective.Where != "" {
		fmt.Fprintln(stderr, "errand push: persistent workspace requires a pinned peer; use --on instead of where")
		return 2
	}
	peer := effective.URL
	if settings.url == "" {
		peer = client.ConfigureSSHPeer(peer, effective.Peer, effective.RemoteCommand, effective.RemoteSocket)
	}
	// Apply is always explicit for push. Run profiles' automatic-apply preference
	// controls successful jobs, not remote workspace mutation.
	opts := client.PushOptions{PeerURL: peer, Workspace: workspace, Root: effective.Root, Path: fs.Arg(0), Apply: *apply, MaterializeConflicts: *conflicts, IncludeAll: *includeAll}
	target := "to workspace " + workspace + " on " + cmpOr(effective.Peer, peer)
	if *watch {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() { <-ctx.Done(); stop() }() // a second interrupt can force exit
		display := newPushWatchDisplay(stderr, *jsonOutput)
		defer display.clear()
		mode := "staging only; running files will not change"
		if *apply {
			mode = "applying changes; application readiness not checked"
		}
		fmt.Fprintf(stderr, "errand: watching %s %s · %s · Ctrl-C stops watching\n", terminalSafeField(effective.Root), terminalSafeField(target), mode)
		err := client.WatchPush(ctx, opts, func(event client.PushWatchEvent) error {
			if event.Result == nil {
				display.status(event.State)
				return nil
			}
			display.clear()
			return reportPushResult(out, stderr, *event.Result, event.Stats, event.Err, *apply, *jsonOutput, event.State, target)
		})
		display.clear()
		nameEarlierStateRecovery(err, settings, effective.Peer)
		if err != nil {
			fmt.Fprintln(stderr, "errand: watch stopped:", err)
			return client.ExitTransaction
		}
		return 0
	}
	var stats client.TransferStats
	opts.Stats = &stats
	result, err := client.PushChanges(opts)
	nameEarlierStateRecovery(err, settings, effective.Peer)
	if writeErr := reportPushResult(out, stderr, result, stats, err, *apply, *jsonOutput, "", target); writeErr != nil {
		fmt.Fprintln(stderr, "errand:", writeErr)
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return client.ExitTransaction
	}
	return 0
}

// The recovery command for a workspace recorded by an earlier errand repeats
// this push's peer and profile, so it recreates the same workspace.
func nameEarlierStateRecovery(err error, settings runConfigFlags, peer string) {
	var earlier *client.EarlierTransferStateError
	if errors.As(err, &earlier) {
		earlier.URL, earlier.Peer, earlier.Profile = settings.url, peer, settings.profile
	}
}

func reportPushResult(out, stderr io.Writer, result proto.PushResult, stats client.TransferStats, err error, apply, jsonOutput bool, watchState, target string) error {
	action := "staged"
	if apply {
		action = "applied"
	}
	if result.Recovered {
		action = "recovered"
	}
	if watchState == "unchanged" {
		action = "unchanged"
	}
	if jsonOutput {
		report := struct {
			proto.PushResult
			transferReport
		}{result, newTransferReport(action, stats, err)}
		if writeErr := json.NewEncoder(out).Encode(report); writeErr != nil {
			return writeErr
		}
	}
	if err != nil {
		return nil
	}
	if !jsonOutput {
		printTransferSummary(stderr, "push", action, target, stats)
		if result.Recovered {
			if watchState == "" {
				fmt.Fprintf(stderr, "errand: completed earlier push %s %s; run push again to send your current changes\n", result.ID, terminalSafeField(target))
			}
		} else if !apply {
			fmt.Fprintf(out, "%s\n", result.ID)
			if watchState == "" {
				fmt.Fprintf(stderr, "errand: changes staged %s; repeat push with the same options and --apply to apply\n", terminalSafeField(target))
			}
		}
	}
	return nil
}
