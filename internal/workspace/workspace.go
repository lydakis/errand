// Package workspace discovers the local snapshot boundary and command workdir.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const markerName = ".errand.toml"

type Selection struct {
	Caches         Caches
	Artifacts      Artifacts
	Session        Session
	Environment    Environment
	Root           string
	Workdir        string
	Project        string
	Source         string
	ApplyOnSuccess *bool
	Peer           *string
	Profiles       map[string]Profile
}

type projectConfig struct {
	Caches      Caches             `toml:"caches"`
	Artifacts   Artifacts          `toml:"artifacts"`
	Session     Session            `toml:"session"`
	Environment Environment        `toml:"env"`
	Profiles    map[string]Profile `toml:"profiles"`
	Run         struct {
		Peer *string `toml:"peer"`
	} `toml:"run"`
	Workspace struct {
		Root bool `toml:"root"`
	} `toml:"workspace"`
	Changes struct {
		ApplyOnSuccess *bool `toml:"apply_on_success"`
	} `toml:"changes"`
}

// Discover selects an explicit root, the nearest marked ancestor, or cwd.
// Explicit and marked roots only establish a boundary; snapshot policy is
// enforced separately by the snapshot package.
func Discover(cwd, explicit string) (Selection, error) {
	cwd, err := canonicalDir(cwd)
	if err != nil {
		return Selection{}, fmt.Errorf("workspace: resolving current directory: %w", err)
	}
	if explicit != "" {
		if !filepath.IsAbs(explicit) {
			explicit = filepath.Join(cwd, explicit)
		}
		root, err := canonicalDir(explicit)
		if err != nil {
			return Selection{}, fmt.Errorf("workspace: resolving --workspace-root: %w", err)
		}
		cfg, err := readExplicitConfig(root)
		if err != nil {
			return Selection{}, err
		}
		return selection(root, cwd, "--workspace-root", cfg)
	}

	var currentConfig projectConfig
	for dir := cwd; ; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, markerName)
		if markerInfo, err := os.Stat(marker); err == nil {
			trusted, err := trustedWorkspaceMarker(dir, markerInfo)
			if err != nil {
				return Selection{}, fmt.Errorf("workspace: checking trust for %s: %w", marker, err)
			}
			if trusted {
				var cfg projectConfig
				if _, err := toml.DecodeFile(marker, &cfg); err != nil {
					return Selection{}, fmt.Errorf("workspace: reading %s: %w", marker, err)
				}
				if dir == cwd {
					currentConfig = cfg
				}
				if cfg.Workspace.Root {
					return selection(dir, cwd, marker, cfg)
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return selection(cwd, cwd, "current directory", currentConfig)
}

func readExplicitConfig(root string) (projectConfig, error) {
	var cfg projectConfig
	marker := filepath.Join(root, markerName)
	info, err := os.Lstat(marker)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("workspace: inspecting %s: %w", marker, err)
	}
	if !info.Mode().IsRegular() {
		return cfg, fmt.Errorf("workspace: %s is not a regular file", marker)
	}
	if _, err := toml.DecodeFile(marker, &cfg); err != nil {
		return cfg, fmt.Errorf("workspace: reading %s: %w", marker, err)
	}
	return cfg, nil
}

func trustedWorkspaceMarker(dir string, markerInfo os.FileInfo) (bool, error) {
	if !markerInfo.Mode().IsRegular() || !ownedByCurrentUser(markerInfo) {
		return false, nil
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return false, err
	}
	if !ownedByCurrentUser(dirInfo) || dirInfo.Mode().Perm()&0o022 != 0 {
		return false, nil
	}
	return true, nil
}

func canonicalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func selection(root, cwd, source string, cfg projectConfig) (Selection, error) {
	rel, err := filepath.Rel(root, cwd)
	if err != nil {
		return Selection{}, fmt.Errorf("workspace: deriving command workdir: %w", err)
	}
	if rel == ".." || (len(rel) > 3 && rel[:3] == ".."+string(filepath.Separator)) {
		return Selection{}, fmt.Errorf("workspace: root %q does not contain current directory %q", root, cwd)
	}
	if rel == "." {
		rel = ""
	} else {
		rel = filepath.ToSlash(rel)
	}
	project := filepath.Base(root)
	if rel != "" && !hasGitMetadata(root) {
		project = strings.SplitN(rel, "/", 2)[0]
	}
	return Selection{
		Artifacts: cfg.Artifacts, Caches: cfg.Caches,
		Environment: cfg.Environment, Session: cfg.Session,
		Root: root, Workdir: rel, Project: project, Source: source,
		ApplyOnSuccess: cfg.Changes.ApplyOnSuccess, Peer: cfg.Run.Peer, Profiles: cfg.Profiles,
	}, nil
}

func hasGitMetadata(root string) bool {
	_, err := os.Lstat(filepath.Join(root, ".git"))
	return err == nil
}
