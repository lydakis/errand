package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/termui"
)

// commandHelp describes one command's --help page. Options are rendered from
// the command's FlagSet so help can't drift from what parses.
type commandHelp struct {
	summary  string
	usage    []string
	order    []string // long flag names in display order; others follow
	hidden   []string // accepted but not listed
	text     map[string]string
	notes    []string
	examples [][2]string // command, what it does
}

// flagArgs names the value a flag takes in help.
var flagArgs = map[string]string{
	"on": "PEER", "url": "URL", "where": "FACTS", "profile": "NAME", "workspace": "NAME",
	"workdir": "DIR", "env": "NAME=VALUE", "passenv": "NAME", "env-file": "FILE",
	"forward": "[LOCAL:]REMOTE", "artifact": "PATH", "cache": "NAME=PATH",
	"workspace-root": "PATH", "last": "N", "output": "DIR", "older-than": "DURATION",
	"keep": "N", "config": "PATH", "allow-user": "LOGIN", "max-jobs": "N",
	"listen": "ADDR", "state-dir": "DIR", "tailscale-cli": "PATH",
	"tailscaled-socket": "PATH", "remote-command": "PATH", "remote-socket": "PATH",
}

// flagText is the default one-line description of each flag.
var flagText = map[string]string{
	"json":     "Machine-readable output",
	"quiet":    "Print only data and errors",
	"verbose":  "Show every detail",
	"on":       "Only this runner",
	"url":      "Only this runner address",
	"dry-run":  "Show what would happen without changing anything",
	"force":    "Replace what's already there",
	"apply":    "Apply the changed files here after a clean success",
	"no-apply": "Leave changed files on the runner",
	"all":      "Include everything, not just the usual",
}

