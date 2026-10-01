// errand: run the thing you would have run locally, on another machine
// you own. One binary, two roles: `errand serve` receives, everything
// else delegates.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
	"github.com/lydakis/errand/internal/tailnet"
	"github.com/lydakis/errand/internal/termui"
	"github.com/lydakis/errand/internal/workspace"
)

var version = "0.1.0-dev"

func main() { os.Exit(runCLI(os.Args[1:])) }

func runCLI(args []string) int {
	if len(args) == 0 {
		printRootHelp(os.Stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	case "setup":
		return cmdSetup(args[1:])
	case "peers":
		return cmdPeers(args[1:])
	case "workspaces":
		return cmdWorkspaces(args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "access":
		return cmdAccess(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "attach":
		return cmdAttach(args[1:])
	case "push":
		return cmdPush(args[1:])
	case "fetch":
		return cmdFetch(args[1:])
	case "ps":
		return cmdPs(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "kill":
		return cmdKill(args[1:])
	case "df":
		return cmdDf(args[1:])
	case "gc":
		return cmdGC(args[1:])
	case "version", "--version":
		return cmdVersion(args[1:])
	case "_automatic-apply":
		return cmdAutomaticApply(args[1:])
	case "_stdio":
		return cmdStdio(args[1:])
	case "-h", "--help", "help":
		printRootHelp(os.Stdout)
		return 0
	case "--help-all":
		printRootHelpAll(os.Stdout, true)
		return 0
	default:
		return cmdRun(args)
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
	local, remote, err := workspace.ParsePortForward(value)
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
// The ULID may be shortened to any unique prefix of at least four characters.
// The peer part must be a configured alias or an explicit HTTP(S) URL. A
// caller-supplied --url may route an alias-qualified handle on a machine that
// does not share that alias, but the resulting label is the effective URL.
func resolveHandle(handleArg, rawURL, on string) (peerURL, label, jobID string, err error) {
	prefix := ""
	jobID = handleArg
	if i := strings.LastIndexByte(handleArg, '/'); i >= 0 {
		prefix, jobID = handleArg[:i], handleArg[i+1:]
	}
	jobID = strings.ToUpper(jobID)
	short := !proto.ValidULID(jobID)
	if short && (len(jobID) < 4 || !proto.ValidULIDPrefix(jobID)) {
		return "", "", "", &badHandleError{handle: handleArg}
	}
	if rawURL != "" && on != "" {
		return "", "", "", fmt.Errorf("--on and --url are mutually exclusive")
	}
	switch {
	case rawURL != "":
		effectiveURL := strings.TrimSuffix(rawURL, "/")
		if strings.HasPrefix(prefix, "http://") || strings.HasPrefix(prefix, "https://") || strings.HasPrefix(prefix, "ssh://") || strings.HasPrefix(prefix, "unix://") {
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
	case strings.HasPrefix(prefix, "http://") || strings.HasPrefix(prefix, "https://") || strings.HasPrefix(prefix, "ssh://") || strings.HasPrefix(prefix, "unix://"):
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
	if short {
		jobID, err = client.ResolveJobPrefix(peerURL, jobID)
		if err != nil {
			return peerURL, label, "", err
		}
	}
	return peerURL, label, jobID, nil
}

// badHandleError is a HANDLE argument that can't name a job.
type badHandleError struct{ handle string }

func (e *badHandleError) Error() string { return fmt.Sprintf("%q isn't a job handle", e.handle) }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type psRow struct {
	applyNote      string
	Peer           string                       `json:"peer"`
	AutomaticApply *client.AutomaticApplyStatus `json:"automatic_apply,omitempty"`
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

var errNoUsablePeers = errors.New("no runners configured; add one with errand peers add NAME HOST, or find them with errand peers discover")

// readFleet standardizes the CLI contract for read-only discovery commands:
// query every configured peer unless explicitly narrowed, preserve partial
// results, report peer-specific failures, and fail the command if any selected
// peer could not be read.
func readFleet[T any](rawURL, on string, e *termui.Stream, query func(string) (T, error)) (fleetRead[T], error) {
	targets, warnings, err := peerTargets(rawURL, on)
	if err != nil {
		return fleetRead[T]{}, err
	}
	read := fleetRead[T]{targets: targets, failed: len(warnings) != 0}
	for _, warning := range warnings {
		e.Warnf("%v", warning)
	}
	if len(targets) == 0 {
		return read, errNoUsablePeers
	}
	for _, result := range queryPeerTargets(targets, query) {
		if result.err != nil {
			msg, _ := describeError(result.err, errorScope{peer: result.target.name})
			e.Warnf("%s: %s", result.target.name, msg)
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

	cfg = cfg.WithLocalPeer()
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	info, err := (setup.RealSystem{}).Probe(ctx, socket)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand _stdio: cannot reach runner at %s (run errand setup): %v\n", socket, err)
		return 1
	}
	if info.SSHDisabled {
		fmt.Fprintln(os.Stderr, "errand _stdio: SSH transport is disabled in the runner config; edit transport and rerun errand setup")
		return 1
	}
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand _stdio: no runner at %s (run errand setup on the runner): %v\n", socket, err)
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
