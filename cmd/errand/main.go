// errand: run the thing you would have run locally, on another machine
// you own. One binary, two roles: `errand serve` receives, everything
// else delegates.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/workspace"
)

var version = "0.1.0-dev"

const usage = `errand — a personal job runner for machines you own

usage:
  errand [--on PEER | --url URL] [--detach] [--env K=V]... [--passenv K]...
         [--workspace-root PATH] [--workdir REL]
         [--include-all | --no-snapshot] -- CMD [ARG...]
  errand attach [--on PEER | --url URL] HANDLE
  errand ps [--json] [--on PEER | --url URL]
  errand kill [--force] [--on PEER | --url URL] HANDLE
  errand caches [--on PEER | --url URL]
  errand gc cache [--on PEER | --url URL]
  errand gc jobs [--older-than DURATION] [--keep N] [--dry-run] [--on PEER | --url URL]
  errand gc all [--older-than DURATION] [--keep N] [--on PEER | --url URL]
  errand serve [--config PATH] [--listen ADDR] [--state-dir DIR] [--allow-user LOGIN]...
  errand info [--on PEER | --url URL]
  errand version

A HANDLE is peer/ULID as printed at submission (a bare ULID works with
--on/--url or a configured default peer). --detach prints the handle on
stdout and returns after admission; the job keeps running on the peer.
When attached from a terminal, Ctrl-D detaches locally and prints the
reattach command without changing the job. Ctrl-C sends SIGINT to the remote
job and a second Ctrl-C sends SIGKILL. "errand kill" requests SIGTERM;
"errand kill --force" sends SIGKILL. Ctrl-D exits 0 for successful local
detachment, not job completion.

Exit status: the remote process's own exit code. If that code is 0 but the
transaction itself fails, errand exits 120; secondary failures accompanying
a nonzero remote exit are reported without replacing that exit code.

Snapshot safety: Git worktrees are selected automatically. Other directories
require .errandignore or --include-all. Filesystem roots are always refused;
the user's home directory requires --include-all. --no-snapshot runs in a
fresh empty remote workspace without inspecting or transferring local files.
A marked ancestor with [workspace] root=true in .errand.toml, or an explicit
--workspace-root, selects a shared snapshot root containing the current
directory without weakening those safety checks.`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "serve":
		os.Exit(cmdServe(args[1:]))
	case "attach":
		os.Exit(cmdAttach(args[1:]))
	case "ps":
		os.Exit(cmdPs(args[1:]))
	case "kill":
		os.Exit(cmdKill(args[1:]))
	case "caches":
		os.Exit(cmdCaches(args[1:]))
	case "gc":
		os.Exit(cmdGC(args[1:]))
	case "info":
		os.Exit(cmdInfo(args[1:]))
	case "version":
		fmt.Println("errand", version)
		os.Exit(0)
	case "help", "-h", "--help":
		fmt.Println(usage)
		os.Exit(0)
	default:
		os.Exit(cmdRun(args))
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	on := fs.String("on", "", "peer name from ~/.config/errand/config.toml")
	rawURL := fs.String("url", "", "peer base URL (mutually exclusive with --on)")
	workdir := fs.String("workdir", "", "working directory, relative to the workspace root")
	workspaceRoot := fs.String("workspace-root", "", "snapshot root containing the current directory")
	includeAll := fs.Bool("include-all", false, "allow an otherwise refused broad snapshot (never permits a filesystem root)")
	noSnapshot := fs.Bool("no-snapshot", false, "run in an empty remote workspace without inspecting local files")
	detach := fs.Bool("detach", false, "return after admission, printing the job handle on stdout")
	var envs, passenvs stringList
	fs.Var(&envs, "env", "set K=V in the job environment (repeatable)")
	fs.Var(&passenvs, "passenv", "forward the named local env var (repeatable)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }

	// Everything after "--" is the command; flags come before it.
	split := -1
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	if split < 0 {
		fmt.Fprintln(os.Stderr, "errand: missing \"--\" before the command\n\n"+usage)
		return 2
	}
	if err := fs.Parse(args[:split]); err != nil {
		return 2
	}
	argv := args[split+1:]
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "errand: empty command after \"--\"")
		return 2
	}
	if *includeAll && *noSnapshot {
		fmt.Fprintln(os.Stderr, "errand: --include-all and --no-snapshot are mutually exclusive")
		return 2
	}
	if *noSnapshot && *workspaceRoot != "" {
		fmt.Fprintln(os.Stderr, "errand: --workspace-root and --no-snapshot are mutually exclusive")
		return 2
	}
	if *noSnapshot && *workdir != "" && *workdir != "." {
		fmt.Fprintln(os.Stderr, "errand: --workdir must be the workspace root when using --no-snapshot")
		return 2
	}

	peerURL, peerLabel, err := resolvePeerTarget(*rawURL, *on)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return client.ExitTransaction
	}
	env := map[string]string{}
	for _, kv := range envs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			fmt.Fprintf(os.Stderr, "errand: --env wants K=V, got %q\n", kv)
			return 2
		}
		env[k] = v
	}
	root := ""
	if !*noSnapshot {
		cwd, cwdErr := os.Getwd()
		if cwdErr != nil {
			fmt.Fprintf(os.Stderr, "errand: %v\n", cwdErr)
			return client.ExitTransaction
		}
		selected, discoverErr := workspace.Discover(cwd, *workspaceRoot)
		if discoverErr != nil {
			fmt.Fprintf(os.Stderr, "errand: %v\n", discoverErr)
			return client.ExitTransaction
		}
		root = selected.Root
		if *workdir == "" {
			*workdir = selected.Workdir
		}
		shownWorkdir := *workdir
		if shownWorkdir == "" {
			shownWorkdir = "."
		}
		fmt.Fprintf(os.Stderr, "errand: workspace root %s (from %s)\n", selected.Root, selected.Source)
		fmt.Fprintf(os.Stderr, "errand: command workdir %s\n", shownWorkdir)
	}
	return client.Run(client.RunOptions{
		PeerURL:    peerURL,
		PeerName:   peerLabel,
		Root:       root,
		Argv:       argv,
		Env:        env,
		PassEnv:    passenvs,
		Workdir:    *workdir,
		IncludeAll: *includeAll,
		NoSnapshot: *noSnapshot,
		Detach:     *detach,
	})
}

