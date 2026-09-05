package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
	"github.com/lydakis/errand/internal/tailnet"
)

const peersUsage = `usage:
  errand peers [--json] [--on PEER | --url URL]
                                    # runner status, capacity, and capabilities
  errand peers add NAME HOST         # verify a runner, then record it (HOST: name, host:port, URL, or --ssh)
  errand peers remove NAME
  errand peers discover [-a | --all] [--json]

Discovery is read-only and scoped to the caller's tailnet: it probes each
online node's errand port with an authenticated /v0/info request and prints
exact "peers add" commands for the ones that admit you.`

const probeTimeout = 4 * time.Second

type peersDeps struct {
	configPath func() (string, error)
	load       func() (config.Client, error)
	probe      func(ctx context.Context, peerURL string) (proto.Info, error)
	provider   func() (tailnet.Provider, error)
}

func realPeersDeps() peersDeps {
	return peersDeps{
		configPath: config.ClientPath,
		load:       config.LoadClient,
		probe: func(ctx context.Context, peerURL string) (proto.Info, error) {
			return client.ProbeInfo(ctx, peerURL, probeTimeout)
		},
		provider: func() (tailnet.Provider, error) { return tailnet.Discover("", "") },
	}
}

func cmdPeers(args []string) int {
	return cmdPeersTo(args, os.Stdout, os.Stderr, realPeersDeps())
}

func cmdPeersTo(args []string, stdout, stderr io.Writer, deps peersDeps) int {
	if len(args) == 0 {
		return cmdPeersList(args, stdout, stderr, deps)
	}
	switch args[0] {
	case "add":
		return cmdPeersAdd(args[1:], stdout, stderr, deps)
	case "remove":
		return cmdPeersRemove(args[1:], stdout, stderr, deps)
	case "discover":
		return cmdPeersDiscover(args[1:], stdout, stderr, deps)
	case "-h", "--help":
		fmt.Fprintln(stderr, peersUsage)
		return 0
	}
	if strings.HasPrefix(args[0], "-") {
		return cmdPeersList(args, stdout, stderr, deps)
	}
	fmt.Fprintf(stderr, "errand peers: unknown subcommand %q\n\n%s\n", args[0], peersUsage)
	return 2
}

func parsePeerTarget(host string, sshMode bool, remoteCommand, remoteSocket string) (config.Peer, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return config.Peer{}, fmt.Errorf("HOST must not be empty")
	}
	if sshMode {
		if strings.Contains(host, "://") {
			return config.Peer{}, fmt.Errorf("--ssh wants an ssh_config host or user@host, not a URL")
		}
		return config.Peer{SSH: host, RemoteCommand: remoteCommand, RemoteSocket: remoteSocket}, nil
	}
	if remoteCommand != "" || remoteSocket != "" {
		return config.Peer{}, fmt.Errorf("--remote-command and --remote-socket apply only with --ssh")
	}
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Host == "" {
			return config.Peer{}, fmt.Errorf("invalid peer URL %q", host)
		}
		switch u.Scheme {
		case "http", "https":
			return config.Peer{URL: strings.TrimSuffix(host, "/")}, nil
		default:
			return config.Peer{}, fmt.Errorf("unsupported scheme %q (use http://, https://, or --ssh)", u.Scheme)
		}
	}
	if strings.ContainsAny(host, "/?# ") {
		return config.Peer{}, fmt.Errorf("HOST %q must be a host name, host:port, or URL", host)
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return config.Peer{URL: "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(setup.DefaultPort))}, nil
	}
	if strings.Contains(host, ":") {
		hostname, port, err := net.SplitHostPort(host)
		if err != nil || hostname == "" {
			return config.Peer{}, fmt.Errorf("HOST %q must use host:port syntax", host)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return config.Peer{}, fmt.Errorf("HOST %q has an invalid port", host)
		}
		return config.Peer{URL: "http://" + net.JoinHostPort(hostname, port)}, nil
	}
	return config.Peer{URL: fmt.Sprintf("http://%s:%d", host, setup.DefaultPort)}, nil
}

