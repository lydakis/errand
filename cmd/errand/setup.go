package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
	"github.com/lydakis/errand/internal/termui"
)

func cmdSetup(args []string) int {
	return cmdSetupTo(args, os.Stdout, os.Stderr, setup.RealSystem{})
}

func cmdSetupTo(args []string, stdout, stderr io.Writer, sys setup.System) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand setup", flag.ContinueOnError)
	local := fs.Bool("local", false, "save local-only transport; no network listener or SSH bridge")
	ssh := fs.Bool("ssh", false, "save SSH-only transport in the runner config")
	tailscale := fs.Bool("tailscale", false, "save Tailscale-only transport in the runner config")
	maxJobs := fs.Int("max-jobs", 1, "concurrent job slots")
	cfgPath := fs.String("config", "", "runner config path (default ~/.config/errand/errandd.toml)")
	socket := fs.String("tailscaled-socket", "", "explicit tailscaled LocalAPI socket")
	cli := fs.String("tailscale-cli", "", "explicit tailscale CLI path (standalone macOS app)")
	force := fs.Bool("force", false, "rewrite an existing config or service definition")
	fs.BoolVar(force, "f", false, "rewrite an existing config or service definition")
	dryRun := fs.Bool("dry-run", false, "decide and report without changing anything")
	fs.BoolVar(dryRun, "n", false, "decide and report without changing anything")
	printACL := fs.Bool("print-acl", false, "print the tailnet ACL grant for capability-based authorization and exit")
	var allow stringList
	fs.Var(&allow, "allow-user", "additional tailnet login granted full runner access (repeatable)")
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "setup", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *maxJobs <= 0 {
		return usageError(e, "--max-jobs must be at least 1")
	}
	if *socket != "" && *cli != "" {
		return usageError(e, "--tailscaled-socket and --tailscale-cli can't be combined")
	}
	if *local && (*ssh || *tailscale || *socket != "" || *cli != "" || len(allow) != 0 || *printACL) {
		return usageError(e, "--local can't be combined with network transport options")
	}
	if *ssh && (*tailscale || *socket != "" || *cli != "" || len(allow) != 0 || *printACL) {
		return usageError(e, "--ssh can't be combined with Tailscale options")
	}
	transport := ""
	if *local {
		transport = config.TransportLocal
	}
	if *ssh {
		transport = config.TransportSSH
	}
	if *tailscale {
		transport = config.TransportTailscale
	}
	if *printACL {
		fmt.Fprint(stdout, setup.RenderACL(proto.DefaultCapability, setup.DefaultPort))
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var spin *termui.Spinner
	if !output.quiet {
		verb := "Setting up this machine as a runner…"
		if *dryRun {
			verb = "Checking what setup would do…"
		}
		spin = e.Spin(verb)
	}
	report, err := setup.Run(ctx, setup.Options{
		ExpectedVersion: version,
		Transport:       transport, ConfigPath: *cfgPath, MaxJobs: *maxJobs, AllowUsers: allow,
		Socket: *socket, CLI: *cli, Force: *force, DryRun: *dryRun,
	}, sys)
	if spin != nil {
		spin.Stop()
	}
	if !output.quiet || report != nil && report.Failed() {
		printSetupReport(con.Out, report, *dryRun, output.verbose)
	}
	if err != nil {
		return failWith(e, 1, err, errorScope{})
	}
	if report.Failed() {
		return 1
	}
	return 0
}

// setupStepNames are how setup's steps read on screen.
var setupStepNames = map[string]string{
	"tailnet": "Tailscale", "ssh": "SSH bridge", "config": "Config", "service": "Service",
	"linger": "Linger", "path": "PATH", "probe": "Runner",
}

var setupNodeRE = regexp.MustCompile(`this node is ([^,]+), owned by (\S+)`)

// setupStepText shortens a step's detail for the default view; -v keeps it.
func setupStepText(step setup.Step, r *setup.Report, verbose bool) string {
	detail := step.Detail
	if verbose {
		return strings.ReplaceAll(detail, "\n", "\n"+strings.Repeat(" ", 15))
	}
	first, _, _ := strings.Cut(detail, "\n")
	first = strings.TrimSuffix(first, ":")
	switch step.Name {
	case "tailnet":
		if m := setupNodeRE.FindStringSubmatch(first); m != nil {
			return m[1] + " · " + m[2]
		}
	case "ssh":
		head, _, _ := strings.Cut(first, ";")
		return strings.Replace(head, "SSH bridge enabled", "on", 1)
	case "config":
		if step.Changed && r != nil {
			return first + " · " + setupTransportText(r.Config) + " · " + termui.Things(r.Config.MaxJobs, "slot", "slots")
		}
	case "probe":
		if first == "skipped (dry run)" {
			return "skipped in a dry run"
		}
	}
	return first
}

func setupTransportText(c setup.ConfigChoice) string {
	switch {
	case c.Transport == config.TransportLocal:
		return "local only"
	case c.Transport == config.TransportSSH, strings.EqualFold(strings.TrimSpace(c.Listen), "none"):
		return "SSH only"
	case c.Transport == config.TransportTailscale:
		return "tailnet " + strings.TrimPrefix(c.Listen, "tailnet")
	default:
		return "SSH + tailnet " + strings.TrimPrefix(c.Listen, "tailnet")
	}
}

