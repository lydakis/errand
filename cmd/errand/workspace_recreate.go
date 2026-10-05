package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

// cmdWorkspaceRecreate previews by default. Removal deletes runner-only files
// for good, so the preview names them and the ways to keep them, and only
// --yes removes anything. Checks that would refuse run before either.
func cmdWorkspaceRecreate(args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("errand workspaces recreate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	on := fs.String("on", "", "peer name, or local")
	rawURL := fs.String("url", "", "peer base URL")
	yes := fs.Bool("yes", false, "remove the workspace and create it again; without it, only show what that deletes")
	includeAll := fs.Bool("include-all", false, "allow an otherwise refused broad snapshot (never a filesystem root)")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	synopsis := "errand workspaces recreate [options] NAME"
	setFlagUsage(fs, synopsis)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: "+synopsis)
		return 2
	}
	url, label, err := resolvePeerTarget(*rawURL, *on)
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return 2
	}
	peer := "--url " + shellArg(url)
	if *rawURL == "" {
		peer = "--on " + label
	}
	r, err := client.PrepareWorkspaceRecreation(url, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return 1
	}
	if r.Root == "" {
		// No transfer state here: like create, use the checkout around the
		// current directory.
		cwd, err := os.Getwd()
		if err == nil {
			var effective config.EffectiveRun
			effective, err = config.ResolveWorkspaceCreation(cwd, config.RunOverrides{Peer: *on, URL: *rawURL})
			r.Root = effective.Root
		}
		if err != nil {
			fmt.Fprintln(stderr, "errand:", err)
			return 1
		}
	}
	printRecreationPreview(stderr, r, peer, cmpOr(label, url))
	if !*yes {
		fmt.Fprintf(stderr, "\nNothing was removed. To recreate it, run:\n  errand workspaces recreate %s --yes %s\n", peer, r.Workspace.Name)
		if *jsonOutput {
			jobs := make([]string, len(r.Jobs))
			for i, job := range r.Jobs {
				jobs[i] = job.ID
			}
			if err := json.NewEncoder(out).Encode(recreationPreview{r.Workspace.ID, r.Workspace.Name, label, url, r.Root, r.WorkingBytes, jobs, r.EarlierState, false}); err != nil {
				fmt.Fprintln(stderr, "errand:", err)
				return 1
			}
		}
		return 0
	}
	w, err := r.Recreate(client.RunOptions{IncludeAll: *includeAll, Stderr: stderr})
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		var incomplete *client.RecreateIncompleteError
		if errors.As(err, &incomplete) {
			fmt.Fprintf(stderr, "errand: create it from %s with: %s\n", shellArg(r.Root), recreationCreateCommand(r, peer))
		}
		return 1
	}
	if *jsonOutput {
		err = json.NewEncoder(out).Encode(struct {
			proto.Workspace
			Peer string `json:"peer"`
			URL  string `json:"url"`
		}{w, label, url})
	} else {
		_, err = fmt.Fprintf(out, "%s\n", w.Name)
		fmt.Fprintf(stderr, "errand: recreated workspace %s on %s\n", w.Name, cmpOr(label, url))
	}
	if err != nil {
		fmt.Fprintln(stderr, "errand:", err)
		return 1
	}
	return 0
}

type recreationPreview struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Peer         string   `json:"peer"`
	URL          string   `json:"url"`
	Root         string   `json:"root"`
	WorkingBytes int64    `json:"working_bytes"` // -1 when the runner could not measure it
	Jobs         []string `json:"retained_jobs"`
	EarlierState bool     `json:"earlier_state"`
	Removed      bool     `json:"removed"`
}

func printRecreationPreview(w io.Writer, r client.WorkspaceRecreation, peer, target string) {
	name := r.Workspace.Name
	source := "from " + r.Root
	if r.NoSnapshot {
		source = "empty"
	}
	fmt.Fprintf(w, "Recreating workspace %s on %s removes it and creates it again %s.\n\n", terminalSafeField(name), terminalSafeField(target), terminalSafeField(source))
	size := ""
	if r.WorkingBytes >= 0 {
		size = " (" + formatByteSize(r.WorkingBytes) + ")"
	}
	fmt.Fprintf(w, "Deleted on the runner: its working tree%s, including files jobs left there that are not in your checkout, such as installed dependencies and build outputs. Named caches are kept.\n", size)
	var kept []string
	if len(r.Workspace.Selection.Artifacts) > 0 {
		kept = append(kept, "artifacts "+strings.Join(r.Workspace.Selection.Artifacts, ", "))
	}
	if len(r.Workspace.Selection.Caches) > 0 {
		caches := make([]string, len(r.Workspace.Selection.Caches))
		for i, c := range r.Workspace.Selection.Caches {
			caches[i] = c.Name
		}
		kept = append(kept, "caches "+strings.Join(caches, ", "))
	}
	if len(kept) > 0 {
		fmt.Fprintf(w, "The new workspace keeps the same %s.\n", terminalSafeField(strings.Join(kept, " and ")))
	}
	if n := len(r.Jobs); n > 0 {
		jobs, them := fmt.Sprintf("Its %d earlier jobs keep their retained results until they expire", n), "them"
		if n == 1 {
			jobs, them = "Its earlier job keeps its retained result until it expires", "it"
		}
		if r.EarlierState {
			fmt.Fprintf(w, "%s, but this version cannot apply %s with fetch --apply. Export with:\n  errand fetch --output DIR %s/%s\n", jobs, them, terminalSafeField(target), r.Jobs[0].ID)
		} else {
			fmt.Fprintf(w, "%s; fetch --apply and fetch --output still work.\n", jobs)
		}
	}
	fmt.Fprintf(w, "\nTo keep what jobs changed in the tree, capture it before recreating:\n"+
		"  errand %s --workspace %s --no-apply -- true\n"+
		"  errand fetch --output DIR HANDLE   (the handle that command prints)\n"+
		"Ignored files that are not artifacts are not captured; the next job rebuilds them.\n", peer, name)
}

func recreationCreateCommand(r client.WorkspaceRecreation, peer string) string {
	args := []string{"errand", "workspaces", "create", peer}
	if r.NoSnapshot {
		args = append(args, "--no-snapshot")
	}
	for _, a := range r.Workspace.Selection.Artifacts {
		args = append(args, "--artifact", shellArg(a))
	}
	for _, c := range r.Workspace.Selection.Caches {
		args = append(args, "--cache", shellArg(c.Name+"="+c.Path))
	}
	return strings.Join(append(args, r.Workspace.Name), " ")
}

// shellArg quotes a value for a printed command only when the shell needs it.
func shellArg(value string) string {
	if value != "" && strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@,+") == "" {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
