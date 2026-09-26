package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/pathpolicy"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
	"github.com/lydakis/errand/internal/workspace"
)

// Both submission and inspection use these flags and the same resolver.
type runConfigFlags struct {
	caches                        stringList
	noCaches                      bool
	artifacts                     stringList
	noArtifacts                   bool
	session                       sessionFlags
	envs, passenvs                stringList
	envFiles                      stringList
	noEnvFiles                    bool
	profile                       string
	workspace                     string
	on, url, where, workdir, root string
	apply, noApply, noSnapshot    bool
}

func (f *runConfigFlags) bind(fs *flag.FlagSet) {
	f.session.bind(fs)
	fs.Var(&f.caches, "cache", "reuse a runner cache NAME=PATH at a workspace-relative directory (repeatable; replaces configured list)")
	fs.BoolVar(&f.noCaches, "no-caches", false, "disable configured named caches")
	fs.Var(&f.artifacts, "artifact", "retain an exact workspace-relative file or directory, including ignored outputs (repeatable; replaces configured list)")
	fs.BoolVar(&f.noArtifacts, "no-artifacts", false, "disable configured artifact declarations")
	fs.Var(&f.envs, "env", "set NAME=VALUE in the job environment (repeatable; values hidden in diagnostics)")
	fs.Var(&f.envs, "e", "set NAME=VALUE in the job environment (repeatable)")
	fs.Var(&f.passenvs, "passenv", "require and forward a local environment variable (repeatable; replaces configured pass list)")
	fs.Var(&f.envFiles, "env-file", "load a local environment file (repeatable; replaces configured file list)")
	fs.BoolVar(&f.noEnvFiles, "no-env-files", false, "clear configured environment files")
	fs.StringVar(&f.profile, "profile", "", "named run preferences from workspace or personal configuration")
	fs.StringVar(&f.workspace, "workspace", "", "use an existing persistent workspace; overrides the selected profile and never uploads local edits")
	fs.StringVar(&f.where, "where", "", "select a configured runner matching facts, e.g. os=linux,go or *")
	fs.StringVar(&f.on, "on", "", "peer name from personal configuration, or local")
	fs.StringVar(&f.url, "url", "", "peer base URL (mutually exclusive with --on and --where)")
	fs.StringVar(&f.workdir, "workdir", "", "working directory, relative to the workspace root")
	fs.StringVar(&f.workdir, "w", "", "working directory, relative to the workspace root")
	fs.StringVar(&f.root, "workspace-root", "", "snapshot root containing the current directory")
	fs.BoolVar(&f.apply, "apply", false, "apply retained workspace changes after successful completion")
	fs.BoolVar(&f.noApply, "no-apply", false, "do not apply retained workspace changes after the run")
	fs.BoolVar(&f.noSnapshot, "no-snapshot", false, "run in an empty job workspace")
}