var helpPages = map[string]commandHelp{
	"attach": {
		summary: "Follow a job's output: live while it runs, replayed after it ends.",
		usage:   []string{"errand attach [options] HANDLE"},
		order:   []string{"forward", "no-forward", "profile", "on", "url"},
		text: map[string]string{
			"forward": "Forward a port while attached (repeatable)", "no-forward": "Ignore configured forwards",
			"profile": "Use forwards from a named profile", "on": "Runner, if the handle doesn't name one", "url": "Runner address",
		},
		examples: [][2]string{{"errand attach mini/01M3BFTQ6QD4", ""}, {"errand attach -L 3000 cabal/01M3BF93Y7HS", "forward port 3000 while attached"}},
		notes:    []string{"Ctrl-D detaches and leaves the job running; Ctrl-C interrupts it."},
	},
	"kill": {
		summary:  "Stop a job and wait for it to end.",
		usage:    []string{"errand kill [options] HANDLE"},
		order:    []string{"force", "no-wait", "on", "url"},
		text:     map[string]string{"force": "Send SIGKILL instead of SIGTERM", "no-wait": "Return as soon as the signal is sent", "on": "Runner, if the handle doesn't name one", "url": "Runner address"},
		examples: [][2]string{{"errand kill mini/01M3BFYR7PQX", ""}, {"errand kill -f mini/01M3BFYR7PQX", "when SIGTERM isn't enough"}},
	},
	"ps": {
		summary:  "List jobs on your runners. Active jobs by default.",
		usage:    []string{"errand ps [options]"},
		order:    []string{"all", "last", "on", "url", "workspace", "verbose", "json"},
		text:     map[string]string{"all": "Include finished jobs", "last": "Only the latest N, across all states", "workspace": "Only jobs in this persistent workspace", "verbose": "Times, source and workdir for each job"},
		examples: [][2]string{{"errand ps -a -n 20", ""}, {"errand ps --on mini --json | jq '.[].state'", ""}},
	},
	"status": {
		summary:  "Show one job: how it ended, what it ran, and what changed.",
		usage:    []string{"errand status [options] HANDLE"},
		order:    []string{"verbose", "json", "on", "url"},
		text:     map[string]string{"verbose": "Timestamps, hashes, logs, cleanup and limits", "on": "Runner, if the handle doesn't name one", "url": "Runner address"},
		examples: [][2]string{{"errand status mini/01M3BFTQ6QD4", ""}, {"errand status -v mini/01M3BFTQ6QD4", ""}},
	},
	"fetch": {
		summary: "Bring a finished job's changed files here.",
		usage:   []string{"errand fetch [options] HANDLE [PATH]"},
		order:   []string{"apply", "conflicts", "output", "json", "on", "url"},
		text: map[string]string{
			"apply": "Apply the files to this checkout (refuses on conflicts)", "conflicts": "With --apply: write conflict markers and apply the rest",
			"output": "Export the files into a new directory instead", "on": "Runner, if the handle doesn't name one", "url": "Runner address",
		},
		notes:    []string{"Without --apply or --output, files are downloaded and staged; nothing in your checkout changes."},
		examples: [][2]string{{"errand fetch --apply mini/01M3BFTQ6QD4", ""}, {"errand fetch -o ./results mini/01M3BFTQ6QD4", ""}, {"errand fetch --apply mini/01M3BFTQ6QD4 go.sum", "just one file"}},
	},
	"push": {
		summary: "Send local edits to a persistent workspace.",
		usage:   []string{"errand push --workspace NAME [options] [PATH]"},
		order:   []string{"workspace", "apply", "watch", "conflicts", "on", "url", "profile", "workspace-root", "include-all", "json"},
		text: map[string]string{
			"workspace": "Workspace to update", "apply": "Replace files in the workspace (otherwise only staged)",
			"watch": "Keep pushing as files change; Ctrl-C stops", "conflicts": "With --apply: write conflict markers and apply the rest",
			"on": "Runner that holds the workspace", "profile": "Settings from a named profile", "workspace-root": "Snapshot root containing this directory",
			"include-all": "Allow a broad snapshot (never /)",
		},
		notes:    []string{"errand replaces files only; it doesn't rebuild or restart anything in the workspace."},
		examples: [][2]string{{"errand push --watch --apply --on mini --workspace dev", ""}, {"errand push --apply --on mini --workspace dev src/main.go", "just one file"}},
	},
	"workspaces": {
		summary: "Manage persistent workspaces: directories that stay on a runner between jobs.",
		usage:   []string{"errand workspaces [--on PEER] [--json]", "errand workspaces create [options] NAME", "errand workspaces rm [--on PEER] NAME"},
		order:   []string{"on", "url", "where", "profile", "workspace-root", "no-snapshot", "include-all", "artifact", "no-artifacts", "cache", "no-caches", "json"},
		text: map[string]string{
			"on": "Runner", "where": "Any runner matching, e.g. os=linux", "profile": "Settings from a named profile",
			"workspace-root": "Snapshot root containing this directory", "no-snapshot": "Start the workspace empty",
			"include-all": "Allow a broad snapshot (never /)", "artifact": "Keep an ignored output (repeatable)",
			"no-artifacts": "Ignore configured artifacts", "cache": "Bind a runner cache (repeatable)", "no-caches": "Ignore configured caches",
		},
		examples: [][2]string{{"errand workspaces create --on mini dev", ""}, {"errand --workspace dev -- make test", ""}, {"errand workspaces rm --on mini dev", ""}},
	},
	"peers": {
		summary:  "Show your runners and whether they're ready.",
		usage:    []string{"errand peers [--on PEER | --url URL] [-v] [--json]", "errand peers add NAME HOST", "errand peers remove NAME", "errand peers discover [-a] [--json]"},
		order:    []string{"on", "url", "verbose", "json"},
		text:     map[string]string{"verbose": "Address, transport and version for each runner"},
		examples: [][2]string{{"errand peers discover", "find runners on your tailnet"}, {"errand peers add mini mini.tail6c3e93.ts.net", ""}},
	},
	"peers add": {
		summary: "Check a runner answers, then save it to your config.",
		usage:   []string{"errand peers add [options] NAME HOST"},
		order:   []string{"ssh", "remote-command", "remote-socket", "force", "dry-run", "no-verify"},
		text: map[string]string{
			"ssh": "HOST is an ssh_config host; connect over SSH", "remote-command": "errand's path on the SSH host, if not on its PATH",
			"remote-socket": "Runner socket path on the SSH host", "force": "Replace a runner with the same name",
			"dry-run": "Check and show what would be saved", "no-verify": "Save without checking (for an offline runner)",
		},
		notes:    []string{"HOST is a name, host:port, URL, or with --ssh an ssh_config host."},
		examples: [][2]string{{"errand peers add mini mini.tail6c3e93.ts.net", ""}, {"errand peers add --ssh buildbox buildbox", ""}},
	},
	"peers remove": {summary: "Remove a runner from your config.", usage: []string{"errand peers remove NAME"}},
	"peers discover": {
		summary: "Find errand runners on your tailnet.",
		usage:   []string{"errand peers discover [-a] [--json]"},
		order:   []string{"all", "json"},
		text:    map[string]string{"all": "Also list nodes that aren't runners"},
		notes:   []string{"Read-only: each online node gets one authenticated info request."},
	},
	"df": {
		summary:  "Show storage used on each runner and on this machine.",
		usage:    []string{"errand df [options]"},
		order:    []string{"verbose", "on", "url", "json"},
		text:     map[string]string{"verbose": "Break down each location, largest first"},
		examples: [][2]string{{"errand df -v --on mini", ""}},
	},
	"gc": {
		summary: "Free space on a runner or on this machine.",
		usage:   []string{"errand gc TARGET [options]"},
		notes: []string{
			"Targets:",
			"  cache    snapshot blobs and named caches past the runner's expiry or budget",
			"  jobs     finished job records; needs --older-than or --keep",
			"  changes  fetched changes here, or push staging on a runner with --on; needs --older-than",
			"  all      every category on one runner (not every runner), plus local changes; needs --older-than",
			"With several runners, pick one with --on, even for --dry-run.",
		},
		examples: [][2]string{{"errand gc all --dry-run --on cabal --older-than 7d", ""}, {"errand gc jobs --on mini --keep 50", ""}},
	},
	"gc cache": {
		summary: "Remove snapshot blobs and named caches past the runner's expiry or budget.",
		usage:   []string{"errand gc cache [options]"}, order: []string{"dry-run", "on", "url", "verbose"},
		text:  map[string]string{"on": "Runner", "url": "Runner address", "verbose": "Show the runner's expiry and budget policy"},
		notes: []string{"Named caches in use by a running job are kept."},
	},
	"gc jobs": {
		summary: "Remove finished job records and their storage.",
		usage:   []string{"errand gc jobs (--older-than DURATION | --keep N) [options]"}, order: []string{"older-than", "keep", "dry-run", "on", "url"},
		text:  map[string]string{"older-than": "Only jobs older than this (e.g. 7d, 12h)", "keep": "Always keep the newest N", "on": "Runner", "url": "Runner address"},
		notes: []string{"With both limits, only jobs outside both are removed. Active jobs and jobs whose changes you haven't fetched are kept."},
	},
	"gc changes": {
		summary: "Remove fetched changes on this machine, or push staging on a runner with --on.",
		usage:   []string{"errand gc changes --older-than DURATION [options]"}, order: []string{"older-than", "dry-run", "on", "url", "verbose"},
		text: map[string]string{"older-than": "Only changes older than this (e.g. 30d)", "on": "Clean push staging on this runner instead", "url": "Runner address", "verbose": "List records that were skipped"},
	},
	"gc all": {
		summary: "Collect every category on one runner, plus local changes.",
		usage:   []string{"errand gc all --older-than DURATION [options]"}, order: []string{"older-than", "keep", "dry-run", "on", "url", "verbose"},
		text: map[string]string{"older-than": "Only data older than this (e.g. 7d)", "keep": "Always keep the newest N jobs", "on": "Runner", "url": "Runner address", "verbose": "Show policies and skipped records"},
	},
	"config": {
		summary:  "Show the settings a run would use and where each comes from.",
		usage:    []string{"errand config [run options]"},
		order:    []string{"verbose", "json"},
		text:     map[string]string{"verbose": "Full source paths, env files and profile details"},
		hidden:   runFlagNames,
		notes:    []string{"Accepts every run option (see errand --help), to preview what it would change."},
		examples: [][2]string{{"errand config --on mini --apply", ""}},
	},
	"doctor": {
		summary:  "Check this installation and every configured runner.",
		usage:    []string{"errand doctor [run options]"},
		order:    []string{"config", "verbose", "json"},
		text:     map[string]string{"config": "Also check this local runner config", "verbose": "Details for every check"},
		hidden:   runFlagNames,
		notes:    []string{"Accepts every run option, to check a specific setup.", doctorScope},
		examples: [][2]string{{"errand doctor", ""}, {"errand doctor --on mini -v", ""}},
	},
	"setup": {
		summary: "Turn this machine into an errand runner.",
		usage:   []string{"errand setup [options]"},
		order:   []string{"dry-run", "ssh", "tailscale", "local", "max-jobs", "allow-user", "config", "force", "tailscale-cli", "tailscaled-socket", "print-acl", "verbose"},
		text: map[string]string{
			"dry-run": "Show what would change without changing anything", "ssh": "Accept jobs over SSH only",
			"tailscale": "Accept jobs over Tailscale only", "local": "Accept local jobs only; no network listener",
			"max-jobs": "Jobs that run at once (default 1)", "allow-user": "Give this tailnet login full runner access (repeatable)",
			"config": "Runner config path (default ~/.config/errand/errandd.toml)", "force": "Rewrite an existing config or service definition",
			"tailscale-cli": "tailscale CLI path (standalone macOS app)", "tailscaled-socket": "tailscaled socket path",
			"print-acl": "Print the tailnet ACL grant and exit", "verbose": "Show the full config and service commands",
		},
		notes: []string{
			"New runners accept jobs over both SSH and Tailscale; --ssh, --tailscale or --local saves a single transport.",
			"Setup installs a service (systemd user unit on Linux, launch agent on macOS), starts it and checks it answers. It keeps unrelated settings unless --force, and refuses to restart while jobs are running.",
		},
		examples: [][2]string{{"errand setup --dry-run", ""}, {"errand setup --max-jobs 2", ""}},
	},
	"access": {
		summary: "Manage who may use this runner.",
		usage:   []string{"errand access list [options]", "errand access add|remove|deny|undeny [options] LOGIN"},
		order:   []string{"config", "dry-run", "verbose", "json"},
		text:    map[string]string{"config": "Runner config (default ~/.config/errand/errandd.toml)", "verbose": "Show saved values before and after"},
		notes:   []string{accessActivation},
	},
	"serve": {
		summary: "Run the runner in the foreground. errand setup installs it as a service instead.",
		usage:   []string{"errand serve [options]"},
		order:   []string{"config", "listen", "state-dir", "allow-user", "insecure-no-auth"},
		text: map[string]string{
			"config": "Runner config path", "listen": `Listen address ("tailnet:7443" uses the tailnet IP; "none" disables TCP)`,
			"state-dir": "Where job records live", "allow-user": "Give this tailnet login full runner access (repeatable)",
			"insecure-no-auth": "Skip all authorization (tests only; dangerous)",
		},
	},
	"version": {
		summary: "Print errand's version.",
		usage:   []string{"errand version [-v]"},
		order:   []string{"verbose"},
		text:    map[string]string{"verbose": "Also show the build and your runners' versions"},
	},
}