// resolvePeerTarget returns the effective transport URL and the label that
// must be embedded in a handle. Routing and labeling are resolved together so
// a handle can never name a different peer from the one actually contacted.
func resolvePeerTarget(rawURL, on string) (peerURL, label string, err error) {
	if rawURL != "" && on != "" {
		return "", "", fmt.Errorf("--on and --url are mutually exclusive")
	}
	if rawURL != "" {
		peerURL = strings.TrimSuffix(rawURL, "/")
		if peerURL == "" {
			return "", "", fmt.Errorf("--url must not be empty")
		}
		return peerURL, peerURL, nil
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return "", "", err
	}
	label = on
	if label == "" {
		label = cfg.DefaultPeer
	}
	peerURL, err = cfg.PeerURL(on)
	return peerURL, label, err
}

// resolveHandle turns "peer/ULID" or a bare ULID into (peerURL, label, jobID).
// The peer part must be a configured alias or an explicit HTTP(S) URL. A
// caller-supplied --url may route an alias-qualified handle on a machine that
// does not share that alias, but the resulting label is the effective URL.
func resolveHandle(handleArg, rawURL, on string) (peerURL, label, jobID string, err error) {
	prefix := ""
	jobID = handleArg
	if i := strings.LastIndexByte(handleArg, '/'); i >= 0 {
		prefix, jobID = handleArg[:i], handleArg[i+1:]
	}
	if !proto.ValidULID(jobID) {
		return "", "", "", fmt.Errorf("handle %q does not contain a valid job ULID", handleArg)
	}
	if rawURL != "" && on != "" {
		return "", "", "", fmt.Errorf("--on and --url are mutually exclusive")
	}
	switch {
	case rawURL != "":
		effectiveURL := strings.TrimSuffix(rawURL, "/")
		if strings.HasPrefix(prefix, "http://") || strings.HasPrefix(prefix, "https://") {
			handleURL := strings.TrimSuffix(prefix, "/")
			if handleURL != effectiveURL {
				return "", "", "", fmt.Errorf("handle peer %q conflicts with --url %q", handleURL, effectiveURL)
			}
		}
		peerURL, label, err = resolvePeerTarget(rawURL, "")
	case on != "":
		if prefix != "" && prefix != on {
			return "", "", "", fmt.Errorf("handle peer %q conflicts with --on %q", prefix, on)
		}
		peerURL, label, err = resolvePeerTarget("", on)
		if err != nil {
			return "", "", "", err
		}
	case prefix == "":
		peerURL, label, err = resolvePeerTarget("", "")
	case strings.HasPrefix(prefix, "http://") || strings.HasPrefix(prefix, "https://"):
		peerURL = strings.TrimSuffix(prefix, "/")
		label = peerURL
	default:
		cfg, cfgErr := config.LoadClient()
		if cfgErr != nil {
			return "", "", "", cfgErr
		}
		peerURL, err = cfg.PeerURL(prefix)
		if err != nil {
			return "", "", "", err
		}
		label = prefix
	}
	if err != nil {
		return "", "", "", err
	}
	return peerURL, label, jobID, nil
}

func cmdAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "errand attach: exactly one HANDLE (peer/ULID) is required")
		return 2
	}
	peerURL, label, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 2
	}
	return client.Attach(client.AttachOptions{
		PeerURL: peerURL, PeerName: label, JobID: jobID,
	})
}

func cmdKill(args []string) int {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	force := fs.Bool("force", false, "SIGKILL instead of SIGTERM")
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "errand kill: exactly one HANDLE (peer/ULID) is required")
		return 2
	}
	peerURL, label, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 2
	}
	if err := client.Kill(peerURL, jobID, *force); err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 1
	}
	label = cmpOr(label, peerURL)
	if *force {
		fmt.Fprintf(os.Stderr, "errand: force kill (SIGKILL) requested for %s/%s\n", label, jobID)
	} else {
		fmt.Fprintf(os.Stderr, "errand: graceful termination (SIGTERM) requested for %s/%s\n", label, jobID)
	}
	return 0
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type psRow struct {
	Peer string `json:"peer"`
	proto.JobListEntry
}

func cmdPs(args []string) int {
	return cmdPsTo(args, os.Stdout, os.Stderr)
}

func cmdPsTo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	on := fs.String("on", "", "restrict to one peer name")
	rawURL := fs.String("url", "", "restrict to one peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Parse(args)
	if *rawURL != "" && *on != "" {
		fmt.Fprintln(stderr, "errand: --on and --url are mutually exclusive")
		return 2
	}

	type target struct{ name, url string }
	var targets []target
	failed := false
	switch {
	case *rawURL != "":
		url := strings.TrimSuffix(*rawURL, "/")
		targets = append(targets, target{name: url, url: url})
	default:
		cfg, err := config.LoadClient()
		if err != nil {
			fmt.Fprintf(stderr, "errand: %v\n", err)
			return 1
		}
		if *on != "" {
			url, err := cfg.PeerURL(*on)
			if err != nil {
				fmt.Fprintf(stderr, "errand: %v\n", err)
				return 1
			}
			targets = append(targets, target{name: *on, url: url})
		} else {
			names := make([]string, 0, len(cfg.Peers))
			for name := range cfg.Peers {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				url, err := cfg.PeerURL(name)
				if err != nil {
					fmt.Fprintf(stderr, "errand: peer %s: %v\n", name, err)
					failed = true
					continue
				}
				targets = append(targets, target{name: name, url: url})
			}
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(stderr, "errand: no usable peers configured; check ~/.config/errand/config.toml")
		return 1
	}

	rows := make([]psRow, 0)
	reached := false
	for _, tgt := range targets {
		entries, err := client.List(tgt.url)
		if err != nil {
			fmt.Fprintf(stderr, "errand: peer %s: %v\n", tgt.name, err)
			failed = true
			continue
		}
		reached = true
		for _, e := range entries {
			rows = append(rows, psRow{Peer: tgt.name, JobListEntry: e})
		}
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "errand: encoding job listing: %v\n", err)
			return 1
		}
	} else {
		writePsTable(stdout, rows)
	}
	if !reached || failed {
		return 1
	}
	return 0
}

