package config

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/lydakis/errand/internal/workspace"
)

type EffectiveSession struct {
	Forwards []string `json:"forward"`
	Source   string   `json:"source"`
}

// ResolveSession resolves attachment preferences in the current workspace.
// It never resolves a run target, environment, workdir, or apply policy.
func ResolveSession(cwd, profileName string, forwards []string) (EffectiveSession, error) {
	personal, err := LoadClient()
	if err != nil {
		return EffectiveSession{}, err
	}
	path, err := ClientPath()
	if err != nil {
		return EffectiveSession{}, err
	}
	selected, err := workspace.Discover(cwd, "")
	if err != nil {
		return EffectiveSession{}, err
	}
	personalSource := "personal: " + path
	workspaceSource := "workspace: " + filepath.Join(selected.Root, ".errand.toml")
	profile, profileSource, err := selectProfile(personal, selected, profileName, personalSource, workspaceSource)
	if err != nil {
		return EffectiveSession{}, err
	}
	return resolveSession(personal.Session, selected.Session, profile.Session, forwards, personalSource, workspaceSource, profileSource)
}

func selectProfile(personal Client, selected workspace.Selection, name, personalSource, workspaceSource string) (workspace.Profile, string, error) {
	if name == "" {
		return workspace.Profile{}, "", nil
	}
	profile, found := personal.Profiles[name]
	source := personalSource
	if local, exists := selected.Profiles[name]; exists {
		profile, found, source = local, true, workspaceSource
	}
	if !found {
		return workspace.Profile{}, "", fmt.Errorf("profile %q is not defined in %s or the selected workspace %s", name, personalSource, selected.Root)
	}
	return profile, source + " (profiles." + name + ")", nil
}

func resolveSession(personal, selected, profile workspace.Session, cli []string, personalSource, workspaceSource, profileSource string) (EffectiveSession, error) {
	result := EffectiveSession{Forwards: []string{}, Source: "default: no forwards"}
	for _, layer := range []struct {
		values []string
		source string
	}{
		{personal.Forward, personalSource + " session.forward"},
		{selected.Forward, workspaceSource + " session.forward"},
		{profile.Forward, profileSource + " session.forward"},
		{cli, "cli: --forward/--no-forward"},
	} {
		if layer.values != nil {
			result.Forwards, result.Source = slices.Clone(layer.values), layer.source
		}
	}
	if err := workspace.ValidatePortForwards(result.Forwards); err != nil {
		return result, fmt.Errorf("%s: %w", result.Source, err)
	}
	return result, nil
}