// runFlagNames are the run options config and doctor accept silently.
var runFlagNames = []string{"L", "forward", "no-forward", "cache", "no-caches", "artifact", "no-artifacts", "e", "env", "passenv", "env-file", "no-env-files", "profile", "workspace", "where", "on", "url", "w", "workdir", "workspace-root", "apply", "no-apply", "no-snapshot"}

// parseFlags parses a command's flags. --help prints the help page on stdout;
// a bad flag prints one error line with a pointer to --help. ok is false
// when the caller should return code.
func parseFlags(fs *flag.FlagSet, args []string, name string, stdout io.Writer, e *termui.Stream) (ok bool, code int) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		printCommandHelp(stdout, name, fs)
		return false, 0
	case err != nil:
		e.Errorf("%s", cleanFlagError(err))
		e.Hintf("errand %s --help", name)
		return false, 2
	}
	return true, 0
}

// cleanFlagError rephrases the flag package's errors with double dashes.
func cleanFlagError(err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "flag provided but not defined: "):
		return "unknown option " + dashed(strings.TrimPrefix(msg, "flag provided but not defined: "))
	case strings.HasPrefix(msg, "flag needs an argument: "):
		return dashed(strings.TrimPrefix(msg, "flag needs an argument: ")) + " needs a value"
	case strings.HasPrefix(msg, "invalid value "):
		// invalid value "x" for flag -n: parse error
		rest := strings.TrimPrefix(msg, "invalid value ")
		if value, tail, ok := strings.Cut(rest, " for flag "); ok {
			name, reason, _ := strings.Cut(tail, ": ")
			if reason == "parse error" {
				reason = "not a valid value"
			}
			return "invalid value " + value + " for " + dashed(name) + ": " + reason
		}
	case strings.HasPrefix(msg, "invalid boolean value "):
		return msg
	}
	return msg
}

