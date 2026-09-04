// errand: run the thing you would have run locally, on another machine
// you own. One binary, two roles: `errand serve` receives, everything
// else delegates.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
	"github.com/lydakis/errand/internal/workspace"
)

var version = "0.1.0-dev"

const usage = `errand — a personal job runner for machines you own

usage:
  errand [--on PEER | --url URL] [-d | --detach]
         [-L [LOCAL:]REMOTE | --forward [LOCAL:]REMOTE]...
         [--apply | --no-apply]
         [-e K=V | --env K=V]... [--passenv K]...
         [--workspace-root PATH] [-w REL | --workdir REL]
         [--include-all | --no-snapshot] -- CMD [ARG...]
  errand attach [-L [LOCAL:]REMOTE | --forward [LOCAL:]REMOTE]...
                [--on PEER | --url URL] HANDLE
  errand fetch [--apply [--conflicts]] [--on PEER | --url URL] HANDLE [PATH]
  errand ps [-a | --all] [-n N | --last N] [--json] [--on PEER | --url URL]
  errand status [--json] [--on PEER | --url URL] HANDLE
  errand kill [-f | --force] [--on PEER | --url URL] HANDLE
  errand df [--json] [--on PEER | --url URL]
  errand gc cache [-n | --dry-run] [--on PEER | --url URL]
  errand gc jobs [--older-than DURATION] [--keep N] [-n | --dry-run] [--on PEER | --url URL]
  errand gc changes --older-than DURATION [-n | --dry-run]
  errand gc all --older-than DURATION [--keep N] [-n | --dry-run] [--on PEER | --url URL]
  errand serve [--config PATH] [--listen ADDR] [--state-dir DIR] [--allow-user LOGIN]...
  errand setup [--max-jobs N] [--allow-user LOGIN]...
               [-f | --force] [-n | --dry-run] [--print-acl]
  errand info [--json] [--on PEER | --url URL]
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
	skipResume := cliHelpRequested(args)
	switch args[0] {
	case "serve", "setup", "_automatic-apply", "_stdio", "version":
		skipResume = true
	}
	if !skipResume {
		if err := client.ResumeAutomaticApplies(); err != nil {
			fmt.Fprintf(os.Stderr, "errand: resuming automatic workspace applications: %v\n", err)
		}
	}
	switch args[0] {
	case "serve":
		os.Exit(cmdServe(args[1:]))
	case "setup":
		os.Exit(cmdSetup(args[1:]))
	case "attach":
		os.Exit(cmdAttach(args[1:]))
	case "fetch":
		os.Exit(cmdFetch(args[1:]))
	case "ps":
		os.Exit(cmdPs(args[1:]))
	case "status":
		os.Exit(cmdStatus(args[1:]))
	case "kill":
		os.Exit(cmdKill(args[1:]))
	case "df":
		os.Exit(cmdDf(args[1:]))
	case "gc":
		os.Exit(cmdGC(args[1:]))
	case "info":
		os.Exit(cmdInfo(args[1:]))
	case "version":
		os.Exit(cmdVersion(args[1:]))
	case "_automatic-apply":
		os.Exit(cmdAutomaticApply(args[1:]))
	case "_stdio":
		os.Exit(cmdStdio(args[1:]))
	case "-h", "--help":
		fmt.Println(usage)
		os.Exit(0)
	default:
		os.Exit(cmdRun(args))
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type portForwardList []client.PortForward

func (p *portForwardList) String() string {
	values := make([]string, 0, len(*p))
	for _, forward := range *p {
		if forward.Local == forward.Remote {
			values = append(values, strconv.Itoa(int(forward.Remote)))
		} else {
			values = append(values, fmt.Sprintf("%d:%d", forward.Local, forward.Remote))
		}
	}
	return strings.Join(values, ",")
}

func (p *portForwardList) Set(value string) error {
	localText, remoteText, hasLocal := strings.Cut(value, ":")
	if !hasLocal {
		remoteText = localText
	}
	if localText == "" || remoteText == "" || strings.Contains(remoteText, ":") {
		return fmt.Errorf("--forward wants [LOCAL:]REMOTE, got %q", value)
	}
	parse := func(label, text string) (uint16, error) {
		port, err := strconv.ParseUint(text, 10, 16)
		if err != nil || port == 0 {
			return 0, fmt.Errorf("%s port %q must be between 1 and 65535", label, text)
		}
		return uint16(port), nil
	}
	local, err := parse("local", localText)
	if err != nil {
		return err
	}
	remote, err := parse("remote", remoteText)
	if err != nil {
		return err
	}
	for _, existing := range *p {
		if existing.Local == local {
			return fmt.Errorf("local port %d is forwarded more than once", local)
		}
	}
	*p = append(*p, client.PortForward{Local: local, Remote: remote})
	return nil
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("errand", flag.ExitOnError)
	on := fs.String("on", "", "peer name from ~/.config/errand/config.toml")
	rawURL := fs.String("url", "", "peer base URL (mutually exclusive with --on)")
	workdir := fs.String("workdir", "", "working directory, relative to the workspace root")
	workspaceRoot := fs.String("workspace-root", "", "snapshot root containing the current directory")
	includeAll := fs.Bool("include-all", false, "allow an otherwise refused broad snapshot (never permits a filesystem root)")
	noSnapshot := fs.Bool("no-snapshot", false, "run in an empty remote workspace without inspecting local file contents")
	detach := fs.Bool("detach", false, "return after admission, printing the job handle on stdout")
	fs.BoolVar(detach, "d", false, "return after admission, printing the job handle on stdout")
	apply := fs.Bool("apply", false, "apply retained workspace changes after successful completion")
	noApply := fs.Bool("no-apply", false, "do not apply retained workspace changes after the run")
	fs.StringVar(workdir, "w", "", "working directory, relative to the workspace root")
	var forwards portForwardList
	var envs, passenvs stringList
	fs.Var(&forwards, "forward", "forward local loopback [LOCAL:]REMOTE while attached (repeatable)")
	fs.Var(&forwards, "L", "forward local loopback [LOCAL:]REMOTE while attached (repeatable)")
	fs.Var(&envs, "env", "set K=V in the job environment (repeatable)")
	fs.Var(&envs, "e", "set K=V in the job environment (repeatable)")
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
	if *apply && *noApply {
		fmt.Fprintln(os.Stderr, "errand: --apply and --no-apply are mutually exclusive")
		return 2
	}
	if *detach && len(forwards) != 0 {
		fmt.Fprintln(os.Stderr, "errand: --detach and --forward are mutually exclusive")
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
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return client.ExitTransaction
	}
	root := cwd
	project := ""
	var workspaceApplyOnSuccess *bool
	if !*noSnapshot {
		selected, discoverErr := workspace.Discover(cwd, *workspaceRoot)
		if discoverErr != nil {
			fmt.Fprintf(os.Stderr, "errand: %v\n", discoverErr)
			return client.ExitTransaction
		}
		root = selected.Root
		project = selected.Project
		workspaceApplyOnSuccess = selected.ApplyOnSuccess
		if *workdir == "" {
			*workdir = selected.Workdir
		}
		shownWorkdir := *workdir
		if shownWorkdir == "" {
			shownWorkdir = "."
		}
		fmt.Fprintf(os.Stderr, "errand: workspace root %s (from %s)\n", selected.Root, selected.Source)
		fmt.Fprintf(os.Stderr, "errand: command workdir %s\n", shownWorkdir)
	} else {
		selected, discoverErr := workspace.Discover(cwd, cwd)
		if discoverErr != nil {
			fmt.Fprintf(os.Stderr, "errand: %v\n", discoverErr)
			return client.ExitTransaction
		}
		workspaceApplyOnSuccess = selected.ApplyOnSuccess
	}
	globalApplyOnSuccess := false
	if !*apply && !*noApply && workspaceApplyOnSuccess == nil {
		clientConfig, err := config.LoadClient()
		if err != nil {
			fmt.Fprintf(os.Stderr, "errand: %v\n", err)
			return client.ExitTransaction
		}
		globalApplyOnSuccess = clientConfig.ApplyOnSuccess
	}
	applyOnSuccess, err := resolveApplyOnSuccess(
		*apply, *noApply, workspaceApplyOnSuccess, globalApplyOnSuccess,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 2
	}
	return client.Run(client.RunOptions{
		PeerURL:        peerURL,
		PeerName:       peerLabel,
		Root:           root,
		Argv:           argv,
		Env:            env,
		PassEnv:        passenvs,
		Workdir:        *workdir,
		Project:        project,
		IncludeAll:     *includeAll,
		NoSnapshot:     *noSnapshot,
		Detach:         *detach,
		ApplyOnSuccess: applyOnSuccess,
		Forwards:       forwards,
	})
}

func resolveApplyOnSuccess(
	explicitApply, explicitNoApply bool,
	workspaceApply *bool,
	globalApply bool,
) (bool, error) {
	if explicitApply && explicitNoApply {
		return false, fmt.Errorf("--apply and --no-apply are mutually exclusive")
	}
	if explicitApply {
		return true, nil
	}
	if explicitNoApply {
		return false, nil
	}
	if workspaceApply != nil {
		return *workspaceApply, nil
	}
	return globalApply, nil
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
	peerURL, err = configuredPeerURL(cfg, on)
	return peerURL, label, err
}

func configuredPeerURL(cfg config.Client, name string) (string, error) {
	peerURL, err := cfg.PeerURL(name)
	if err != nil {
		return "", err
	}
	identity := name
	if identity == "" {
		identity = cfg.DefaultPeer
	}
	peerURL = client.ConfigureSSHPeer(
		peerURL, identity, cfg.SSHRemoteCommand(name), cfg.SSHRemoteSocket(name),
	)
	return peerURL, nil
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
		if strings.HasPrefix(prefix, "http://") || strings.HasPrefix(prefix, "https://") || strings.HasPrefix(prefix, "ssh://") {
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
	case strings.HasPrefix(prefix, "http://") || strings.HasPrefix(prefix, "https://") || strings.HasPrefix(prefix, "ssh://"):
		peerURL = strings.TrimSuffix(prefix, "/")
		label = peerURL
	default:
		cfg, cfgErr := config.LoadClient()
		if cfgErr != nil {
			return "", "", "", cfgErr
		}
		peerURL, err = configuredPeerURL(cfg, prefix)
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
	fs := flag.NewFlagSet("errand attach", flag.ExitOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	var forwards portForwardList
	fs.Var(&forwards, "forward", "forward local loopback [LOCAL:]REMOTE while attached (repeatable)")
	fs.Var(&forwards, "L", "forward local loopback [LOCAL:]REMOTE while attached (repeatable)")
	setFlagUsage(fs, "errand attach [options] HANDLE")
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
		PeerURL: peerURL, PeerName: label, JobID: jobID, Forwards: forwards,
	})
}

func cmdFetch(args []string) int {
	fs := flag.NewFlagSet("errand fetch", flag.ExitOnError)
	apply := fs.Bool("apply", false, "apply retained workspace changes with a clean-or-refuse three-way merge")
	conflicts := fs.Bool("conflicts", false, "materialize text conflicts and apply clean changes")
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	setFlagUsage(fs, "errand fetch [options] HANDLE [PATH]")
	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(os.Stderr, "errand fetch: HANDLE (peer/ULID) and at most one changed PATH are required")
		return 2
	}
	if *conflicts && !*apply {
		fmt.Fprintln(os.Stderr, "errand fetch: --conflicts requires --apply")
		return 2
	}
	peerURL, _, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 2
	}
	changePath := ""
	if fs.NArg() == 2 {
		changePath = fs.Arg(1)
	}
	callerDir := ""
	if *apply {
		callerDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "errand: resolving current workspace: %v\n", err)
			return client.ExitTransaction
		}
	}
	staged, err := client.FetchChanges(client.ChangeFetchOptions{
		PeerURL: peerURL, JobID: jobID, Apply: *apply, MaterializeConflicts: *conflicts,
		Path: changePath, CallerDir: callerDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		if staged != "" {
			fmt.Fprintf(os.Stderr, "errand: workspace changes remain staged at %s\n", staged)
		}
		return client.ExitTransaction
	}
	if *apply {
		fmt.Fprintf(os.Stderr, "errand: workspace changes applied from %s\n", staged)
	} else {
		fmt.Fprintln(os.Stdout, staged)
	}
	return 0
}

func cmdKill(args []string) int {
	fs := flag.NewFlagSet("errand kill", flag.ExitOnError)
	force := fs.Bool("force", false, "SIGKILL instead of SIGTERM")
	fs.BoolVar(force, "f", false, "SIGKILL instead of SIGTERM")
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	setFlagUsage(fs, "errand kill [options] HANDLE")
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

type peerTarget struct{ name, url string }

type peerQueryResult[T any] struct {
	target peerTarget
	value  T
	err    error
}

type fleetRead[T any] struct {
	targets []peerTarget
	results []peerQueryResult[T]
	failed  bool
}

func queryPeerTargets[T any](targets []peerTarget, query func(string) (T, error)) []peerQueryResult[T] {
	results := make([]peerQueryResult[T], len(targets))
	var wg sync.WaitGroup
	wg.Add(len(targets))
	for i, target := range targets {
		go func() {
			defer wg.Done()
			value, err := query(target.url)
			results[i] = peerQueryResult[T]{target: target, value: value, err: err}
		}()
	}
	wg.Wait()
	return results
}

// readFleet standardizes the CLI contract for read-only discovery commands:
// query every configured peer unless explicitly narrowed, preserve partial
// results, report peer-specific failures, and fail the command if any selected
// peer could not be read.
func readFleet[T any](rawURL, on string, stderr io.Writer, query func(string) (T, error)) (fleetRead[T], error) {
	targets, warnings, err := peerTargets(rawURL, on)
	if err != nil {
		return fleetRead[T]{}, err
	}
	read := fleetRead[T]{targets: targets, failed: len(warnings) != 0}
	for _, warning := range warnings {
		fmt.Fprintf(stderr, "errand: %v\n", warning)
	}
	if len(targets) == 0 {
		return read, fmt.Errorf("no usable peers configured; check ~/.config/errand/config.toml")
	}
	for _, result := range queryPeerTargets(targets, query) {
		if result.err != nil {
			fmt.Fprintf(stderr, "errand: peer %s: %v\n", result.target.name, result.err)
			read.failed = true
			continue
		}
		read.results = append(read.results, result)
	}
	return read, nil
}

func (r fleetRead[T]) exitCode() int {
	if r.failed || len(r.results) == 0 {
		return 1
	}
	return 0
}

// peerTargets fans discovery commands out to every configured peer unless the
// caller explicitly narrows the request with --on or --url.
func peerTargets(rawURL, on string) ([]peerTarget, []error, error) {
	if rawURL != "" && on != "" {
		return nil, nil, fmt.Errorf("--on and --url are mutually exclusive")
	}
	if rawURL != "" {
		url := strings.TrimSuffix(rawURL, "/")
		return []peerTarget{{name: url, url: url}}, nil, nil
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, nil, err
	}
	if on != "" {
		url, err := configuredPeerURL(cfg, on)
		if err != nil {
			return nil, nil, err
		}
		return []peerTarget{{name: on, url: url}}, nil, nil
	}

	names := make([]string, 0, len(cfg.Peers))
	for name := range cfg.Peers {
		names = append(names, name)
	}
	sort.Strings(names)
	targets := make([]peerTarget, 0, len(names))
	var warnings []error
	for _, name := range names {
		url, err := configuredPeerURL(cfg, name)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("peer %s: %w", name, err))
			continue
		}
		targets = append(targets, peerTarget{name: name, url: url})
	}
	return targets, warnings, nil
}

func cmdPs(args []string) int {
	return cmdPsTo(args, os.Stdout, os.Stderr)
}

func cmdPsTo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("errand ps", flag.ExitOnError)
	fs.SetOutput(stderr)
	on := fs.String("on", "", "restrict to one peer name")
	rawURL := fs.String("url", "", "restrict to one peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	all := false
	last := 0
	fs.BoolVar(&all, "all", false, "include terminal jobs")
	fs.BoolVar(&all, "a", false, "include terminal jobs")
	fs.IntVar(&last, "last", 0, "show only the latest N jobs across all states")
	fs.IntVar(&last, "n", 0, "show only the latest N jobs across all states")
	setFlagUsage(fs, "errand ps [options]")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand: unexpected ps arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if last < 0 {
		fmt.Fprintln(stderr, "errand: --last must be positive")
		return 2
	}
	if last > proto.MaxJobListEntries {
		fmt.Fprintf(stderr, "errand: --last must not exceed %d\n", proto.MaxJobListEntries)
		return 2
	}
	list := client.List
	if !all && last == 0 {
		list = client.ListActive
	}
	read, err := readFleet(*rawURL, *on, stderr, list)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 1
	}

	rows := make([]psRow, 0)
	for _, result := range read.results {
		for _, e := range result.value {
			rows = append(rows, psRow{Peer: result.target.name, JobListEntry: e})
		}
	}
	sort.SliceStable(rows, func(i, k int) bool {
		return rows[i].ID > rows[k].ID
	})
	if !all && last == 0 {
		active := rows[:0]
		for _, row := range rows {
			if activeJobState(row.State) {
				active = append(active, row)
			}
		}
		rows = active
	}
	if last > 0 && len(rows) > last {
		rows = rows[:last]
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "errand: encoding job listing: %v\n", err)
			return 1
		}
	} else if len(rows) != 0 {
		writePs(stdout, rows)
	} else if len(read.results) != 0 {
		fmt.Fprintln(stdout, psEmptyMessage(read.targets, !all && last == 0, read.failed))
	}
	return read.exitCode()
}

func psEmptyMessage(targets []peerTarget, activeOnly, partial bool) string {
	kind := "jobs"
	if activeOnly {
		kind = "active jobs"
	}
	if partial {
		return fmt.Sprintf("No %s found on reachable peers.", kind)
	}
	if len(targets) == 1 {
		return fmt.Sprintf("No %s on %s.", kind, terminalSafeField(targets[0].name))
	}
	return fmt.Sprintf("No %s.", kind)
}

func activeJobState(state string) bool {
	return state == proto.StateStaging || state == proto.StateQueued || state == proto.StateRunning
}

func cmdGC(args []string) int {
	return cmdGCTo(args, os.Stdout, os.Stderr)
}

func cmdGCTo(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: errand gc cache|jobs|changes|all [options]")
		return 2
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, "usage: errand gc cache|jobs|changes|all [options]")
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
	if target != "changes" {
		fs.StringVar(&on, "on", "", "peer name")
		fs.StringVar(&rawURL, "url", "", "peer base URL")
	}
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
	if target != "changes" {
		var err error
		peerURL, label, err = resolvePeerTarget(rawURL, on)
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
			fmt.Fprintf(stdout, "%s cache: would remove %d blobs and free %d bytes\n",
				label, result.RemovedBlobs, result.FreedBytes)
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
		if !dryRun {
			if err := client.ReconcileCollectedJobChanges(peerURL); err != nil {
				fmt.Fprintf(stderr, "errand: reconciling removed job changes: %v\n", err)
				failed = true
			}
		}
	}
	if target == "changes" || target == "all" {
		result, err := client.ChangeGC(retentionDuration, dryRun)
		if err != nil {
			fmt.Fprintf(stderr, "errand: local change gc: %v\n", err)
			failed = true
		} else if result.DryRun {
			fmt.Fprintf(stdout, "local changes: would remove %d records and free %d bytes (%d protected, %d failed)\n",
				result.Removed, result.FreedBytes, result.Protected, result.Failed)
		} else {
			fmt.Fprintf(stdout, "local changes: removed %d records, freed %d bytes (%d protected, %d failed)\n",
				result.Removed, result.FreedBytes, result.Protected, result.Failed)
		}
		if err == nil && result.Failed != 0 {
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
	return cmdInfoTo(args, os.Stdout, os.Stderr)
}

func cmdInfoTo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("errand info", flag.ExitOnError)
	fs.SetOutput(stderr)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	setFlagUsage(fs, "errand info [options]")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand: unexpected info arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	read, err := readFleet(*rawURL, *on, stderr, client.Info)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 1
	}
	if *jsonOutput {
		infos := make(map[string]proto.Info, len(read.results))
		for _, result := range read.results {
			infos[result.target.name] = result.value
		}

		var output any = infos
		if (*rawURL != "" || *on != "") && len(infos) == 1 {
			for _, info := range infos {
				output = info
			}
		}
		encoded, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "errand: encoding runner info: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
	} else if len(read.results) != 0 {
		writeInfo(stdout, read.results)
	}
	return read.exitCode()
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("errand serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to errandd.toml")
	listen := fs.String("listen", "", `listen address ("tailnet:7443" resolves the tailnet IP; "none" is SSH-only)`)
	stateDir := fs.String("state-dir", "", "receipt and job state directory")
	insecure := fs.Bool("insecure-no-auth", false, "DANGEROUS: skip all authorization (tests only)")
	var allowUsers stringList
	fs.Var(&allowUsers, "allow-user", "tailnet login allowed to use this runner (repeatable)")
	setFlagUsage(fs, "errand serve [options]")
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

	tcpEnabled := !strings.EqualFold(strings.TrimSpace(fileCfg.Listen), config.DisabledListener)
	var identity tailnet.Provider
	var addr string
	if tcpEnabled {
		addr, identity, err = resolveServeTransport(
			fileCfg.Listen, *insecure, fileCfg.TailscaledSocket, fileCfg.TailscaleCLI, tailnet.Discover,
		)
		if err != nil {
			log.Fatalf("errand serve: %v", err)
		}
	}
	d, err := daemon.New(daemon.Config{
		Listen:           addr,
		StateDir:         fileCfg.StateDir,
		AllowUsers:       fileCfg.AllowUsers,
		Capability:       fileCfg.Capability,
		TailscaledSocket: fileCfg.TailscaledSocket,
		Identity:         identity,
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
	handler := d.Handler()
	socketPath := fileCfg.SocketPath()
	unixListener, err := listenUnixSocket(socketPath)
	if err != nil {
		log.Fatalf("errand serve: %v", err)
	}
	defer os.Remove(socketPath)
	if tcpEnabled {
		mode := "INSECURE no-auth"
		if identity != nil {
			mode = "tailnet whois via " + identity.Name()
		}
		log.Printf("errand %s serving on %s (%s) and %s (SSH); state %s",
			version, addr, mode, socketPath, fileCfg.StateDir)
	} else {
		log.Printf("errand %s serving on %s (SSH only); state %s", version, socketPath, fileCfg.StateDir)
	}

	unixServer := &http.Server{
		Handler:           handler,
		ConnContext:       daemon.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errs := make(chan error, 2)
	go func() { errs <- unixServer.Serve(unixListener) }()
	if tcpEnabled {
		tcpServer := &http.Server{
			Addr: addr, Handler: handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
		go func() { errs <- tcpServer.ListenAndServe() }()
	}
	if err := <-errs; err != nil {
		log.Fatalf("errand serve: %v", err)
	}
	return 0
}

type tailnetDiscoverFunc func(string, string) (tailnet.Provider, error)

func resolveServeTransport(
	listen string,
	insecure bool,
	socket string,
	cli string,
	discover tailnetDiscoverFunc,
) (string, tailnet.Provider, error) {
	var provider tailnet.Provider
	var selfIPs func(context.Context) ([]string, error)
	host, _, splitErr := net.SplitHostPort(listen)
	needsTailnetAddress := splitErr == nil && host == "tailnet"
	if !insecure || needsTailnetAddress {
		var err error
		provider, err = discover(socket, cli)
		if err != nil {
			return "", nil, err
		}
		selfIPs = provider.SelfIPs
	}
	addr, err := config.ResolveListen(listen, selfIPs)
	if err != nil {
		return "", nil, err
	}
	if insecure {
		provider = nil
	}
	return addr, provider, nil
}

// listenUnixSocket binds the daemon's local socket, replacing a stale one
// left by a previous instance (the state-dir flock already guarantees a
// single live daemon per state directory).
func listenUnixSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("local socket path %q exists and is not a socket", path)
		}
		conn, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("local socket %q already has a live listener", path)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("checking existing local socket %q: %w", path, dialErr)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale local socket: %w", err)
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("binding local socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

// cmdStdio bridges an SSH session to the daemon's Unix socket.
func cmdStdio(args []string) int {
	fs := flag.NewFlagSet("errand _stdio", flag.ContinueOnError)
	socketFlag := fs.String("socket", "", "daemon Unix socket (default from errandd.toml / state dir)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	socket := *socketFlag
	if socket == "" {
		cfg, err := config.LoadDaemon("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "errand _stdio: %v\n", err)
			return 1
		}
		socket = cfg.SocketPath()
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand _stdio: no runner at %s (is errand serve running here?): %v\n", socket, err)
		return 1
	}
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(os.Stdout, conn)
		done <- struct{}{}
	}()
	<-done // either direction ending ends the bridge; the other unblocks on close
	return 0
}
