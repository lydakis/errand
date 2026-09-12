package config

import (
	"fmt"
	"sort"

	"github.com/lydakis/errand/internal/placement"
	"github.com/lydakis/errand/internal/workspace"
)

// RunCandidate contains only personally configured transport authority.
type RunCandidate struct{ Name, URL, RemoteCommand, RemoteSocket string }

func resolvePlacement(out *EffectiveRun, personal Client, selected workspace.Selection, profile workspace.Profile, cli RunOverrides, personalSource, workspaceSource, profileSource string) error {
	set := func(peer, where *string, prefix, suffix string) error {
		source := func(field string) string { return prefix + field + suffix }
		if peer != nil && where != nil {
			return fmt.Errorf("%s: peer and where are mutually exclusive", source("peer/where"))
		}
		if where != nil {
			if _, err := placement.Parse(*where); err != nil {
				return fmt.Errorf("%s: %w", source("where"), err)
			}
			out.Where = *where
			out.Peer = ""
			delete(out.Sources, "peer")
			out.Sources["where"] = source("where")
		}
		if peer != nil {
			out.Peer = *peer
			out.Where = ""
			delete(out.Sources, "where")
			out.Sources["peer"] = source("peer")
		}
		return nil
	}
	if personal.DefaultWhere != "" && personal.DefaultPeer != "" {
		return fmt.Errorf("default_peer and default_where are mutually exclusive")
	}
	out.Peer = personal.DefaultPeer
	out.Sources["peer"] = personalSource + " (default_peer)"
	if personal.DefaultWhere != "" {
		if err := set(nil, &personal.DefaultWhere, personalSource+" (default_", ")"); err != nil {
			return err
		}
	}
	if err := set(selected.Peer, selected.Where, workspaceSource+" (run.", ")"); err != nil {
		return err
	}
	if err := set(profile.Run.Peer, profile.Run.Where, profileSource+" run.", ""); err != nil {
		return err
	}
	if cli.Peer != "" {
		out.Peer = cli.Peer
		out.Where = ""
		delete(out.Sources, "where")
		out.Sources["peer"] = "cli: --on"
	}
	if cli.URL != "" {
		out.Where = ""
		delete(out.Sources, "where")
	}
	if cli.Where != "" {
		if err := set(nil, &cli.Where, "cli: --", ""); err != nil {
			return err
		}
	}
	if out.Where == "" {
		return nil
	}
	names := make([]string, 0, len(personal.Peers))
	for name := range personal.Peers {
		names = append(names, name)
	}
	// Installing a local daemon does not authorize automatic local selection.
	if personal.DefaultPeer == "local" {
		if _, ok := personal.Peers["local"]; !ok {
			names = append(names, "local")
		}
	}
	sort.Strings(names)
	for _, name := range names {
		url, err := personal.PeerURL(name)
		if err != nil {
			return fmt.Errorf("peer %q: %w", name, err)
		}
		out.Candidates = append(out.Candidates, RunCandidate{Name: name, URL: url, RemoteCommand: personal.SSHRemoteCommand(name), RemoteSocket: personal.SSHRemoteSocket(name)})
	}
	if len(out.Candidates) == 0 {
		return fmt.Errorf("where has no personally configured runners; add a peer first")
	}
	return nil
}