func printSetupReport(s *termui.Stream, r *setup.Report, dryRun, verbose bool) {
	if r == nil {
		return
	}
	if dryRun {
		s.Print(s.Paint("Dry run", termui.Yellow) + " " + s.D("· nothing changes"))
		s.Print("")
	}
	for _, step := range r.Steps {
		if !verbose && step.Err == nil && strings.HasPrefix(step.Detail, "would run: ") {
			continue // the command behind a service change is -v detail
		}
		glyph := termui.OK
		switch {
		case step.Err != nil:
			glyph = termui.Fail
		case strings.HasPrefix(step.Detail, "skipped"), strings.HasPrefix(step.Detail, "unavailable"):
			glyph = termui.Skip
		}
		name := setupStepNames[step.Name]
		if name == "" {
			name = step.Name
		}
		text := homeRelativeText(setupStepText(step, r, verbose))
		if step.Err != nil {
			s.Print(s.G(glyph) + " " + padRight(name, 11) + " " + s.Paint(text, termui.Red))
			continue
		}
		s.Print(s.G(glyph) + " " + padRight(name, 11) + " " + s.D(text))
	}
	if r.Failed() {
		return
	}
	if r.Config.Transport == config.TransportLocal {
		s.Print("")
		if dryRun {
			s.Print("This machine would accept local jobs only; no network listener or SSH bridge.")
		} else {
			s.Print(s.B("Local runner ready.") + " It accepts local jobs only; no network listener or SSH bridge.")
		}
		s.Next("errand --on local -- make test", "")
		if verbose {
			s.Print(s.D(fmt.Sprintf("For a custom runner config, set socket = %q in a personal [peers.NAME] table.", r.SocketPath)))
		}
		return
	}
	sshOnly := strings.EqualFold(strings.TrimSpace(r.Config.Listen), "none")
	if r.Config.Listen == "" || (r.Self.DNSName == "" && !sshOnly) {
		return
	}
	short := r.Self.DNSName
	if i := strings.IndexByte(short, '.'); i > 0 {
		short = short[:i]
	}
	sshHost := short
	if short == "" {
		short = "buildbox"
		sshHost = "YOUR_SSH_HOST"
	}
	s.Print("")
	who := "people with tailnet capability grants"
	switch {
	case sshOnly:
		who = "anyone who can SSH in as this user"
	case len(r.Config.AllowUsers) != 0:
		who = strings.Join(r.Config.AllowUsers, ", ")
	}
	name := s.B(short)
	if r.Self.DNSName == "" {
		name = "This machine"
	}
	facts := ""
	if r.Info != nil {
		facts = " " + s.D("("+peerSystem(r.Info)+" · "+termui.Things(r.Info.MaxJobs, "slot", "slots")+")")
	}
	if dryRun {
		s.Print(name + " would accept jobs from " + who + ".")
	} else {
		s.Print(name + " is ready" + facts + ". It accepts jobs from " + who + ".")
	}
	if !sshOnly && len(r.Config.DenyUsers) != 0 {
		s.Print(s.D("Denied even with a grant: " + strings.Join(r.Config.DenyUsers, ", ")))
	}
	if sshHost == "YOUR_SSH_HOST" {
		s.Print(s.D("Replace YOUR_SSH_HOST with your SSH host or ssh_config alias; buildbox is a name you choose."))
	}
	s.Print("On another machine, add it with:")
	if _, ok := setupPeerURL(r.Config.Listen, r.Self.DNSName); ok {
		s.Print("  " + s.Hint("errand peers add "+short+" "+r.Self.DNSName))
	} else {
		// Options go before NAME HOST: peers add stops reading them there.
		cmd := "errand peers add --ssh"
		if r.RemoteCommand != "" {
			cmd += " --remote-command " + termui.ShellQuote([]string{r.RemoteCommand})
		}
		if r.SocketPath != "" {
			cmd += " --remote-socket " + termui.ShellQuote([]string{r.SocketPath})
		}
		cmd += " " + short + " " + sshHost
		s.Print("  " + s.Hint(cmd))
	}
	if !verbose {
		return
	}
	s.Print("")
	s.Print(s.D("Or add it to ~/.config/errand/config.toml yourself:"))
	if peerURL, ok := setupPeerURL(r.Config.Listen, r.Self.DNSName); ok {
		s.Print(s.D(fmt.Sprintf("    [peers.%s]\n    url = %q", short, peerURL)))
	}
	if r.Config.Transport == config.TransportTailscale {
		return
	}
	block := fmt.Sprintf("    [peers.%s]\n    ssh = %q", short, sshHost)
	if r.RemoteCommand != "" {
		block += fmt.Sprintf("\n    remote_command = %q", r.RemoteCommand)
	}
	block += fmt.Sprintf("\n    remote_socket = %q", r.SocketPath)
	s.Print(s.D(block))
}

// homeRelativeText shortens any home-directory paths inside a sentence.
func homeRelativeText(text string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return text
	}
	return strings.ReplaceAll(text, home+"/", "~/")
}

func setupPeerURL(listen, dnsName string) (string, bool) {
	if strings.EqualFold(strings.TrimSpace(listen), "none") {
		return "", false
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", false
	}
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "tailnet") || host == "" || ip != nil && ip.IsUnspecified() {
		host = dnsName
	}
	if host == "" {
		return "", false
	}
	return "http://" + net.JoinHostPort(host, port), true
}