func writePsTable(w io.Writer, rows []psRow) {
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tJOB\tSTATE\tEXIT\tADMITTED\tSTARTED\tDURATION\tSOURCE\tWORKDIR\tCOMMAND")
	for _, row := range rows {
		exit := "-"
		switch {
		case row.ExitCode != nil:
			exit = fmt.Sprintf("%d", *row.ExitCode)
		case row.Signal != "":
			exit = row.Signal
		}
		admitted := formatLocalTime(row.AdmittedAt)
		started := "-"
		if row.StartedAt != nil {
			started = formatLocalTime(*row.StartedAt)
		}
		duration := "-"
		if row.StartedAt != nil {
			duration = shortDuration(time.Duration(row.DurationMS) * time.Millisecond)
		}
		source := terminalSafeField(jobSource(row.JobListEntry))
		workdir := row.Workdir
		if workdir == "" {
			workdir = "."
		}
		workdir = terminalSafeField(workdir)
		commandRunes := []rune(row.Command)
		cmd := row.Command
		if len(commandRunes) > 60 {
			cmd = string(commandRunes[:59]) + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.Peer, row.ID, row.State, exit, admitted, started, duration, source, workdir, cmd)
	}
	_ = tw.Flush()
}

func formatLocalTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

func terminalSafeField(value string) string {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return strconv.QuoteToGraphic(value)
	}
	return value
}

func jobSource(entry proto.JobListEntry) string {
	if entry.GitCommit != "" {
		source := truncateHash(entry.GitCommit)
		if entry.GitDirty {
			source += "+dirty"
		}
		return source
	}
	if entry.ManifestRoot != "" {
		return "snapshot:" + truncateHash(entry.ManifestRoot)
	}
	return "-"
}

func truncateHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	}
}

func cmdCaches(args []string) int {
	fs := flag.NewFlagSet("caches", flag.ExitOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	fs.Parse(args)
	peerURL, label, err := resolvePeerTarget(*rawURL, *on)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 1
	}
	stats, err := client.CacheStats(peerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 1
	}
	fmt.Printf("%s: %d blobs, %d bytes used of %d (ttl %dh)\n",
		label, stats.Blobs, stats.Bytes, stats.MaxBytes, stats.TTLHours)
	return 0
}

func cmdGC(args []string) int {
	return cmdGCTo(args, os.Stdout, os.Stderr)
}

