// errand: run the thing you would have run locally, on another machine
// you own. One binary, two roles: `errand serve` receives, everything
// else delegates.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
)

var version = "0.1.0-dev"

const usage = `errand — a personal job runner for machines you own

usage:
  errand [--on PEER | --url URL] [--env K=V]... [--passenv K]... [--workdir REL] [--include-all] -- CMD [ARG...]
  errand serve [--config PATH] [--listen ADDR] [--state-dir DIR] [--allow-user LOGIN]...
  errand info [--on PEER | --url URL]
  errand version

Exit status: the remote process's own exit code. If that code is 0 but the
transaction itself fails, errand exits 120; secondary failures accompanying
a nonzero remote exit are reported without replacing that exit code.

Snapshot safety: Git worktrees are selected automatically. Other directories
require .errandignore or --include-all. Filesystem roots are always refused;
the user's home directory requires --include-all.`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "serve":
		os.Exit(cmdServe(args[1:]))
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
	rawURL := fs.String("url", "", "peer base URL (overrides --on)")
	workdir := fs.String("workdir", "", "working directory, relative to the workspace root")
	includeAll := fs.Bool("include-all", false, "allow an otherwise refused broad snapshot (never permits a filesystem root)")
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

	peerURL, err := resolvePeer(*rawURL, *on)
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
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "errand: %v\n", err)
		return client.ExitTransaction
	}
	return client.Run(client.RunOptions{
		PeerURL:    peerURL,
		Root:       root,
		Argv:       argv,
		Env:        env,
		PassEnv:    passenvs,
		Workdir:    *workdir,
		IncludeAll: *includeAll,
	})
}

func resolvePeer(rawURL, on string) (string, error) {
	if rawURL != "" {
		return strings.TrimSuffix(rawURL, "/"), nil
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return "", err
	}
	return cfg.PeerURL(on)
}

func cmdInfo(args []string) int {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	fs.Parse(args)
	peerURL, err := resolvePeer(*rawURL, *on)
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
