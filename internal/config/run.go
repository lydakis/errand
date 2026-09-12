package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/workspace"
)

// ErrNoPeerSelected distinguishes an unconfigured target from an invalid
// preference. ResolveRun still returns the other effective settings so doctor
// can inspect them without requiring every machine to have an outbound peer.
var ErrNoPeerSelected = errors.New("no peer selected")

// RunOverrides contains only explicit caller choices. Pointers distinguish
// an absent override from false or an empty (workspace-root) workdir.
type RunOverrides struct {
	Caches                          []proto.CacheBinding
	Artifacts                       []string
	Forwards                        []string
	Environment                     workspace.Environment
	Profile                         string
	Workspace                       *string
	Peer, URL, Where, WorkspaceRoot string
	Workdir                         *string
	ApplyOnSuccess                  *bool
	NoSnapshot                      bool
}

// EffectiveRun holds locally resolved preferences. PrepareExecution loads the
// environment and applies persistent-job rules for submission and inspection.
// URL is the configured endpoint, before the client installs its private SSH identity.
type EffectiveRun struct {
	environmentLayers []environmentLayer
	// Explicit CLI or selected-profile choices may override a persistent
	// workspace's creation defaults. Ambient configuration cannot rebind it.
	CachesOverride    bool                  `json:"-"`
	ArtifactsOverride bool                  `json:"-"`
	Caches            []proto.CacheBinding  `json:"caches"`
	CacheSources      map[string]string     `json:"cache_sources,omitempty"`
	Artifacts         []string              `json:"artifacts"`
	Forwards          []string              `json:"forward"`
	Environment       []EnvironmentVariable `json:"environment,omitempty"`
	Profile           string                `json:"profile,omitempty"`
	Workspace         string                `json:"workspace,omitempty"`
	Where             string                `json:"where,omitempty"`
	Candidates        []RunCandidate        `json:"-"`
	Peer              string                `json:"peer"`
	URL               string                `json:"url"`
	RemoteCommand     string                `json:"remote_command,omitempty"`
	RemoteSocket      string                `json:"remote_socket,omitempty"`
	Root              string                `json:"workspace_root"`
	Workdir           string                `json:"workdir"`
	Project           string                `json:"project"`
	ApplyOnSuccess    bool                  `json:"apply_on_success"`
	NoSnapshot        bool                  `json:"no_snapshot"`
	Sources           map[string]string     `json:"sources"`
}

// ResolveRun reads personal configuration once and uses only the workspace
// configuration accepted by boundary discovery. It does not contact runners,
// read environment files, register transports, or mutate client state. Cache
// groups inspect directory names locally when bindings are needed. Call
// PrepareExecution before using the job environment.
func ResolveRun(cwd string, cli RunOverrides) (EffectiveRun, error) {
	return resolveRun(cwd, cli, cachesForRun)
}

// ResolveWorkspaceCreation expands configured caches even when the profile
// names an existing workspace. Creation always takes a new set of bindings.
func ResolveWorkspaceCreation(cwd string, cli RunOverrides) (EffectiveRun, error) {
	return resolveRun(cwd, cli, cachesForCreation)
}

// ResolvePush leaves cache resolution to the workspace's frozen selection.
// Local package discovery must not prevent pushing package additions/removals.
func ResolvePush(cwd string, cli RunOverrides) (EffectiveRun, error) {
	return resolveRun(cwd, cli, cachesForPush)
}

type cacheResolution int

const (
	cachesForRun cacheResolution = iota
	cachesForCreation
	cachesForPush
)

