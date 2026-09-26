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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
	"github.com/lydakis/errand/internal/tailnet"
	"github.com/lydakis/errand/internal/termui"
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
	case "remove", "rm":
		return cmdPeersRemove(args[1:], stdout, stderr, deps)
	case "discover":
		return cmdPeersDiscover(args[1:], stdout, stderr, deps)
	}
	if strings.HasPrefix(args[0], "-") {
		return cmdPeersList(args, stdout, stderr, deps)
	}
	e := newConsole(stdout, stderr).Err
	e.Errorf("unknown peers command '%s'", args[0])
	if guess := termui.Suggest(args[0], []string{"add", "remove", "discover"}); guess != "" {
		e.Hintf("did you mean errand peers %s?", guess)
	} else {
		e.Hintf("use add, remove or discover; errand peers --help")
	}
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
	if p.Socket != "" {
		return p.Socket
	}
	if p.SSH != "" {
		return "ssh://" + p.SSH
	}
	return ""
}

func cmdPeersAdd(args []string, stdout, stderr io.Writer, deps peersDeps) int {
	con := newConsole(stdout, stderr)
	e, o := con.Err, con.Out
	fs := flag.NewFlagSet("errand peers add", flag.ContinueOnError)
	sshMode := fs.Bool("ssh", false, "HOST is an ssh_config host; use the SSH transport")
	remoteCommand := fs.String("remote-command", "", "absolute errand path on the SSH host when not on its login PATH")
	remoteSocket := fs.String("remote-socket", "", "absolute daemon Unix socket path on the SSH host")
	force := fs.Bool("force", false, "replace an existing peer of the same name")
	fs.BoolVar(force, "f", false, "replace an existing peer of the same name")
	dryRun := fs.Bool("dry-run", false, "verify and show what would be written without writing")
	fs.BoolVar(dryRun, "n", false, "verify and show what would be written without writing")
	noVerify := fs.Bool("no-verify", false, "record the peer without probing it (offline runner)")
	if ok, code := parseFlags(fs, args, "peers add", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 2 {
		e.Errorf("errand peers add needs a NAME and a HOST")
		e.Hintf("for example errand peers add mini mini.tail6c3e93.ts.net")
		return 2
	}
	name, host := fs.Arg(0), fs.Arg(1)
	peer, err := parsePeerTarget(host, *sshMode, *remoteCommand, *remoteSocket)
	if err != nil {
		return usageError(e, "%v", err)
	}
	if err := config.ValidatePeer(name, peer); err != nil {
		return usageError(e, "%v", err)
	}
	path, err := deps.configPath()
	if err != nil {
		return failWith(e, 1, err, errorScope{})
	}
	plan, err := config.PlanAddPeer(path, name, peer, *force)
	if err != nil {
		e.Errorf("%v", err)
		if strings.Contains(err.Error(), "already") {
			e.Hintf("replace it with errand peers add --force %s %s", name, host)
		}
		return 1
	}
	peerURL := peerURLOf(peer)
	if !*noVerify {
		dialURL := client.ConfigureSSHPeer(peerURL, name, peer.RemoteCommand, peer.RemoteSocket)
		spin := e.Spin("Checking " + e.B(strings.TrimPrefix(strings.TrimPrefix(peerURL, "http://"), "https://")) + "…")
		info, err := deps.probe(context.Background(), dialURL)
		spin.Stop()
		if err != nil {
			printProbeFailure(e, name, peerURL, err, deps)
			return 1
		}
		facts := termui.Things(info.MaxJobs, "slot", "slots")
		if system := peerSystem(&info); system != "" {
			facts = system + " · " + facts
		}
		e.Say(termui.OK, e.B(terminalSafeField(name))+" is an errand "+terminalSafeField(info.Version)+" runner "+e.D("· "+facts))
	}
	if *dryRun {
		action := "Would add to"
		if plan.Replacing {
			action = "Would replace in"
		}
		o.Print(action + " " + homeRelative(path) + ":")
		o.Print("")
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
		return failWith(e, 1, err, errorScope{})
	}
	extra := ""
	if madeDefault {
		extra = " " + e.D("· it's your default runner now")
	}
	e.Say(termui.OK, "Added "+e.B(terminalSafeField(name))+" to "+homeRelative(path)+extra)
	if madeDefault {
		e.Next("errand -- make test", "runs on "+name)
	} else {
		e.Next("errand --on "+name+" -- make test", "")
	}
	return 0
}

func printProbeFailure(e *termui.Stream, name, peerURL string, err error, deps peersDeps) {
	name = terminalSafeField(name)
	host := terminalSafeField(strings.TrimPrefix(strings.TrimPrefix(peerURL, "http://"), "https://"))
	detail := terminalSafeField(err.Error())
	kind, _ := client.ProbeKindOf(err)
	switch kind {
	case client.ProbeForbidden:
		e.Errorf("%s is an errand runner, but it refused you", host)
		login := callerLogin(deps)
		if login == "" {
			login = "YOUR_LOGIN"
		}
		e.Hintf("on %s, run errand access add %s, then errand setup to restart it (pass both the same --config its service uses, if it isn't the default)", name, login)
		e.Print("  " + e.D("runner said: "+detail))
	case client.ProbeNotErrand:
		e.Errorf("%s answered, but it isn't an errand runner", host)
		e.Print("  " + e.D(detail))
	default:
		e.Errorf("%s didn't answer (%s)", host, peerProblem(peerRow{Detail: detail}))
		e.Hintf("run errand setup on that machine first, or add it anyway with --no-verify")
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
	e := newConsole(stdout, stderr).Err
	fs := flag.NewFlagSet("errand peers remove", flag.ContinueOnError)
	if ok, code := parseFlags(fs, args, "peers remove", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 1 {
		e.Errorf("errand peers remove needs a runner name")
		e.Hintf("errand peers lists them")
		return 2
	}
	name := fs.Arg(0)
	path, err := deps.configPath()
	if err != nil {
		return failWith(e, 1, err, errorScope{})
	}
	clearedDefault, err := config.RemovePeer(path, name)
	if err != nil {
		if strings.Contains(err.Error(), "not configured") {
			e.Errorf("no runner named %s", name)
			e.Hintf("%s", knownRunnersHint(name))
			return 1
		}
		return failWith(e, 1, err, errorScope{})
	}
	extra := ""
	if cfg, err := deps.load(); err == nil && cfg.DefaultPeer != "" && !clearedDefault {
		extra = " " + e.D("· "+cfg.DefaultPeer+" is still the default")
	}
	e.Say(termui.OK, "Removed "+e.B(terminalSafeField(name))+" from "+homeRelative(path)+extra)
	if clearedDefault {
		e.Warnf("%s was your default runner; there's no default now", name)
		e.Next("errand peers add NAME HOST", "the first runner you add becomes the default")
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
	info       *proto.Info
}

func cmdPeersDiscover(args []string, stdout, stderr io.Writer, deps peersDeps) int {
	con := newConsole(stdout, stderr)
	e, o := con.Err, con.Out
	fs := flag.NewFlagSet("errand peers discover", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	all := false
	fs.BoolVar(&all, "all", false, "include offline nodes and nodes that are not runners")
	fs.BoolVar(&all, "a", false, "include offline nodes and nodes that are not runners")
	if ok, code := parseFlags(fs, args, "peers discover", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg, err := deps.load()
	if err != nil {
		return failWith(e, 1, err, errorScope{})
	}
	provider, err := deps.provider()
	if err != nil {
		e.Errorf("couldn't talk to Tailscale: %v", err)
		e.Hintf("discover finds runners on your tailnet; make sure Tailscale is running here")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes, err := provider.Peers(ctx)
	if err != nil {
		e.Errorf("couldn't list tailnet nodes: %v", err)
		return 1
	}
	self, _ := provider.Self(ctx)
	configuredHosts := configuredPeerHosts(cfg) // host (name, FQDN, or IP) -> alias

	var spin *termui.Spinner
	if !*jsonOutput {
		spin = e.Spin("Probing " + termui.Things(countOnline(nodes), "online tailnet node", "online tailnet nodes") + "…")
	}
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
			rows[i].info = &info
			rows[i].Detail = fmt.Sprintf("%s/%s, %d cpu, kvm=%v, %s", info.Facts.OS, info.Facts.Arch, info.Facts.NumCPU, info.Facts.KVM, termui.Things(info.MaxJobs, "slot", "slots"))
		}(i, target)
	}
	wg.Wait()
	if spin != nil {
		spin.Stop()
	}

	var shown []discoveredRow
	runners, added, others := 0, 0, 0
	for _, r := range rows {
		switch {
		case r.Status == "runner":
			runners++
			if r.Configured != "" {
				added++
			}
		case r.Status != "offline":
			others++
		}
		if all || r.Status == "runner" || r.Status == "forbidden" {
			shown = append(shown, r)
		}
	}
	sort.SliceStable(shown, func(i, j int) bool { return shown[i].Name < shown[j].Name })
	if *jsonOutput {
		return writeJSONRows(stdout, stderr, shown)
	}
	if len(shown) == 0 {
		o.Print("No errand runners answered among " + termui.Things(countOnline(nodes), "online tailnet node", "online tailnet nodes") + ".")
		o.Next("errand setup", "on a machine makes it a runner")
		if !all {
			o.Next("errand peers discover -a", "lists every node")
		}
		return 0
	}
	t := o.Table("NODE", "RUNNER", "SYSTEM")
	for _, r := range shown {
		name := termui.C(terminalSafeField(r.Name), termui.Bold)
		switch r.Status {
		case "runner":
			status := termui.C("✓ added as "+terminalSafeField(r.Configured), termui.Green)
			if r.Configured == "" {
				status = termui.C("● not added yet", termui.Yellow)
			}
			parts := []string{}
			if s := peerSystem(r.info); s != "" {
				parts = append(parts, s)
			}
			parts = append(parts, termui.Things(r.info.MaxJobs, "slot", "slots"))
			if r.Version != version {
				parts = append(parts, "errand "+terminalSafeField(r.Version))
			}
			system := strings.Join(parts, " · ")
			t.Row(name, status, termui.C(system))
		case "forbidden":
			t.Row(name, termui.C("○ refused you", termui.Red), termui.C(terminalSafeField(r.OS), termui.Dim))
		default:
			label := map[string]string{"none": "○ no answer", "not-errand": "○ not a runner", "offline": "○ offline"}[r.Status]
			detail := r.OS
			if cause := peerProblem(peerRow{Detail: r.Detail}); cause != "" && r.Detail != "" {
				detail += " · " + cause
			}
			t.Row(termui.C(terminalSafeField(r.Name), termui.Dim), termui.C(label, termui.Dim), termui.C(terminalSafeField(detail), termui.Dim))
		}
	}
	t.Print()
	summary := ""
	switch {
	case runners > 0 && added == runners && runners == 2:
		summary = "Both runners are already added."
	case runners > 0 && added == runners:
		summary = "All " + termui.Things(runners, "runner is", "runners are") + " already added."
		if runners == 1 {
			summary = "The runner is already added."
		}
	case runners > added:
		summary = termui.Things(runners-added, "runner isn't", "runners aren't") + " added yet."
	}
	if !all && others > 0 {
		summary += " " + termui.Things(others, "other node isn't a runner", "other nodes aren't runners") + " (errand peers discover -a)."
	}
	if summary != "" {
		o.Print(o.D(strings.TrimSpace(summary)))
	}
	for _, r := range shown {
		switch r.Status {
		case "runner":
			if r.Configured == "" {
				o.Next("errand peers add "+terminalSafeField(r.Name)+" "+terminalSafeField(r.DNSName), "")
			}
		case "forbidden":
			if self.Login != "" {
				o.Warnf("%s refused you; on it, run errand access add %s, then errand setup", terminalSafeField(r.Name), self.Login)
			} else {
				o.Warnf("%s refused you; add your tailnet login to its allow_users", terminalSafeField(r.Name))
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
