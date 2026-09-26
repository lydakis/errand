package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
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
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand peers", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "emit configuration and complete runner facts as JSON")
	on := fs.String("on", "", "query only this configured peer")
	rawURL := fs.String("url", "", "query a peer base URL directly")
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "peers", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *on != "" && *rawURL != "" {
		return usageError(e, "--on and --url can't be combined")
	}
	rows, targets, err := peerListTargets(*on, *rawURL, deps)
	if err != nil {
		var unknown *config.UnknownPeerError
		if errors.As(err, &unknown) {
			return failWith(e, 2, err, errorScope{peer: *on})
		}
		e.Errorf("%v", err)
		if strings.Contains(err.Error(), "no peers configured") {
			e.Hintf("find runners on your tailnet with errand peers discover")
		}
		return 1
	}
	var spin *termui.Spinner
	if !*jsonOutput && !output.quiet {
		names := make([]string, 0, len(rows))
		for _, row := range rows {
			names = append(names, row.Name)
		}
		spin = e.Spin("Asking " + joinWords(names) + "…")
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
			if info.Version != version {
				rows[i].Detail = fmt.Sprintf("CLI %s; runner uses a different version", version)
			}
			if info.Busy {
				rows[i].Status = "busy"
			}
		}(i, target)
	}
	wg.Wait()
	if spin != nil {
		spin.Stop()
	}
	switch {
	case *jsonOutput:
		if code := writeJSONRows(stdout, stderr, rows); code != 0 {
			return code
		}
	case output.quiet:
		for _, row := range rows {
			if row.Info != nil {
				fmt.Fprintln(stdout, row.Name)
			}
		}
	default:
		writePeers(con.Out, rows, output.verbose)
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
	cfg = cfg.WithLocalPeer()
	if on == "local" {
		target, err := configuredPeerURL(cfg, on)
		if err != nil {
			return nil, nil, err
		}
		return []peerRow{{Name: on, Target: target, Default: cfg.DefaultPeer == on}}, []string{target}, nil
	}
	if on != "" {
		if _, ok := cfg.Peers[on]; !ok {
			return nil, nil, &config.UnknownPeerError{Name: on}
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
		if strings.HasPrefix(target, "unix://") {
			rows[i].Target = target
		}
	}
	return rows, targets, nil
}