func cmdGCTo(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: errand gc cache|jobs|all [options]")
		return 2
	}
	target := args[0]
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	olderThan := fs.String("older-than", "", "remove jobs settled longer than this duration ago")
	keep := fs.Int("keep", -1, "retain at least the newest N eligible jobs")
	dryRun := fs.Bool("dry-run", false, "report eligible jobs without removing them")
	fs.Parse(args[1:])
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand: unexpected gc arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if target != "cache" && target != "jobs" && target != "all" {
		fmt.Fprintf(stderr, "errand: unknown gc target %q; want cache, jobs, or all\n", target)
		return 2
	}
	if target == "cache" && (*olderThan != "" || *keep != -1 || *dryRun) {
		fmt.Fprintln(stderr, "errand: job retention flags do not apply to gc cache")
		return 2
	}
	if target == "all" && *dryRun {
		fmt.Fprintln(stderr, "errand: --dry-run applies only to gc jobs")
		return 2
	}
	var jobRequest proto.JobGCRequest
	if target == "jobs" || target == "all" {
		if *olderThan == "" && *keep == -1 {
			fmt.Fprintln(stderr, "errand: gc jobs requires --older-than or --keep")
			return 2
		}
		if *olderThan != "" {
			duration, err := parseRetentionDuration(*olderThan)
			if err != nil || duration <= 0 || duration < time.Second {
				fmt.Fprintln(stderr, "errand: --older-than must be a positive duration of at least 1s")
				return 2
			}
			seconds := int64(duration / time.Second)
			if duration%time.Second != 0 {
				seconds++
			}
			jobRequest.OlderThanSeconds = &seconds
		}
		if *keep != -1 {
			if *keep < 0 {
				fmt.Fprintln(stderr, "errand: --keep must not be negative")
				return 2
			}
			jobRequest.Keep = keep
		}
		jobRequest.DryRun = *dryRun
	}
	peerURL, label, err := resolvePeerTarget(*rawURL, *on)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 1
	}
	failed := false
	if target == "cache" || target == "all" {
		result, err := client.CacheGC(peerURL)
		if err != nil {
			fmt.Fprintf(stderr, "errand: cache gc: %v\n", err)
			failed = true
		} else {
			fmt.Fprintf(stdout, "%s cache: removed %d blobs, freed %d bytes\n",
				label, result.RemovedBlobs, result.FreedBytes)
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
	}
	if failed {
		return 1
	}
	return 0
}

func parseRetentionDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		if err != nil || days <= 0 || days > int64((1<<63-1)/int64(24*time.Hour)) {
			return 0, fmt.Errorf("invalid day duration %q", value)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(value)
}

func cmdInfo(args []string) int {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	fs.Parse(args)
	peerURL, _, err := resolvePeerTarget(*rawURL, *on)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 1
	}
	info, err := client.Info(peerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 1
	}
	out, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(out))
	return 0
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to errandd.toml")
	listen := fs.String("listen", "", `listen address ("tailnet:7443" resolves the tailnet IP)`)
	stateDir := fs.String("state-dir", "", "receipt and job state directory")
	insecure := fs.Bool("insecure-no-auth", false, "DANGEROUS: skip all authorization (tests only)")
	var allowUsers stringList
	fs.Var(&allowUsers, "allow-user", "tailnet login allowed to use this runner (repeatable)")
	fs.Parse(args)

	fileCfg, err := config.LoadDaemon(*cfgPath)
	if err != nil {
		log.Fatalf("errand serve: %v", err)
	}
	if *listen != "" {
		fileCfg.Listen = *listen
	}
	if *stateDir != "" {
		fileCfg.StateDir = *stateDir
	}
	fileCfg.AllowUsers = append(fileCfg.AllowUsers, allowUsers...)

	addr, err := config.ResolveListen(fileCfg.Listen, fileCfg.TailscaledSocket)
	if err != nil {
		log.Fatalf("errand serve: %v", err)
	}
	d, err := daemon.New(daemon.Config{
		Listen:           addr,
		StateDir:         fileCfg.StateDir,
		AllowUsers:       fileCfg.AllowUsers,
		Capability:       fileCfg.Capability,
		TailscaledSocket: fileCfg.TailscaledSocket,
		InsecureNoAuth:   *insecure,
		Version:          version,
		CacheDisabled:    fileCfg.Cache.Disabled,
		CacheMaxBytes:    fileCfg.Cache.MaxBytes,
		CacheTTL:         time.Duration(fileCfg.Cache.TTLHours) * time.Hour,
		MaxJobs:          fileCfg.MaxJobs,
		MaxQueued:        fileCfg.MaxQueued,
	})
	if err != nil {
		log.Fatalf("errand serve: %v", err)
	}
	defer d.Close()
	mode := "tailnet whois"
	if *insecure {
		mode = "INSECURE no-auth"
	}
	log.Printf("errand %s serving on %s (%s; state %s)", version, addr, mode, fileCfg.StateDir)
	server := &http.Server{
		Addr: addr, Handler: d.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("errand serve: %v", err)
	}
	return 0
}
