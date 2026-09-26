package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/termui"
)

const accessActivation = "Saved configuration only; restart the runner to activate it. deny_users overrides allow_users and capability grants for tailnet requests. Removing a denial restores any remaining grants. SSH access is separate."

func cmdAccess(args []string) int { return cmdAccessTo(args, os.Stdout, os.Stderr) }

func cmdAccessTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e, o := con.Err, con.Out
	action := "list"
	if len(args) != 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	actions := []string{"list", "add", "remove", "deny", "undeny"}
	if !slices.Contains(actions, action) {
		e.Errorf("unknown access command '%s'", action)
		if guess := termui.Suggest(action, actions); guess != "" {
			e.Hintf("did you mean errand access %s?", guess)
		} else {
			e.Hintf("use list, add, remove, deny or undeny")
		}
		return 2
	}
	fs := flag.NewFlagSet("errand access "+action, flag.ContinueOnError)
	path := fs.String("config", "", "local runner config (default ~/.config/errand/errandd.toml)")
	asJSON := fs.Bool("json", false, "print saved policy or edit result as JSON")
	dryRun := false
	if action != "list" {
		fs.BoolVar(&dryRun, "dry-run", false, "preview the selected access list without writing or restarting")
		fs.BoolVar(&dryRun, "n", false, "preview the selected access list without writing or restarting")
	}
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "access", stdout, e); !ok {
		return code
	}
	wantArgs := 0
	if action != "list" {
		wantArgs = 1
	}
	if fs.NArg() != wantArgs {
		if action == "list" {
			return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
		}
		e.Errorf("errand access %s needs a tailnet login", action)
		e.Hintf("for example errand access %s alice@example.com", action)
		return 2
	}
	notRunner := func(err error) int {
		if strings.Contains(err.Error(), "is missing") {
			e.Errorf("this machine isn't an errand runner (no %s)", homeRelative(cmpOr(*path, "~/.config/errand/errandd.toml")))
			e.Hintf("run this on the runner you want to manage, or errand setup to make this machine one")
			return 1
		}
		return failWith(e, 1, err, errorScope{})
	}
	if action == "list" {
		policy, err := config.ReadAccess(*path)
		if err != nil {
			return notRunner(err)
		}
		if *asJSON {
			return writeAccessJSON(stdout, stderr, struct {
				config.AccessPolicy
				Activation string `json:"activation"`
			}{policy, accessActivation})
		}
		field := func(label, value string) { o.Print(o.D(padRight(label, 8)) + " " + value) }
		listen := ""
		if policy.Listen != "" {
			listen = " " + o.D("· listening on "+terminalSafeField(policy.Listen))
		}
		field("Runner", homeRelative(terminalSafeField(policy.Path))+listen)
		allowed := o.D("nobody")
		if len(policy.AllowUsers) != 0 {
			allowed = o.B(terminalSafeField(strings.Join(policy.AllowUsers, ", ")))
		}
		field("Allowed", allowed)
		denied := "nobody"
		if len(policy.DenyUsers) != 0 {
			denied = o.Paint(terminalSafeField(strings.Join(policy.DenyUsers, ", ")), termui.Red)
		}
		field("Denied", denied)
		if policy.Capability != "" {
			field("Grants", "tailnet capability "+terminalSafeField(policy.Capability)+" also admits people")
		}
		field("SSH", o.D("separate: anyone who can SSH in as this user"))
		if output.verbose {
			o.Print("")
			for _, l := range wrapPlain(accessActivation, 90) {
				o.Print(o.D(l))
			}
		}
		return 0
	}
	login := fs.Arg(0)
	edit := config.ChangeAccess
	if action == "deny" || action == "undeny" {
		edit = config.ChangeDeniedAccess
	}
	change, err := edit(*path, login, action == "add" || action == "deny", dryRun)
	if err != nil {
		return notRunner(err)
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
	past := map[string]string{"add": "Allowed", "remove": "Removed", "deny": "Denied", "undeny": "Stopped denying"}[action]
	where := homeRelative(terminalSafeField(change.Path))
	switch {
	case !change.Changed:
		already := map[string]string{"add": "already allowed", "remove": "wasn't allowed", "deny": "already denied", "undeny": "wasn't denied"}[action]
		o.Say(termui.OK, o.B(terminalSafeField(login))+" "+already+" "+o.D("· no change to "+where))
		return 0
	case dryRun:
		o.Print(o.Paint("Dry run", termui.Yellow) + " " + o.D("· nothing changes"))
		o.Print("Would update " + change.Field + " in " + where + ":")
	default:
		o.Say(termui.OK, past+" "+o.B(terminalSafeField(login))+" "+o.D("· saved to "+where+" (comments are dropped on save)"))
	}
	if output.verbose || dryRun {
		o.Print("  " + o.D(padRight("before", 7)) + " " + fmt.Sprintf("%q", change.Before))
		o.Print("  " + o.D(padRight("after", 7)) + " " + fmt.Sprintf("%q", change.After))
	}
	if output.verbose {
		for _, l := range wrapPlain(accessActivation, 90) {
			o.Print(o.D(l))
		}
	}
	if !dryRun {
		o.Warnf("Not active until the runner restarts")
		command := "errand setup"
		if *path != "" {
			command = "errand setup --config '" + strings.ReplaceAll(change.Path, "'", "'\"'\"'") + "'"
		}
		o.Next(command, "")
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
