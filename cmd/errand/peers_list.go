package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

// Keep configuration and failed probes alongside live facts in every view.
// JSON always returns an array, including when a single peer is selected.
type peerRow struct {
	Name    string      `json:"name"`
	Target  string      `json:"target"`
	Default bool        `json:"default"`
	Status  string      `json:"status"`
	Detail  string      `json:"detail,omitempty"`
	Info    *proto.Info `json:"info,omitempty"`
}

func cmdPeersList(args []string, stdout, stderr io.Writer, deps peersDeps) int {
	fs := flag.NewFlagSet("errand peers", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit configuration and complete runner facts as JSON")
	on := fs.String("on", "", "query only this configured peer")
	rawURL := fs.String("url", "", "query a peer base URL directly")
	setFlagUsage(fs, "errand peers [--json] [--on PEER | --url URL]")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand peers: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *on != "" && *rawURL != "" {
		fmt.Fprintln(stderr, "errand peers: --on and --url are mutually exclusive")
		return 2
	}
	rows, targets, err := peerListTargets(*on, *rawURL, deps)
	if err != nil {
		fmt.Fprintf(stderr, "errand peers: %v\n", err)
		return 1
	}
	var wg sync.WaitGroup
	for i, target := range targets {
		if target == "" { // Misconfigured peers already have a diagnostic row.
			continue
		}
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			info, err := deps.probe(context.Background(), target)
			if err != nil {
				kind, _ := client.ProbeKindOf(err)
				rows[i].Status = string(kind)
				rows[i].Detail = err.Error()
				return
			}
			rows[i].Info = &info
			rows[i].Status = "ready"
			if info.Busy {
				rows[i].Status = "busy"
			}
		}(i, target)
	}
	wg.Wait()
	if *jsonOutput {
		if code := writeJSONRows(stdout, stderr, rows); code != 0 {
			return code
		}
	} else {
		writePeers(stdout, rows)
	}
	for _, row := range rows {
		if row.Info == nil {
			return 1
		}
	}
	return 0
}

func peerListTargets(on, rawURL string, deps peersDeps) ([]peerRow, []string, error) {
	if rawURL != "" {
		target := strings.TrimSuffix(rawURL, "/")
		return []peerRow{{Name: target, Target: target}}, []string{target}, nil
	}
	cfg, err := deps.load()
	if err != nil {
		return nil, nil, err
	}
	if on != "" {
		if _, ok := cfg.Peers[on]; !ok {
			return nil, nil, fmt.Errorf("unknown peer %q", on)
		}
		cfg.Peers = map[string]config.Peer{on: cfg.Peers[on]}
	}
	if len(cfg.Peers) == 0 {
		return nil, nil, fmt.Errorf("no peers configured; try `errand peers discover` or `errand peers add NAME HOST`")
	}
	names := make([]string, 0, len(cfg.Peers))
	for name := range cfg.Peers {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]peerRow, len(names))
	targets := make([]string, len(names))
	for i, name := range names {
		rows[i] = peerRow{Name: name, Target: peerURLOf(cfg.Peers[name]), Default: name == cfg.DefaultPeer}
		target, err := configuredPeerURL(cfg, name)
		if err != nil {
			rows[i].Status = "misconfigured"
			rows[i].Detail = err.Error()
			continue
		}
		targets[i] = target
	}
	return rows, targets, nil
}
