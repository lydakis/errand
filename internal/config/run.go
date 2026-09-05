package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/workspace"
)

// RunOverrides contains only explicit caller choices. Pointers distinguish
// an absent override from false or an empty (workspace-root) workdir.
type RunOverrides struct {
	Environment              workspace.Environment
	Profile                  string
	Peer, URL, WorkspaceRoot string
	Workdir                  *string
	ApplyOnSuccess           *bool
	NoSnapshot               bool
}

// EffectiveRun is shared by submission and config inspection. URL is the
// configured endpoint, before the client installs its private SSH identity.
type EffectiveRun struct {
	Environment    []EnvironmentVariable `json:"environment,omitempty"`
	Profile        string                `json:"profile,omitempty"`
	Peer           string                `json:"peer"`
	URL            string                `json:"url"`
	RemoteCommand  string                `json:"remote_command,omitempty"`
	RemoteSocket   string                `json:"remote_socket,omitempty"`
	Root           string                `json:"workspace_root"`
	Workdir        string                `json:"workdir"`
	Project        string                `json:"project"`
	ApplyOnSuccess bool                  `json:"apply_on_success"`
	NoSnapshot     bool                  `json:"no_snapshot"`
	Sources        map[string]string     `json:"sources"`
}

// ResolveRun reads personal configuration once and uses only the workspace
// configuration accepted by boundary discovery. It does not contact runners,
// inspect snapshot contents, register transports, or mutate client state.
func ResolveRun(cwd string, cli RunOverrides) (EffectiveRun, error) {
	var result EffectiveRun
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
	var profile workspace.Profile
	profileSource := ""
	if cli.Profile != "" {
		var found bool
		profile, found = personal.Profiles[cli.Profile]
		profileSource = personalSource
		if local, exists := selected.Profiles[cli.Profile]; exists {
			profile, found = local, true
			profileSource = workspaceSource
		}
		if !found {
			return result, fmt.Errorf("profile %q is not defined in %s or the selected workspace %s", cli.Profile, personalPath, selected.Root)
		}
		profileSource += " (profiles." + cli.Profile + ")"
	}
	result = EffectiveRun{
		Profile: cli.Profile,
		Root:    selected.Root, Workdir: selected.Workdir, Project: selected.Project,
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
	result.Environment = resolveEnvironment(
		environmentLayer{personal.Environment, personalSource + " env"},
		environmentLayer{selected.Environment, workspaceSource + " env"},
		environmentLayer{profile.Environment, profileSource + " env"},
		environmentLayer{cli.Environment, "cli: --env/--passenv"},
	)
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
	result.Peer = personal.DefaultPeer
	result.Sources["peer"] = personalSource + " (default_peer)"
	if selected.Peer != nil {
		result.Peer = *selected.Peer
		result.Sources["peer"] = workspaceSource + " (run.peer)"
	}
	if profile.Run.Peer != nil {
		result.Peer = *profile.Run.Peer
		result.Sources["peer"] = profileSource + " run.peer"
	}
	if cli.Peer != "" {
		result.Peer = cli.Peer
		result.Sources["peer"] = "cli: --on"
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
		return result, fmt.Errorf("no peer selected by %s; set --on or configure a peer", result.Sources["peer"])
	}
	result.URL, err = personal.PeerURL(result.Peer)
	if err != nil {
		return result, fmt.Errorf("%s: %w", result.Sources["peer"], err)
	}
	result.RemoteCommand = personal.SSHRemoteCommand(result.Peer)
	result.RemoteSocket = personal.SSHRemoteSocket(result.Peer)
	result.Sources["url"] = personalSource + " (peers." + result.Peer + ")"
	if result.RemoteCommand != "" {
		result.Sources["remote_command"] = result.Sources["url"]
	}
	if result.RemoteSocket != "" {
		result.Sources["remote_socket"] = result.Sources["url"]
	}
	return result, nil
}
