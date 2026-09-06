package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
)

const setupUsage = `usage: errand setup [options]

Turn this machine into an errand runner. New runners default to both SSH
and Tailscale; connect Tailscale later and rerun setup if it is unavailable.
--ssh and --tailscale save a single-transport preference in errandd.toml.
Plain setup respects the saved transport setting, which you can edit anytime.

Install and start the platform service (systemd user unit + linger on Linux,
a launch agent on macOS), and prove the daemon answers. Setup preserves
unrelated configuration and service definitions unless --force is given.
It restarts the service only after the runner has no active jobs.`

func cmdSetup(args []string) int {
	return cmdSetupTo(args, os.Stdout, os.Stderr, setup.RealSystem{})
}

func cmdSetupTo(args []string, stdout, stderr io.Writer, sys setup.System) int {
	fs := flag.NewFlagSet("errand setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
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
	fs.Usage = func() {
		fmt.Fprintln(stderr, setupUsage)
		fmt.Fprintln(stderr, "\noptions:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand setup: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *maxJobs <= 0 {
		fmt.Fprintln(stderr, "errand setup: --max-jobs must be positive")
		return 2
	}
	if *socket != "" && *cli != "" {
		fmt.Fprintln(stderr, "errand setup: --tailscaled-socket and --tailscale-cli are mutually exclusive")
		return 2
	}
	if *ssh && (*tailscale || *socket != "" || *cli != "" || len(allow) != 0 || *printACL) {
		fmt.Fprintln(stderr, "errand setup: --ssh conflicts with Tailscale options")
		return 2
	}
	transport := ""
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
	report, err := setup.Run(ctx, setup.Options{
		Transport: transport, ConfigPath: *cfgPath, MaxJobs: *maxJobs, AllowUsers: allow,
		Socket: *socket, CLI: *cli, Force: *force, DryRun: *dryRun,
	}, sys)
	printSetupReport(stdout, report, *dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "errand setup: %v\n", err)
		return 1
	}
	if report.Failed() {
		return 1
	}
	return 0
}

func printSetupReport(w io.Writer, r *setup.Report, dryRun bool) {
	if r == nil {
		return
	}
	if dryRun {
		fmt.Fprintln(w, "errand setup (dry run; nothing changed)")
	}
	for _, s := range r.Steps {
		mark := "·"
		switch {
		case s.Err != nil:
			mark = "✗"
		case s.Changed:
			mark = "✓"
		}
		fmt.Fprintf(w, "%s %-8s %s\n", mark, s.Name, s.Detail)
	}
	sshOnly := strings.EqualFold(strings.TrimSpace(r.Config.Listen), "none")
	if r.Failed() || r.Config.Listen == "" || (r.Self.DNSName == "" && !sshOnly) {
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
	runnerLabel := "runner"
	if r.Self.DNSName != "" {
		runnerLabel += " " + short
	}
	fmt.Fprintln(w)
	if dryRun {
		fmt.Fprintf(w, "%s would be configured", runnerLabel)
	} else {
		fmt.Fprintf(w, "%s is ready", runnerLabel)
	}
	if r.Info != nil {
		fmt.Fprintf(w, " (%s/%s, %d cpu, kvm=%v, %d slot(s))", r.Info.Facts.OS, r.Info.Facts.Arch, r.Info.Facts.NumCPU, r.Info.Facts.KVM, r.Info.MaxJobs)
	}
	fmt.Fprintln(w)
	if sshOnly {
		fmt.Fprintln(w, "access: SSH as the user running this service (no tailnet listener)")
	} else if len(r.Config.AllowUsers) == 0 {
		fmt.Fprintln(w, "allows: tailnet capability grants")
	} else {
		fmt.Fprintf(w, "allows: %s\n", strings.Join(r.Config.AllowUsers, ", "))
	}
	if !sshOnly && len(r.Config.DenyUsers) != 0 {
		fmt.Fprintf(w, "denies (overrides tailnet grants): %s\n", strings.Join(r.Config.DenyUsers, ", "))
	}
	if r.Config.Transport != config.TransportTailscale && sshHost == "YOUR_SSH_HOST" {
		fmt.Fprintln(w, "Replace YOUR_SSH_HOST with your SSH host or ssh_config alias; buildbox is a name you choose.")
	}
	fmt.Fprintf(w, "\nOn a client, add to ~/.config/errand/config.toml:\n\n")
	if peerURL, ok := setupPeerURL(r.Config.Listen, r.Self.DNSName); ok {
		fmt.Fprintf(w, "    [peers.%s]\n    url = %q\n", short, peerURL)
		if r.Config.Transport != config.TransportTailscale {
			fmt.Fprintf(w, "\nOr use SSH:\n\n")
		}
	}
	if r.Config.Transport == config.TransportTailscale {
		return
	}
	fmt.Fprintf(w, "    [peers.%s]\n    ssh = %q\n", short, sshHost)
	if r.RemoteCommand != "" {
		fmt.Fprintf(w, "    remote_command = %q\n", r.RemoteCommand)
	}
	fmt.Fprintf(w, "    remote_socket = %q\n", r.SocketPath)
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