func peerURLOf(p config.Peer) string {
	if p.URL != "" {
		return p.URL
	}
	return "ssh://" + p.SSH
}

func cmdPeersAdd(args []string, stdout, stderr io.Writer, deps peersDeps) int {
	fs := flag.NewFlagSet("errand peers add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sshMode := fs.Bool("ssh", false, "HOST is an ssh_config host; use the SSH transport")
	remoteCommand := fs.String("remote-command", "", "absolute errand path on the SSH host when not on its login PATH")
	remoteSocket := fs.String("remote-socket", "", "absolute daemon Unix socket path on the SSH host")
	force := fs.Bool("force", false, "replace an existing peer of the same name")
	fs.BoolVar(force, "f", false, "replace an existing peer of the same name")
	dryRun := fs.Bool("dry-run", false, "verify and show what would be written without writing")
	fs.BoolVar(dryRun, "n", false, "verify and show what would be written without writing")
	noVerify := fs.Bool("no-verify", false, "record the peer without probing it (offline runner)")
	setFlagUsage(fs, "errand peers add [--ssh] [--remote-command PATH] [--remote-socket PATH] [-f | --force] [-n | --dry-run] [--no-verify] NAME HOST")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "errand peers add: NAME and HOST are required")
		return 2
	}
	name, host := fs.Arg(0), fs.Arg(1)
	peer, err := parsePeerTarget(host, *sshMode, *remoteCommand, *remoteSocket)
	if err != nil {
		fmt.Fprintf(stderr, "errand peers add: %v\n", err)
		return 2
	}
	if err := config.ValidatePeer(name, peer); err != nil {
		fmt.Fprintf(stderr, "errand peers add: %v\n", err)
		return 2
	}
	path, err := deps.configPath()
	if err != nil {
		fmt.Fprintf(stderr, "errand peers add: %v\n", err)
		return 1
	}
	plan, err := config.PlanAddPeer(path, name, peer, *force)
	if err != nil {
		fmt.Fprintf(stderr, "errand peers add: %v\n", err)
		return 1
	}
	peerURL := peerURLOf(peer)
	if !*noVerify {
		dialURL := client.ConfigureSSHPeer(peerURL, name, peer.RemoteCommand, peer.RemoteSocket)
		info, err := deps.probe(context.Background(), dialURL)
		if err != nil {
			printProbeFailure(stderr, name, peerURL, err, deps)
			return 1
		}
		fmt.Fprintf(stdout, "%s: errand %s on %s/%s, %d cpu, kvm=%v, %d slot(s)\n",
			terminalSafeField(name), terminalSafeField(info.Version), terminalSafeField(info.Facts.OS),
			terminalSafeField(info.Facts.Arch), info.Facts.NumCPU, info.Facts.KVM, info.MaxJobs)
	}
	if *dryRun {
		action := "add to"
		if plan.Replacing {
			action = "replace in"
		}
		fmt.Fprintf(stdout, "would %s %s:\n\n", action, path)
		if plan.MadeDefault {
			fmt.Fprintf(stdout, "    default_peer = %q\n\n", name)
		}
		fmt.Fprintf(stdout, "    [peers.%s]\n", name)
		if peer.URL != "" {
			fmt.Fprintf(stdout, "    url = %q\n", peer.URL)
		} else {
			fmt.Fprintf(stdout, "    ssh = %q\n", peer.SSH)
			if peer.RemoteCommand != "" {
				fmt.Fprintf(stdout, "    remote_command = %q\n", peer.RemoteCommand)
			}
			if peer.RemoteSocket != "" {
				fmt.Fprintf(stdout, "    remote_socket = %q\n", peer.RemoteSocket)
			}
		}
		return 0
	}
	madeDefault, err := config.AddPeer(path, name, peer, *force)
	if err != nil {
		fmt.Fprintf(stderr, "errand peers add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "added peer %s (%s) to %s\n", name, peerURL, path)
	if madeDefault {
		fmt.Fprintf(stdout, "%s is now the default peer: `errand -- CMD` runs there\n", name)
	} else {
		fmt.Fprintf(stdout, "use it with: errand --on %s -- CMD\n", name)
	}
	return 0
}

func printProbeFailure(stderr io.Writer, name, peerURL string, err error, deps peersDeps) {
	name = terminalSafeField(name)
	peerURL = terminalSafeField(peerURL)
	detail := terminalSafeField(err.Error())
	kind, _ := client.ProbeKindOf(err)
	switch kind {
	case client.ProbeForbidden:
		login := callerLogin(deps)
		fmt.Fprintf(stderr, "errand peers add: %s (%s) is an errand runner but refused this caller.\n", name, peerURL)
		if login != "" {
			fmt.Fprintf(stderr, "  on the runner, add %q to allow_users in the config used by its errand service, then rerun `errand setup` with the same `--config` value (omit it for the default config)\n", login)
		} else {
			fmt.Fprintln(stderr, "  on the runner, add your tailnet login to allow_users in the config used by its errand service, then rerun `errand setup` with the same `--config` value (omit it for the default config)")
		}
		fmt.Fprintf(stderr, "  runner said: %s\n", detail)
	case client.ProbeNotErrand:
		fmt.Fprintf(stderr, "errand peers add: %s (%s) answered, but it is not an errand runner: %s\n", name, peerURL, detail)
	default:
		fmt.Fprintf(stderr, "errand peers add: %s (%s) did not answer: %s\n", name, peerURL, detail)
		fmt.Fprintf(stderr, "  if errand is not installed there yet, run `errand setup` on that machine; to record it anyway, pass --no-verify\n")
	}
}

func callerLogin(deps peersDeps) string {
	if deps.provider == nil {
		return ""
	}
	provider, err := deps.provider()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	self, err := provider.Self(ctx)
	if err != nil {
		return ""
	}
	return self.Login
}

func cmdPeersRemove(args []string, stdout, stderr io.Writer, deps peersDeps) int {
	fs := flag.NewFlagSet("errand peers remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setFlagUsage(fs, "errand peers remove NAME")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "errand peers remove: NAME is required")
		return 2
	}
	path, err := deps.configPath()
	if err != nil {
		fmt.Fprintf(stderr, "errand peers remove: %v\n", err)
		return 1
	}
	clearedDefault, err := config.RemovePeer(path, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "errand peers remove: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed peer %s from %s\n", fs.Arg(0), path)
	if clearedDefault {
		fmt.Fprintln(stdout, "it was the default peer; set another with `errand peers add` or edit default_peer")
	}
	return 0
}

type discoveredRow struct {
	Name       string `json:"name"`
	DNSName    string `json:"dns_name"`
	OS         string `json:"os"`
	Status     string `json:"status"` // runner | forbidden | none | not-errand | offline
	Version    string `json:"version,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Configured string `json:"configured_as,omitempty"`
}

func cmdPeersDiscover(args []string, stdout, stderr io.Writer, deps peersDeps) int {
	fs := flag.NewFlagSet("errand peers discover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	all := false
	fs.BoolVar(&all, "all", false, "include offline nodes and nodes that are not runners")
	fs.BoolVar(&all, "a", false, "include offline nodes and nodes that are not runners")
	setFlagUsage(fs, "errand peers discover [-a | --all] [--json]")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand peers discover: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	cfg, err := deps.load()
	if err != nil {
		fmt.Fprintf(stderr, "errand peers discover: %v\n", err)
		return 1
	}
	provider, err := deps.provider()
	if err != nil {
		fmt.Fprintf(stderr, "errand peers discover: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, err := provider.Peers(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "errand peers discover: listing tailnet peers: %v\n", err)
		return 1
	}
	self, _ := provider.Self(ctx)
	configuredHosts := configuredPeerHosts(cfg) // host (name, FQDN, or IP) -> alias

	rows := make([]discoveredRow, len(nodes))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, node := range nodes {
		short := node.DNSName
		if j := strings.IndexByte(short, '.'); j > 0 {
			short = short[:j]
		}
		target := fmt.Sprintf("http://%s:%d", node.DNSName, setup.DefaultPort)
		rows[i] = discoveredRow{Name: short, DNSName: node.DNSName, OS: node.OS, Configured: configuredAliasFor(configuredHosts, node)}
		if !node.Online {
			rows[i].Status = "offline"
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, target string) {
			defer wg.Done()
			defer func() { <-sem }()
			info, err := deps.probe(ctx, target)
			if err != nil {
				kind, _ := client.ProbeKindOf(err)
				switch kind {
				case client.ProbeForbidden:
					rows[i].Status = "forbidden"
				case client.ProbeNotErrand:
					rows[i].Status = "not-errand"
				default:
					rows[i].Status = "none"
				}
				rows[i].Detail = err.Error()
				return
			}
			rows[i].Status = "runner"
			rows[i].Version = info.Version
			rows[i].Detail = fmt.Sprintf("%s/%s, %d cpu, kvm=%v, %d slot(s)", info.Facts.OS, info.Facts.Arch, info.Facts.NumCPU, info.Facts.KVM, info.MaxJobs)
		}(i, target)
	}
	wg.Wait()

	var shown []discoveredRow
	for _, r := range rows {
		if all || r.Status == "runner" || r.Status == "forbidden" {
			shown = append(shown, r)
		}
	}
	if *jsonOutput {
		return writeJSONRows(stdout, stderr, shown)
	}
	if len(shown) == 0 {
		fmt.Fprintf(stdout, "no errand runners answered among %d online tailnet node(s); run `errand setup` on a machine to make it one (--all lists every node)\n", countOnline(nodes))
		return 0
	}
	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tOS\tSTATUS\tVERSION\tDETAIL")
	for _, r := range shown {
		name := terminalSafeField(r.Name)
		if r.Configured != "" {
			name += " (configured as " + terminalSafeField(r.Configured) + ")"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, terminalSafeField(r.OS),
			terminalSafeField(r.Status), terminalSafeField(r.Version), terminalSafeField(r.Detail))
	}
	w.Flush()
	fmt.Fprintln(stdout)
	for _, r := range shown {
		switch r.Status {
		case "runner":
			if r.Configured == "" {
				fmt.Fprintf(stdout, "  errand peers add %s %s\n", terminalSafeField(r.Name), terminalSafeField(r.DNSName))
			}
		case "forbidden":
			if self.Login != "" {
				fmt.Fprintf(stdout, "  %s refused you; on it, add %q to allow_users in ~/.config/errand/errandd.toml, then run `errand setup` to restart it\n", terminalSafeField(r.Name), self.Login)
			} else {
				fmt.Fprintf(stdout, "  %s refused you; add your tailnet login to its allow_users\n", terminalSafeField(r.Name))
			}
		}
	}
	return 0
}

// configuredPeerHosts indexes configured peers by addressable host.
func configuredPeerHosts(cfg config.Client) map[string]string {
	hosts := map[string]string{}
	for alias, p := range cfg.Peers {
		host := ""
		switch {
		case p.URL != "":
			if u, err := url.Parse(p.URL); err == nil {
				host = u.Hostname()
			}
		case p.SSH != "":
			host = p.SSH
			if i := strings.IndexByte(host, '@'); i >= 0 {
				host = host[i+1:]
			}
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if host != "" {
			hosts[host] = alias
		}
	}
	return hosts
}

func configuredAliasFor(hosts map[string]string, node tailnet.Peer) string {
	candidates := []string{strings.ToLower(node.DNSName), strings.ToLower(node.HostName)}
	if i := strings.IndexByte(node.DNSName, '.'); i > 0 {
		candidates = append(candidates, strings.ToLower(node.DNSName[:i]))
	}
	candidates = append(candidates, node.IPs...)
	for _, c := range candidates {
		if alias, ok := hosts[c]; ok && c != "" {
			return alias
		}
	}
	return ""
}

func countOnline(nodes []tailnet.Peer) int {
	n := 0
	for _, node := range nodes {
		if node.Online {
			n++
		}
	}
	return n
}

func writeJSONRows(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "errand peers: %v\n", err)
		return 1
	}
	return 0
}
