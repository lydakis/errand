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

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
)

const setupUsage = `usage: errand setup [options]

Turn this machine into an errand runner: discover how to reach tailscaled,
write ~/.config/errand/errandd.toml granting this node's own tailnet login
full runner access,
install and start the platform service (systemd user unit + linger on
Linux, a launch agent on macOS), make errand reachable for SSH callers, and
prove the daemon answers. Re-running is safe: existing config and service
definitions are kept unless --force is given, and the service is restarted
so the current config is active once the runner has no active jobs.`

func cmdSetup(args []string) int {
	return cmdSetupTo(args, os.Stdout, os.Stderr, setup.RealSystem{})
}

func cmdSetupTo(args []string, stdout, stderr io.Writer, sys setup.System) int {
	fs := flag.NewFlagSet("errand setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
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
	if *printACL {
		fmt.Fprint(stdout, setup.RenderACL(proto.DefaultCapability, setup.DefaultPort))
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := setup.Run(ctx, setup.Options{
		ConfigPath: *cfgPath, MaxJobs: *maxJobs, AllowUsers: allow,
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
	if r.Failed() || r.Self.DNSName == "" {
		return
	}
	short := r.Self.DNSName
	if i := strings.IndexByte(short, '.'); i > 0 {
		short = short[:i]
	}
	fmt.Fprintln(w)
	if dryRun {
		fmt.Fprintf(w, "runner %s would be configured", short)
	} else {
		fmt.Fprintf(w, "runner %s is ready", short)
	}
	if r.Info != nil {
		fmt.Fprintf(w, " (%s/%s, %d cpu, kvm=%v, %d slot(s))", r.Info.Facts.OS, r.Info.Facts.Arch, r.Info.Facts.NumCPU, r.Info.Facts.KVM, r.Info.MaxJobs)
	}
	fmt.Fprintln(w)
	if len(r.Config.AllowUsers) == 0 {
		fmt.Fprintln(w, "allows: tailnet capability grants")
	} else {
		fmt.Fprintf(w, "allows: %s\n", strings.Join(r.Config.AllowUsers, ", "))
	}
	if len(r.Config.DenyUsers) != 0 {
		fmt.Fprintf(w, "denies (overrides tailnet grants): %s\n", strings.Join(r.Config.DenyUsers, ", "))
	}
	fmt.Fprintf(w, "\nOn a client, add to ~/.config/errand/config.toml:\n\n")
	if peerURL, ok := setupPeerURL(r.Config.Listen, r.Self.DNSName); ok {
		fmt.Fprintf(w, "    [peers.%s]\n    url = %q\n", short, peerURL)
		fmt.Fprintf(w, "\nOr use SSH:\n\n")
	}
	fmt.Fprintf(w, "    [peers.%s]\n    ssh = %q\n", short, short)
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