func resolveRun(cwd string, cli RunOverrides, cacheMode cacheResolution) (EffectiveRun, error) {
	var result EffectiveRun
	if cli.Where != "" && (cli.Peer != "" || cli.URL != "") {
		return result, fmt.Errorf("--where cannot be combined with --on or --url")
	}
	if cli.Peer != "" && cli.URL != "" {
		return result, fmt.Errorf("--on and --url are mutually exclusive")
	}
	if cli.NoSnapshot && cli.WorkspaceRoot != "" {
		return result, fmt.Errorf("--workspace-root and --no-snapshot are mutually exclusive")
	}
	if cli.NoSnapshot && cli.Workdir != nil && *cli.Workdir != "" && *cli.Workdir != "." {
		return result, fmt.Errorf("--workdir must be the workspace root when using --no-snapshot")
	}
	personal, err := LoadClient()
	if err != nil {
		return result, err
	}
	personalPath, err := ClientPath()
	if err != nil {
		return result, err
	}
	explicitRoot := cli.WorkspaceRoot
	if cli.NoSnapshot {
		// An empty remote workspace uses only the caller directory's config.
		explicitRoot, err = filepath.Abs(cwd)
		if err != nil {
			return result, err
		}
	}
	selected, err := workspace.Discover(cwd, explicitRoot)
	if err != nil {
		return result, err
	}
	personalSource := "personal: " + personalPath
	workspaceSource := "workspace: " + filepath.Join(selected.Root, ".errand.toml")
	// A repository may provide literal defaults, but cannot select ambient
	// caller variables without a personal setting or explicit profile choice.
	if len(selected.Environment.Pass) != 0 {
		return result, fmt.Errorf("%s: nonempty workspace env.pass requires an explicit choice; move the names to personal config or an explicitly selected profile, or remove this default and use --passenv", workspaceSource)
	}
	if len(selected.Environment.Files) != 0 {
		return result, fmt.Errorf("%s: nonempty workspace env.files requires an explicit choice; move paths to personal config or an explicitly selected profile, or use --env-file", workspaceSource)
	}
	profile, profileSource, err := selectProfile(personal, selected, cli.Profile, personalSource, workspaceSource)
	if err != nil {
		return result, err
	}
	result = EffectiveRun{
		CachesOverride:    cli.Caches != nil || profile.Caches != nil,
		ArtifactsOverride: cli.Artifacts != nil || profile.Artifacts.Paths != nil,
		Profile:           cli.Profile,
		Root:              selected.Root, Workdir: selected.Workdir, Project: selected.Project,
		NoSnapshot: cli.NoSnapshot,
		Sources: map[string]string{
			"workspace_root":   selected.Source,
			"workdir":          "current directory relative to workspace root",
			"project":          "derived from workspace root and current directory",
			"apply_on_success": "default: false",
			"no_snapshot":      "default: false",
		},
	}
	if cli.Profile != "" {
		result.Sources["profile"] = profileSource
	}
	if profile.Run.Workspace != nil {
		result.Workspace = *profile.Run.Workspace
		result.Sources["workspace"] = profileSource + " run.workspace"
	}
	if cli.Workspace != nil {
		if err := proto.ValidateWorkspaceName(*cli.Workspace); err != nil {
			return result, fmt.Errorf("--workspace: %w", err)
		}
		result.Workspace = *cli.Workspace
		result.Sources["workspace"] = "cli: --workspace"
	}
	session, err := resolveSession(personal.Session, selected.Session, profile.Session, cli.Forwards, personalSource, workspaceSource, profileSource)
	if err != nil {
		return result, err
	}
	result.Forwards, result.Sources["forward"] = session.Forwards, session.Source
	result.Artifacts, result.Sources["artifacts"], err = resolveArtifacts(personal.Artifacts.Paths, selected.Artifacts.Paths, profile.Artifacts.Paths, cli.Artifacts, personalSource, workspaceSource, profileSource)
	if err != nil {
		return result, err
	}
	if cacheMode == cachesForPush || (cacheMode == cachesForRun && result.Workspace != "" && !result.CachesOverride) {
		result.Sources["caches"] = workspaceDefaultsSource(result.Workspace)
	} else {
		result.Caches, result.CacheSources, result.Sources["caches"], err = resolveCaches(selected.Root, personal.Caches, selected.Caches, profile.Caches, cli.Caches, personalSource, workspaceSource, profileSource)
		if err != nil {
			return result, err
		}
	}
	profileDir := filepath.Dir(personalPath)
	if _, ok := selected.Profiles[cli.Profile]; ok {
		profileDir = selected.Root
	}
	result.environmentLayers = []environmentLayer{
		environmentLayer{personal.Environment, personalSource + " env", filepath.Dir(personalPath)},
		environmentLayer{selected.Environment, workspaceSource + " env", selected.Root},
		environmentLayer{profile.Environment, profileSource + " env", profileDir},
		environmentLayer{cli.Environment, "cli: --env/--passenv/--env-file", cwd},
	}
	if cli.NoSnapshot {
		result.Sources["workspace_root"] = "current directory (--no-snapshot)"
		result.Sources["no_snapshot"] = "cli: --no-snapshot"
		// Preserve the empty-workspace submission's existing project semantics.
		result.Project = ""
		result.Sources["project"] = "empty workspace (--no-snapshot)"
	}
	if profile.Run.Workdir != nil {
		result.Workdir = *profile.Run.Workdir
		result.Sources["workdir"] = profileSource + " run.workdir"
	}
	if cli.Workdir != nil {
		result.Workdir = *cli.Workdir
		result.Sources["workdir"] = "cli: --workdir"
	}
	if cli.NoSnapshot && result.Workdir != "" && result.Workdir != "." {
		return result, fmt.Errorf("workdir from %s must be the workspace root when using --no-snapshot", result.Sources["workdir"])
	}
	if personal.ApplyOnSuccess != nil {
		result.ApplyOnSuccess = *personal.ApplyOnSuccess
		result.Sources["apply_on_success"] = personalSource + " (apply_on_success)"
	}
	if selected.ApplyOnSuccess != nil {
		result.ApplyOnSuccess = *selected.ApplyOnSuccess
		result.Sources["apply_on_success"] = workspaceSource + " (changes.apply_on_success)"
	}
	if profile.Changes.ApplyOnSuccess != nil {
		result.ApplyOnSuccess = *profile.Changes.ApplyOnSuccess
		result.Sources["apply_on_success"] = profileSource + " changes.apply_on_success"
	}
	if cli.ApplyOnSuccess != nil {
		result.ApplyOnSuccess = *cli.ApplyOnSuccess
		result.Sources["apply_on_success"] = "cli: --apply/--no-apply"
	}
	if err := resolvePlacement(&result, personal, selected, profile, cli, personalSource, workspaceSource, profileSource); err != nil {
		return result, err
	}
	if result.Where != "" {
		return result, nil
	}
	if cli.URL != "" {
		result.URL = strings.TrimSuffix(cli.URL, "/")
		if result.URL == "" {
			return result, fmt.Errorf("--url must not be empty")
		}
		result.Peer = result.URL
		result.Sources["peer"] = "cli: --url"
		result.Sources["url"] = "cli: --url"
		return result, nil
	}
	if result.Peer == "" {
		if selected.Peer == nil && profile.Run.Peer == nil {
			return result, fmt.Errorf("%w by %s; set --on or configure a peer", ErrNoPeerSelected, result.Sources["peer"])
		}
		return result, fmt.Errorf("no peer selected by %s; set --on or configure a peer", result.Sources["peer"])
	}
	if result.Peer == "local" && cli.Peer == "" && profile.Run.Peer == nil && selected.Peer != nil && personal.DefaultPeer != "local" {
		if _, explicit := personal.Peers["local"]; !explicit {
			return result, fmt.Errorf("workspace run.peer = local requires personal configuration, an explicitly selected profile, or --on local")
		}
	}
	result.URL, err = personal.PeerURL(result.Peer)
	if err != nil {
		return result, fmt.Errorf("%s: %w", result.Sources["peer"], err)
	}
	result.RemoteCommand = personal.SSHRemoteCommand(result.Peer)
	result.RemoteSocket = personal.SSHRemoteSocket(result.Peer)
	result.Sources["url"] = personalSource + " (peers." + result.Peer + ")"
	if result.Peer == "local" && personal.Peers["local"].Socket == "" {
		path, _ := DaemonPath()
		result.Sources["url"] = "local runner configuration: " + path
	}
	if result.RemoteCommand != "" {
		result.Sources["remote_command"] = result.Sources["url"]
	}
	if result.RemoteSocket != "" {
		result.Sources["remote_socket"] = result.Sources["url"]
	}
	return result, nil
}
