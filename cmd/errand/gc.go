package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

const gcUsage = `usage: errand gc cache|jobs|changes|all [options]

Targets:
  cache    Collect snapshot blobs and named caches using runner expiry and budgets.
  jobs     Remove eligible completed job receipts; requires --older-than DURATION or --keep N.
  changes  Remove local changes, or remote transfer staging with --on PEER; requires --older-than DURATION.
  all      Collect cache, jobs, and transfers on one runner, plus local changes; requires --older-than DURATION.
           This means all categories, not all runners.

Runner selection (cache, jobs, all):
  --on PEER or --url URL selects one runner. Required with multiple configured runners,
  even for previews. A sole configured runner is selected automatically.

Preview any target with --dry-run; the same runner and retention rules apply.
Use errand gc TARGET --help for target-specific options.
`

func resolveGCPeerTarget(rawURL, on string) (string, string, error) {
	if rawURL != "" {
		return resolvePeerTarget(rawURL, on)
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return "", "", err
	}
	if on == "" {
		cfg = cfg.WithLocalPeer()
		switch len(cfg.Peers) {
		case 0:
			return "", "", fmt.Errorf("no runners configured; select one with --url URL or add one with errand peers add")
		case 1:
			for name := range cfg.Peers {
				on = name
			}
		default:
			return "", "", fmt.Errorf("multiple runners configured; select one with --on PEER or --url URL (use errand peers to list runners)")
		}
	}
	peerURL, err := configuredPeerURL(cfg, on)
	return peerURL, on, err
}

func cmdGC(args []string) int {
	return cmdGCTo(args, os.Stdout, os.Stderr)
}

func writeGCCachePolicies(w io.Writer, label string, policies *proto.CacheGCPolicies) {
	for _, category := range []struct {
		name   string
		policy *proto.CacheGCPolicy
	}{
		{"snapshot", policies.Snapshot}, {"named", policies.Named},
	} {
		if category.policy == nil {
			fmt.Fprintf(w, "%s %s cache policy: collection disabled\n", label, category.name)
			continue
		}
		seconds := category.policy.TTLSeconds
		expiry := fmt.Sprintf("%ds", seconds)
		if seconds > 0 && seconds%86400 == 0 {
			expiry = fmt.Sprintf("%dd", seconds/86400)
		}
		fmt.Fprintf(w, "%s %s cache policy: expire after %s unused; budget %s\n", label, category.name, expiry, formatByteSize(category.policy.MaxBytes))
	}
	fmt.Fprintln(w, "Cache collection spans owners; leased named caches are protected.")
}