func dashed(flagName string) string {
	name := strings.TrimLeft(flagName, "-")
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// flagRow is one line of an options list: aliases share a row.
type flagRow struct {
	long, short string
	arg         string
	text        string
}

func helpRows(fs *flag.FlagSet, page commandHelp) []flagRow {
	hidden := map[string]bool{}
	for _, h := range page.hidden {
		hidden[h] = true
	}
	var all []*flag.Flag
	fs.VisitAll(func(f *flag.Flag) { all = append(all, f) })
	byValue := map[flag.Value][]*flag.Flag{}
	var values []flag.Value
	for _, f := range all {
		if _, seen := byValue[f.Value]; !seen {
			values = append(values, f.Value)
		}
		byValue[f.Value] = append(byValue[f.Value], f)
	}
	rows := map[string]flagRow{}
	var names []string
	for _, v := range values {
		var row flagRow
		skip := false
		for _, f := range byValue[v] {
			if hidden[f.Name] {
				skip = true
			}
			if len(f.Name) == 1 {
				row.short = f.Name
			} else if row.long == "" || len(f.Name) > len(row.long) {
				row.long = f.Name
			}
		}
		if skip {
			continue
		}
		key := row.long
		if key == "" {
			key, row.long, row.short = row.short, row.short, ""
		}
		first := byValue[v][0]
		row.text = page.text[key]
		if row.text == "" {
			row.text = flagText[key]
		}
		if row.text == "" {
			row.text = sentence(first.Usage)
		}
		if _, isBool := first.Value.(interface{ IsBoolFlag() bool }); !isBool {
			row.arg = flagArgs[key]
			if row.arg == "" {
				row.arg = strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
			}
		}
		rows[key] = row
		names = append(names, key)
	}
	var ordered []flagRow
	placed := map[string]bool{}
	for _, name := range page.order {
		if row, ok := rows[name]; ok && !placed[name] {
			ordered = append(ordered, row)
			placed[name] = true
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if !placed[name] {
			ordered = append(ordered, rows[name])
			placed[name] = true
		}
	}
	return ordered
}

func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func helpStream(w io.Writer) *termui.Stream {
	return termui.Detect(w, io.Discard).Out
}

// printCommandHelp renders one command's help page.
func printCommandHelp(w io.Writer, name string, fs *flag.FlagSet) {
	page := helpPages[name]
	s := helpStream(w)
	var lines []string
	if page.summary != "" {
		lines = append(lines, page.summary, "")
	}
	lines = append(lines, s.B("Usage"))
	for _, u := range page.usage {
		lines = append(lines, "  "+u)
	}
	if rows := helpRows(fs, page); len(rows) > 0 {
		lines = append(lines, "", s.B("Options"))
		lines = append(lines, renderRows(s, rows)...)
	}
	for _, note := range page.notes {
		lines = append(lines, "")
		lines = append(lines, wrapPlain(note, 78)...)
	}
	if len(page.examples) > 0 {
		lines = append(lines, "", s.B("Examples"))
		width := 0
		for _, ex := range page.examples {
			width = max(width, len(ex[0]))
		}
		for _, ex := range page.examples {
			line := "  " + ex[0]
			if ex[1] != "" {
				line += strings.Repeat(" ", width-len(ex[0])+3) + s.D(ex[1])
			}
			lines = append(lines, line)
		}
	}
	fmt.Fprintln(w, strings.Join(lines, "\n"))
}

func renderRows(s *termui.Stream, rows []flagRow) []string {
	labels := make([]string, len(rows))
	plain := make([]string, len(rows))
	width := 0
	for i, r := range rows {
		label, text := "", ""
		if r.short != "" {
			label = s.ID("-"+r.short) + ", "
			text = "-" + r.short + ", "
		} else {
			label, text = "    ", "    "
		}
		flagName := dashed(r.long)
		label += s.ID(flagName)
		text += flagName
		if r.arg != "" {
			label += " " + s.ID(r.arg)
			text += " " + r.arg
		}
		labels[i], plain[i] = label, text
		width = max(width, len(text))
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = "  " + labels[i] + strings.Repeat(" ", width-len(plain[i])+3) + r.text
	}
	return out
}

func wrapPlain(text string, width int) []string {
	if strings.HasPrefix(text, "  ") || !strings.Contains(text, " ") || len(text) <= width {
		return []string{text}
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		if line == "" {
			line = word
		} else {
			line += " " + word
		}
	}
	return append(lines, line)
}

// commands lists every subcommand, for help and typo suggestions.
var commands = []string{"peers", "workspaces", "ps", "status", "attach", "fetch", "push", "kill", "df", "gc", "config", "doctor", "setup", "access", "serve", "version"}

// printRootHelp renders errand --help. all includes rarely used run options.
func printRootHelp(w io.Writer) { printRootHelpAll(w, false) }

func printRootHelpAll(w io.Writer, all bool) {
	s := helpStream(w)
	opt := func(flags, text string) string {
		return "  " + s.ID(flags) + strings.Repeat(" ", max(1, 24-len(flags))) + text
	}
	lines := []string{
		"Run a command on another machine you own.",
		"",
		s.B("Usage"),
		"  errand [options] -- COMMAND [ARG...]",
		"  errand SUBCOMMAND [options]",
		"",
		s.B("Choose a runner"),
		opt("--on PEER", "A runner by name, or local for this machine"),
		opt("--where FACTS", "Any runner matching, e.g. os=linux,go"),
		opt("--url URL", "A runner by address"),
		opt("--profile NAME", "Settings from a named profile"),
		"",
		s.B("Files"),
		opt("--apply, --no-apply", "Bring changed files back after a clean success"),
		opt("--artifact PATH", "Also keep an ignored output (repeatable)"),
		opt("--cache NAME=PATH", "Reuse a runner cache (repeatable)"),
		opt("--workspace NAME", "Run in a persistent workspace"),
		opt("--no-snapshot", "Start from an empty directory"),
	}
	if all {
		lines = append(lines,
			opt("--workspace-root PATH", "Snapshot root containing this directory"),
			opt("--include-all", "Allow a broad snapshot (never /)"),
			opt("--no-artifacts", "Ignore configured artifacts"),
			opt("--no-caches", "Ignore configured caches"),
			opt("--env-file FILE", "Load variables from a file (repeatable)"),
			opt("--no-env-files", "Ignore configured env files"),
			opt("--no-forward", "Ignore configured port forwards"),
		)
	} else {
		lines = append(lines, "  "+s.D("--workspace-root, --include-all, --no-artifacts, --no-caches and more: errand --help-all"))
	}
	lines = append(lines,
		"",
		s.B("Session"),
		opt("-d, --detach", "Start in the background and print the job handle"),
		opt("-w, --workdir DIR", "Directory inside the workspace to run in"),
		opt("-e, --env NAME=VALUE", "Set a variable; --passenv NAME forwards yours"),
		opt("-L, --forward PORT", "Forward a port while attached"),
		opt("-q, --quiet", "Only your command's output"),
		opt("-v, --verbose", "Every step, with timings"),
		"",
		s.B("Subcommands"),
		"  "+s.D("Jobs   ")+"    ps  status  attach  kill",
		"  "+s.D("Files  ")+"    fetch  push  workspaces",
		"  "+s.D("Runners")+"    peers  setup  access  serve",
		"  "+s.D("Upkeep ")+"    df  gc  config  doctor  version",
		"",
		s.B("Examples"),
		"  errand -- make test                      "+s.D("on your default runner"),
		"  errand --on mini --apply -- gofmt -w .   "+s.D("format on mini, bring the files back"),
		"  errand fetch --apply mini/01M3BFTQ6QD4   "+s.D("apply a finished job's changes"),
		"",
		s.D("errand SUBCOMMAND --help for details · Ctrl-D detaches · Ctrl-C interrupts"),
	)
	fmt.Fprintln(w, strings.Join(lines, "\n"))
}

// unknownCommand handles arguments that are neither a subcommand nor a run
// with "--": a typo, or a command that forgot the separator.
func unknownCommand(e *termui.Stream, args []string) int {
	if len(args) == 0 {
		printRootHelp(os.Stderr)
		return 2
	}
	first := args[0]
	if strings.HasPrefix(first, "-") {
		e.Errorf(`missing "--" before the command`)
		example := "errand " + strings.Join(runExample(args), " ")
		e.Hintf("put the command after --, e.g. %s", example)
		return 2
	}
	e.Errorf("unknown command '%s'", first)
	if guess := termui.Suggest(first, commands); guess != "" {
		e.Hintf("did you mean errand %s? To run a program on a runner, put it after --: errand -- %s", guess, strings.Join(args, " "))
	} else {
		e.Hintf("to run it on a runner, put it after --: errand -- %s", strings.Join(args, " "))
	}
	return 2
}

// runExample inserts "--" after the leading options: [--on mini make test]
// becomes [--on mini -- make test].
func runExample(args []string) []string {
	valued := map[string]bool{"--on": true, "--url": true, "--where": true, "--profile": true, "-w": true, "--workdir": true, "-e": true, "--env": true, "--passenv": true, "-L": true, "--forward": true, "--artifact": true, "--cache": true, "--workspace": true, "--workspace-root": true, "--env-file": true}
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if valued[args[i]] && !strings.Contains(args[i], "=") {
			i++
		}
		i++
	}
	if i > len(args) {
		i = len(args)
	}
	out := append([]string{}, args[:i]...)
	out = append(out, "--")
	if i == len(args) {
		return append(out, "COMMAND")
	}
	return append(out, args[i:]...)
}