func (f runConfigFlags) overrides(fs *flag.FlagSet) (config.RunOverrides, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	result := config.RunOverrides{Peer: f.on, URL: f.url, Where: f.where, WorkspaceRoot: f.root, NoSnapshot: f.noSnapshot}
	if set["artifact"] && set["no-artifacts"] {
		return result, fmt.Errorf("--artifact and --no-artifacts are mutually exclusive")
	}
	if set["cache"] && set["no-caches"] {
		return result, fmt.Errorf("--cache and --no-caches are mutually exclusive")
	}
	if set["cache"] || f.noCaches {
		result.Caches = []proto.CacheBinding{}
	}
	for _, binding := range f.caches {
		name, path, ok := strings.Cut(binding, "=")
		if !ok {
			return result, fmt.Errorf("--cache requires NAME=PATH")
		}
		result.Caches = append(result.Caches, proto.CacheBinding{Name: name, Path: path})
	}
	if err := pathpolicy.ValidateCaches(result.Caches); err != nil {
		return result, err
	}
	result.Artifacts = f.artifacts
	if f.noArtifacts {
		result.Artifacts = []string{}
	}
	if err := pathpolicy.ValidateArtifacts(result.Artifacts); err != nil {
		return result, err
	}
	var err error
	result.Forwards, err = f.session.overrides(fs)
	if err != nil {
		return result, err
	}
	result.Environment.Pass = f.passenvs
	if set["env-file"] && set["no-env-files"] {
		return result, fmt.Errorf("--env-file and --no-env-files are mutually exclusive")
	}
	result.Environment.Files = f.envFiles
	if f.noEnvFiles {
		result.Environment.Files = []string{}
	}
	for _, path := range result.Environment.Files {
		if path == "" || strings.ContainsRune(path, 0) {
			return result, fmt.Errorf("--env-file requires a nonempty path without NUL")
		}
	}
	for _, name := range f.passenvs {
		if err := workspace.ValidateEnvironmentName(name); err != nil {
			return result, err
		}
	}
	result.Environment.Set = map[string]string{}
	for _, assignment := range f.envs {
		name, value, ok := strings.Cut(assignment, "=")
		if !ok {
			return result, fmt.Errorf("--env requires NAME=VALUE")
		}
		if err := workspace.ValidateEnvironmentName(name); err != nil {
			return result, err
		}
		if strings.ContainsRune(value, 0) {
			return result, fmt.Errorf("--env value for %q contains NUL", name)
		}
		result.Environment.Set[name] = value
	}
	result.Profile = f.profile
	if set["workspace"] {
		if err := proto.ValidateWorkspaceName(f.workspace); err != nil {
			return result, fmt.Errorf("--workspace: %w", err)
		}
		result.Workspace = &f.workspace
	}
	if set["profile"] && f.profile == "" {
		return result, fmt.Errorf("--profile requires a non-empty name")
	}
	if set["where"] && f.where == "" {
		return result, fmt.Errorf("--where requires a non-empty selector")
	}
	if f.where != "" && (f.on != "" || f.url != "") {
		return result, fmt.Errorf("--where cannot be combined with --on or --url")
	}
	if set["on"] && f.on == "" || set["url"] && f.url == "" {
		return result, fmt.Errorf("--on and --url require non-empty values")
	}
	if f.on != "" && f.url != "" {
		return result, fmt.Errorf("--on and --url are mutually exclusive")
	}
	if set["apply"] && set["no-apply"] {
		return result, fmt.Errorf("--apply and --no-apply are mutually exclusive")
	}
	if set["apply"] {
		result.ApplyOnSuccess = &f.apply
	}
	if set["no-apply"] {
		value := !f.noApply
		result.ApplyOnSuccess = &value
	}
	if set["workdir"] || set["w"] {
		result.Workdir = &f.workdir
	}
	if f.noSnapshot && f.root != "" {
		return result, fmt.Errorf("--workspace-root and --no-snapshot are mutually exclusive")
	}
	if f.noSnapshot && f.workdir != "" && f.workdir != "." {
		return result, fmt.Errorf("--workdir must be the workspace root when using --no-snapshot")
	}
	return result, nil
}

func cmdConfig(args []string) int { return cmdConfigTo(args, os.Stdout, os.Stderr) }

// Describe the local source directory without implying it is the runner's path.
func displaySourceWorkdir(effective config.EffectiveRun) string {
	if effective.NoSnapshot {
		return "empty workspace root"
	}
	return filepath.Join(effective.Root, effective.Workdir)
}

func cmdConfigTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand config", flag.ContinueOnError)
	var flags runConfigFlags
	flags.bind(fs)
	asJSON := fs.Bool("json", false, "print effective run configuration and sources as JSON")
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "config", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	overrides, err := flags.overrides(fs)
	if err != nil {
		return usageError(e, "%v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failWith(e, client.ExitTransaction, err, errorScope{})
	}
	effective, err := config.ResolveRun(cwd, overrides)
	if err != nil {
		return failWith(e, runConfigErrorCode(err), err, errorScope{peer: flags.on})
	}
	if err := effective.PrepareExecution(false); err != nil {
		return usageError(e, "%v", err)
	}
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(effective); err != nil {
			e.Errorf("writing config: %v", err)
			return client.ExitTransaction
		}
		return 0
	}
	writeConfig(con.Out, effective, output.verbose)
	return 0
}

// configRow is one setting as shown by errand config.
type configRow struct {
	label, value, source string
	isDefault, fromFlag  bool
}

