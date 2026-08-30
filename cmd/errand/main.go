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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

var version = "0.1.0-dev"

const usage = `errand — a personal job runner for machines you own

usage:
  errand [--on PEER | --url URL] [--detach] [--env K=V]... [--passenv K]...
         [--workdir REL] [--include-all | --no-snapshot] -- CMD [ARG...]
  errand attach [--on PEER | --url URL] HANDLE
  errand ps [--on PEER | --url URL]
  errand kill [--force] [--on PEER | --url URL] HANDLE
  errand caches [--on PEER | --url URL]
  errand gc [--on PEER | --url URL]
  errand serve [--config PATH] [--listen ADDR] [--state-dir DIR] [--allow-user LOGIN]...
  errand info [--on PEER | --url URL]
  errand version

A HANDLE is peer/ULID as printed at submission (a bare ULID works with
--on/--url or a configured default peer). --detach prints the handle on
stdout and returns after admission; the job keeps running on the peer.
When attached from a terminal, Ctrl-D detaches locally and prints the
reattach command; Ctrl-C sends SIGINT to the remote job and a second Ctrl-C
force-kills it. Ctrl-D exits 0 for successful detachment, not job completion.

Exit status: the remote process's own exit code. If that code is 0 but the
transaction itself fails, errand exits 120; secondary failures accompanying
a nonzero remote exit are reported without replacing that exit code.

Snapshot safety: Git worktrees are selected automatically. Other directories
require .errandignore or --include-all. Filesystem roots are always refused;
the user's home directory requires --include-all. --no-snapshot runs in a
fresh empty remote workspace without inspecting or transferring local files.`

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
		root, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "errand: %v\n", err)
			return client.ExitTransaction
		}
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
	fmt.Fprintf(os.Stderr, "errand: kill requested for %s/%s\n", label, jobID)
	return 0
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func cmdPs(args []string) int {
	return cmdPsTo(args, os.Stdout, os.Stderr)
}

func cmdPsTo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	on := fs.String("on", "", "restrict to one peer name")
	rawURL := fs.String("url", "", "restrict to one peer base URL")
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

	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "PEER\tJOB\tSTATE\tEXIT\tAGE\tCOMMAND")
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
			exit := "-"
			switch {
			case e.ExitCode != nil:
				exit = fmt.Sprintf("%d", *e.ExitCode)
			case e.Signal != "":
				exit = e.Signal
			}
			age := "-"
			if !e.AdmittedAt.IsZero() {
				age = shortDuration(time.Since(e.AdmittedAt))
			}
			commandRunes := []rune(e.Command)
			cmd := e.Command
			if len(commandRunes) > 60 {
				cmd = string(commandRunes[:59]) + "…"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", tgt.name, e.ID, e.State, exit, age, cmd)
		}
	}
	w.Flush()
	if !reached || failed {
		return 1
	}
	return 0
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
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
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	fs.Parse(args)
	peerURL, label, err := resolvePeerTarget(*rawURL, *on)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 1
	}
	result, err := client.CacheGC(peerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return 1
	}
	fmt.Printf("%s: removed %d blobs, freed %d bytes\n", label, result.RemovedBlobs, result.FreedBytes)
	return 0
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
