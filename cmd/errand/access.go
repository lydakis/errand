package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lydakis/errand/internal/config"
)

const accessActivation = "Saved configuration only; restart the runner to activate it. Capability grants and SSH access are separate."

func cmdAccess(args []string) int { return cmdAccessTo(args, os.Stdout, os.Stderr) }

func cmdAccessTo(args []string, stdout, stderr io.Writer) int {
	action := "list"
	if len(args) != 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	if action != "list" && action != "add" && action != "remove" {
		fmt.Fprintln(stderr, "usage: errand access [list|add|remove] [options] [LOGIN]")
		return 2
	}
	fs := flag.NewFlagSet("errand access "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "local runner config (default ~/.config/errand/errandd.toml)")
	asJSON := fs.Bool("json", false, "print saved policy or edit result as JSON")
	dryRun := false
	synopsis := "errand access " + action + " [options]"
	if action != "list" {
		synopsis += " LOGIN"
		fs.BoolVar(&dryRun, "dry-run", false, "preview allow_users without writing or restarting")
		fs.BoolVar(&dryRun, "n", false, "preview allow_users without writing or restarting")
	}
	setFlagUsage(fs, synopsis)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	wantArgs := 0
	if action != "list" {
		wantArgs = 1
	}
	if fs.NArg() != wantArgs {
		fmt.Fprintf(stderr, "usage: %s\n", synopsis)
		return 2
	}
	if action == "list" {
		policy, err := config.ReadAccess(*path)
		if err != nil {
			fmt.Fprintf(stderr, "errand access: %v\n", err)
			return 1
		}
		if *asJSON {
			return writeAccessJSON(stdout, stderr, struct {
				config.AccessPolicy
				Activation string `json:"activation"`
			}{policy, accessActivation})
		}
		fmt.Fprintf(stdout, "Runner config: %s\nListen: %s\nCapability: %s\n", terminalSafeField(policy.Path), terminalSafeField(policy.Listen), terminalSafeField(policy.Capability))
		fmt.Fprintln(stdout, "Configured allow_users (full runner access):")
		if len(policy.AllowUsers) == 0 {
			fmt.Fprintln(stdout, "  (none)")
		}
		for _, login := range policy.AllowUsers {
			fmt.Fprintf(stdout, "  %s\n", terminalSafeField(login))
		}
		fmt.Fprintln(stdout, accessActivation)
		return 0
	}
	login := fs.Arg(0)
	change, err := config.ChangeAccess(*path, login, action == "add", dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "errand access: %v\n", err)
		return 1
	}
	if *asJSON {
		return writeAccessJSON(stdout, stderr, struct {
			Operation string `json:"operation"`
			Login     string `json:"login"`
			DryRun    bool   `json:"dry_run"`
			config.AccessChange
			Activation string `json:"activation"`
		}{action, login, dryRun, change, accessActivation})
	}
	verb := "No change to"
	if change.Changed {
		verb = "Updated"
		if dryRun {
			verb = "Would update"
		}
	}
	fmt.Fprintf(stdout, "%s allow_users in %s\n", verb, terminalSafeField(change.Path))
	fmt.Fprintf(stdout, "Before: %q\nAfter:  %q\n", change.Before, change.After)
	if change.Changed {
		fmt.Fprintln(stdout, "Writing re-formats TOML and removes comments; other setting values are preserved.")
	}
	fmt.Fprintln(stdout, accessActivation)
	if !dryRun {
		// Single-quote paths so copying the suggested command cannot expand
		// shell substitutions in a caller-supplied filename.
		quoted := "'" + strings.ReplaceAll(change.Path, "'", "'\"'\"'") + "'"
		fmt.Fprintf(stdout, "On this runner, restart with: errand setup --config %s\n", quoted)
	}
	return 0
}

func writeAccessJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(stderr, "errand access: writing result: %v\n", err)
		return 1
	}
	return 0
}