// writeConfig groups settings by what they control, dims defaults, and
// highlights what this invocation's flags changed.
func writeConfig(s *termui.Stream, effective config.EffectiveRun, verbose bool) {
	src := func(key string) (string, bool, bool) {
		raw, ok := effective.Sources[key]
		if !ok {
			return "", false, false
		}
		return configSource(raw, verbose), strings.HasPrefix(raw, "default"), strings.HasPrefix(raw, "cli: ")
	}
	var rows []configRow
	add := func(label, key, value string) {
		source, isDefault, fromFlag := src(key)
		if _, ok := effective.Sources[key]; !ok {
			return
		}
		rows = append(rows, configRow{label, value, source, isDefault, fromFlag})
	}
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	listOr := func(values []string) string {
		if len(values) == 0 {
			return "none"
		}
		return strings.Join(values, ", ")
	}
	add("Profile", "profile", effective.Profile)
	add("Persistent", "workspace", effective.Workspace)
	add("Where", "where", effective.Where)
	add("Runner", "peer", effective.Peer)
	add("", "url", effective.URL)
	add("", "remote_command", effective.RemoteCommand)
	add("", "remote_socket", effective.RemoteSocket)
	add("Workspace", "workspace_root", homeRelative(effective.Root))
	workdir := workdirLabel(effective.Workdir)
	if effective.NoSnapshot {
		workdir = "empty workspace"
	}
	add("Workdir", "workdir", workdir)
	add("Project", "project", effective.Project)
	add("Apply", "apply_on_success", onOff(effective.ApplyOnSuccess))
	add("Snapshot", "no_snapshot", onOff(!effective.NoSnapshot))
	add("Forwards", "forward", listOr(effective.Forwards))
	artifacts := listOr(effective.Artifacts)
	caches := "none"
	if len(effective.Caches) != 0 {
		caches = termui.Things(len(effective.Caches), "binding", "bindings")
	}
	if effective.Workspace != "" {
		if !effective.ArtifactsOverride {
			artifacts = "from the workspace (resolved on the runner)"
		}
		if !effective.CachesOverride {
			caches = "from the workspace (resolved on the runner)"
		}
	}
	add("Artifacts", "artifacts", artifacts)
	add("Caches", "caches", caches)
	cacheRows := slices.Clone(effective.Caches)
	slices.SortFunc(cacheRows, func(a, b proto.CacheBinding) int {
		if order := cmp.Compare(effective.CacheSources[a.Name], effective.CacheSources[b.Name]); order != 0 {
			return order
		}
		return cmp.Compare(a.Path, b.Path)
	})
	for _, cache := range cacheRows {
		source := effective.CacheSources[cache.Name]
		if source == "" {
			source = effective.Sources["caches"]
		}
		rows = append(rows, configRow{label: "", value: cache.Name + " → " + cache.Path, source: configSource(source, verbose), fromFlag: strings.HasPrefix(source, "cli: ")})
	}
	for _, entry := range effective.Environment {
		state := "set here (value hidden)"
		switch {
		case entry.Kind == "passenv" && entry.Available:
			state = "from your shell"
		case entry.Kind == "passenv":
			state = "missing from your shell"
		case entry.Kind == "file":
			state = "from an env file (value hidden)"
		}
		rows = append(rows, configRow{label: "Env " + entry.Name, value: state, source: configSource(entry.Source, verbose), fromFlag: strings.HasPrefix(entry.Source, "cli: ")})
	}
	// Long values (deep paths) don't push every source to the far right.
	const maxValueW = 40
	labelW, valueW := 0, 0
	for _, r := range rows {
		labelW = max(labelW, termui.CellWidth(r.label))
		if w := termui.CellWidth(terminalSafeField(r.value)); w <= maxValueW {
			valueW = max(valueW, w)
		}
	}
	for _, r := range rows {
		value := terminalSafeField(r.value)
		label := padRight(r.label, labelW)
		pad := strings.Repeat(" ", max(3, valueW-termui.CellWidth(value)+3))
		var line string
		switch {
		case r.fromFlag:
			line = label + "   " + s.Paint(value, termui.Bold, termui.Yellow) + pad + s.Paint(r.source, termui.Yellow)
		case r.isDefault:
			line = s.D(label + "   " + value + pad + r.source)
		case r.label == "":
			line = label + "   " + s.D(value) + pad + s.D(r.source)
		case r.label == "Runner":
			line = label + "   " + s.B(value) + pad + s.D(r.source)
		default:
			line = label + "   " + value + pad + s.D(r.source)
		}
		if strings.HasPrefix(r.value, "missing") {
			line = label + "   " + s.Paint(value, termui.Red) + pad + s.D(r.source)
		}
		s.Print(strings.TrimRight(line, " "))
	}
}

// configSource shortens where a setting came from.
func configSource(raw string, verbose bool) string {
	switch {
	case raw == "":
		return ""
	case strings.HasPrefix(raw, "cli: "):
		flagText := strings.TrimPrefix(raw, "cli: ")
		if before, _, ok := strings.Cut(flagText, "/"); ok && !verbose {
			return before
		}
		return flagText
	case strings.HasPrefix(raw, "default"):
		if verbose {
			return raw
		}
		return "default"
	case raw == "current directory relative to workspace root":
		return "current directory"
	case raw == "derived from workspace root and current directory":
		return "workspace folder name"
	}
	for _, prefix := range []string{"personal: ", "workspace: ", "profile "} {
		if !strings.HasPrefix(raw, prefix) {
			continue
		}
		rest := strings.TrimPrefix(raw, prefix)
		path, key, hasKey := strings.Cut(rest, " (")
		key = strings.TrimSuffix(key, ")")
		if !hasKey {
			if p, k, ok := strings.Cut(rest, " "); ok {
				path, key, hasKey = p, k, true
			}
		}
		path = homeRelative(path)
		switch {
		case !hasKey:
			return path
		case verbose || key == "default_peer":
			return key + " · " + path
		default:
			return key
		}
	}
	return raw
}