func cmdGCTo(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, gcUsage)
		return 2
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stderr, gcUsage)
		return 0
	}
	target := args[0]
	if target != "cache" && target != "jobs" && target != "changes" && target != "all" {
		fmt.Fprintf(stderr, "errand: unknown gc target %q; want cache, jobs, changes, or all\n", target)
		return 2
	}

	fs := flag.NewFlagSet("errand gc "+target, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var on, rawURL, olderThan string
	keep := -1
	dryRun := false
	fs.StringVar(&on, "on", "", "peer name")
	fs.StringVar(&rawURL, "url", "", "peer base URL")
	if target == "jobs" || target == "changes" || target == "all" {
		fs.StringVar(&olderThan, "older-than", "", "remove eligible data older than this duration")
	}
	if target == "jobs" || target == "all" {
		fs.IntVar(&keep, "keep", -1, "retain at least the newest N eligible jobs")
	}
	fs.BoolVar(&dryRun, "dry-run", false, "report eligible data without removing it")
	fs.BoolVar(&dryRun, "n", false, "report eligible data without removing it")
	synopsis := "errand gc " + target + " [options]"
	if target == "changes" || target == "all" {
		synopsis = "errand gc " + target + " --older-than DURATION [options]"
	}
	setFlagUsage(fs, synopsis)
	flagUsage := fs.Usage
	fs.Usage = func() {
		flagUsage()
		fmt.Fprintln(stderr)
		switch target {
		case "cache":
			fmt.Fprintln(stderr, "Collects snapshot blobs and named caches across owners using separate runner expiry and budgets. Leased named caches are protected.")
		case "jobs":
			fmt.Fprintln(stderr, "Requires --older-than DURATION or --keep N. With both, only jobs outside both retention bounds are removed. Active and incomplete jobs are protected.")
		case "changes":
			fmt.Fprintln(stderr, "Collects local changes by default, or remote transfer staging with --on PEER. Checkpoints and pending applies are protected.")
		case "all":
			fmt.Fprintln(stderr, "Collects cache, jobs, and transfer staging on one runner, plus local changes.")
		}
		if target != "changes" {
			fmt.Fprintln(stderr, "With multiple configured runners, --on PEER or --url URL is required, including for --dry-run. A sole configured runner is selected automatically.")
		}
	}
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand: unexpected gc arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	var jobRequest proto.JobGCRequest
	var retentionDuration time.Duration
	if target == "jobs" || target == "all" {
		if olderThan == "" && keep == -1 {
			fmt.Fprintln(stderr, "errand: gc jobs requires --older-than or --keep")
			return 2
		}
		if olderThan != "" {
			duration, err := parseRetentionDuration(olderThan)
			if err != nil || duration <= 0 || duration < time.Second {
				fmt.Fprintln(stderr, "errand: --older-than must be a positive duration of at least 1s")
				return 2
			}
			retentionDuration = duration
			seconds := int64(duration / time.Second)
			if duration%time.Second != 0 {
				seconds++
			}
			jobRequest.OlderThanSeconds = &seconds
		}
		if keep != -1 {
			if keep < 0 {
				fmt.Fprintln(stderr, "errand: --keep must not be negative")
				return 2
			}
			jobRequest.Keep = &keep
		}
		jobRequest.DryRun = dryRun
	}
	if target == "changes" {
		if olderThan == "" {
			fmt.Fprintln(stderr, "errand: gc changes requires --older-than")
			return 2
		}
		duration, err := parseRetentionDuration(olderThan)
		if err != nil || duration < time.Second {
			fmt.Fprintln(stderr, "errand: --older-than must be a positive duration of at least 1s")
			return 2
		}
		retentionDuration = duration
	}
	if target == "all" && retentionDuration == 0 {
		fmt.Fprintln(stderr, "errand: gc all requires --older-than so local change state has an explicit retention boundary")
		return 2
	}
	peerURL, label := "", "local"
	if target != "changes" || on != "" || rawURL != "" {
		var err error
		peerURL, label, err = resolveGCPeerTarget(rawURL, on)
		if err != nil {
			fmt.Fprintf(stderr, "errand: %v\n", err)
			return 1
		}
	}
	failed := false
	if target == "cache" || target == "all" {
		result, err := client.CacheGC(peerURL, dryRun)
		if err != nil {
			fmt.Fprintf(stderr, "errand: cache gc: %v\n", err)
			failed = true
		} else if result.DryRun {
			writeGCCachePolicies(stdout, label, result.Policies)
			fmt.Fprintf(stdout, "%s cache: would remove %d blobs and %d named caches and free %d bytes (%d protected; %d interrupted cleanups)\n",
				label, result.RemovedBlobs, result.RemovedCaches, result.FreedBytes, result.ProtectedCaches, result.ReclaimedTemps)
		} else {
			fmt.Fprintf(stdout, "%s cache: removed %d blobs and %d named caches, freed %d bytes (%d protected; %d interrupted cleanups)\n",
				label, result.RemovedBlobs, result.RemovedCaches, result.FreedBytes, result.ProtectedCaches, result.ReclaimedTemps)
		}
	}
	if target == "jobs" || target == "all" {
		result, err := client.JobGC(peerURL, jobRequest)
		if err != nil {
			fmt.Fprintf(stderr, "errand: job gc: %v\n", err)
			failed = true
		} else if result.DryRun {
			fmt.Fprintf(stdout, "%s jobs: would remove %d jobs and free %d bytes (%d protected)\n",
				label, result.SelectedJobs-result.FailedJobs, result.FreedBytes, result.ProtectedJobs)
		} else {
			fmt.Fprintf(stdout, "%s jobs: removed %d jobs, freed %d bytes (%d protected, %d skipped, %d failed, %d cleanup failures)\n",
				label, result.RemovedJobs, result.FreedBytes, result.ProtectedJobs, result.SkippedJobs,
				result.FailedJobs, result.CleanupFailures)
		}
		if err == nil && (result.FailedJobs != 0 || result.CleanupFailures != 0) {
			failed = true
		}
		if !dryRun {
			if err := client.ReconcileCollectedJobChanges(peerURL); err != nil {
				fmt.Fprintf(stderr, "errand: reconciling removed job changes: %v\n", err)
				failed = true
			}
		}
	}
	if target == "all" || target == "changes" && peerURL == "" {
		result, err := client.ChangeGC(retentionDuration, dryRun)
		if err != nil {
			fmt.Fprintf(stderr, "errand: local change gc: %v\n", err)
			failed = true
		}
		if err == nil || result.Removed > 0 || result.Protected > 0 || result.Failed > 0 {
			if result.DryRun {
				fmt.Fprintf(stdout, "local changes: would remove %d records and free %d bytes (%d protected, %d failed)\n",
					result.Removed, result.FreedBytes, result.Protected, result.Failed)
			} else {
				fmt.Fprintf(stdout, "local changes: removed %d records, freed %d bytes (%d protected, %d failed)\n",
					result.Removed, result.FreedBytes, result.Protected, result.Failed)
			}
		}
		if err == nil && result.Failed != 0 {
			failed = true
		}
	}
	if target == "all" || target == "changes" && peerURL != "" {
		result, err := client.RemoteTransferGC(peerURL, retentionDuration, dryRun)
		if err != nil {
			fmt.Fprintln(stderr, "errand: workspace change gc:", err)
			failed = true
		}
		if err == nil || len(result.Failures) > 0 {
			verb := "removed"
			if dryRun {
				verb = "would remove"
			}
			fmt.Fprintf(stdout, "%s workspace changes: %s %d transfers, %d bytes (%d protected, %d failures)\n", label, verb, result.Removed, result.FreedBytes, result.Protected, len(result.Failures))
		}
	}
	if failed {
		return 1
	}
	return 0
}
