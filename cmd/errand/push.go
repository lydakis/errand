package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func cmdPush(args []string) int { return cmdPushTo(args, os.Stdout, os.Stderr) }
func cmdPushTo(args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("errand push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var settings runConfigFlags
	fs.StringVar(&settings.on, "on", "", "peer name, or local")
	fs.StringVar(&settings.url, "url", "", "peer base URL")
	fs.StringVar(&settings.profile, "profile", "", "use a named configuration profile")
	fs.StringVar(&settings.root, "workspace-root", "", "snapshot root containing the current directory")
	fs.StringVar(&settings.workspace, "workspace", "", "persistent workspace name (overrides the selected profile)")
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
	effective, err := config.ResolveRun(cwd, overrides)
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
	var stats client.TransferStats
	result, err := client.PushChanges(client.PushOptions{PeerURL: peer, Workspace: workspace, Root: effective.Root, Path: fs.Arg(0), Apply: *apply, MaterializeConflicts: *conflicts, IncludeAll: *includeAll, Stats: &stats})
	action := "staged"
	if *apply {
		action = "applied"
	}
	if result.Recovered {
		action = "recovered"
	}
	if *jsonOutput {
		report := struct {
			proto.PushResult
			transferReport
		}{result, newTransferReport(action, stats, err)}
		if writeErr := json.NewEncoder(out).Encode(report); writeErr != nil {
			fmt.Fprintln(stderr, "errand:", writeErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return client.ExitTransaction
	}
	if !*jsonOutput {
		printTransferSummary(stderr, "push", action, "to workspace "+workspace+" on "+cmpOr(effective.Peer, peer), stats)
		if result.Recovered {
			fmt.Fprintf(stderr, "errand: completed earlier push %s to workspace %s on %s; run push again to send your current changes\n", result.ID, workspace, cmpOr(effective.Peer, peer))
		} else if !*apply {
			fmt.Fprintf(out, "%s\n", result.ID)
			fmt.Fprintf(stderr, "errand: changes staged for workspace %s; repeat push with the same options and --apply to apply\n", workspace)
		}
	}
	return 0
}
