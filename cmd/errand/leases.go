package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/placement"
	"github.com/lydakis/errand/internal/proto"
)

// leaseOption is a cloud peer offer that can satisfy --where when no runner
// of the caller's own matches it.
type leaseOption struct {
	Broker placementChoice
	Offer  proto.Offer
}

var leasePollInterval = time.Second

const leaseRequestTimeout = 30 * time.Second

// matchingOffer returns the first offer whose declared facts satisfy q.
func matchingOffer(q placement.Requirements, offers []proto.Offer) (proto.Offer, bool) {
	for _, o := range offers {
		if len(q.Missing(o.Facts)) == 0 {
			return o, true
		}
	}
	return proto.Offer{}, false
}

// describeOffer names an offer with its price when the cloud peer sets one.
func describeOffer(o proto.Offer) string {
	if o.PricePerHour > 0 {
		return fmt.Sprintf("%s ($%.2f/h)", o.Name, o.PricePerHour)
	}
	return o.Name
}

// forgetEndedLeases drops lease peers that probed cloud peers no longer
// report as active. It returns the forgotten names.
func forgetEndedLeases(probed map[string]proto.Info) map[string]bool {
	brokers := map[string]map[string]bool{}
	for name, info := range probed {
		if len(info.Offers) == 0 {
			continue
		}
		active := map[string]bool{}
		for _, id := range info.Leases {
			active[id] = true
		}
		brokers[name] = active
	}
	if len(brokers) == 0 {
		return nil
	}
	dropped, err := config.ForgetLeases(func(_ string, rec config.LeaseRecord) bool {
		active, probedBroker := brokers[rec.Broker]
		return !probedBroker || active[rec.ID]
	})
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, name := range dropped {
		out[name] = true
	}
	return out
}

// leaseRunner acquires a machine from a cloud peer, waits until it runs
// errand, and returns it as an ordinary placement choice. Ctrl-C while the
// machine is launching releases the lease, so nothing unused keeps running.
func leaseRunner(opt leaseOption, where string, stderr io.Writer) (placementChoice, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	broker := opt.Broker
	brokerName := terminalSafeField(broker.Name)
	fmt.Fprintf(stderr, "errand: no runner of yours matches %s; leasing %s from %s\n", terminalSafeField(where), terminalSafeField(describeOffer(opt.Offer)), brokerName)
	// Ctrl-C does not cut the request short: the cloud peer may already be
	// launching, and only its answer names the lease to release. The loop
	// below releases a launching lease once it sees the interrupt.
	// A machine reached over SSH admits this client by its own errand key.
	keyFile, err := leaseKeyFile()
	if err != nil {
		return placementChoice{}, err
	}
	sshKey, err := client.EnsureSSHKey(ctx, keyFile, "errand")
	if err != nil {
		return placementChoice{}, err
	}
	acquireCtx, cancelAcquire := context.WithTimeout(context.WithoutCancel(ctx), leaseRequestTimeout)
	lease, err := client.AcquireLease(acquireCtx, broker.Target, where, sshKey)
	cancelAcquire()
	if err != nil {
		return placementChoice{}, fmt.Errorf("leasing from %s: %w", broker.Name, err)
	}
	if ctx.Err() != nil && lease.State != proto.LeaseLaunching {
		return placementChoice{}, fmt.Errorf("interrupted; lease %s is %s", lease.ID, lease.State)
	}
	shown := 0
	if lease.State == proto.LeaseReady {
		// An existing lease of yours already matches; its history is old news.
		shown = len(lease.Progress)
		fmt.Fprintf(stderr, "errand: %s: reusing your ready lease %s\n", brokerName, lease.ID)
	}
	for {
		for ; shown < len(lease.Progress); shown++ {
			fmt.Fprintf(stderr, "errand: %s: %s\n", brokerName, terminalSafeField(lease.Progress[shown]))
		}
		if lease.State != proto.LeaseLaunching {
			break
		}
		select {
		case <-ctx.Done():
			releaseCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, releaseErr := client.ReleaseLease(releaseCtx, broker.Target, lease.ID)
			cancel()
			if releaseErr != nil {
				return placementChoice{}, fmt.Errorf("interrupted; releasing lease %s failed: %v (run errand leases rm --on %s %s)", lease.ID, releaseErr, broker.Name, lease.ID)
			}
			return placementChoice{}, fmt.Errorf("interrupted; released lease %s", lease.ID)
		case <-time.After(leasePollInterval):
		}
		before := len(lease.Progress)
		next, err := client.GetLease(ctx, broker.Target, lease.ID)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			return placementChoice{}, fmt.Errorf("following lease %s: %w", lease.ID, err)
		}
		// Progress is a bounded tail; restart numbering if it was trimmed.
		if len(next.Progress) < before {
			shown = 0
		}
		lease = next
	}
	if lease.State != proto.LeaseReady || lease.Target == nil {
		detail := lease.Error
		if detail == "" {
			detail = "lease " + lease.State
		}
		return placementChoice{}, fmt.Errorf("lease %s from %s did not become ready: %s", lease.ID, broker.Name, detail)
	}
	return recordLeasePeer(ctx, broker.Name, lease, where, keyFile, stderr)
}

// leaseKeyFile is the SSH key this client sends with lease requests.
func leaseKeyFile() (string, error) {
	dir, err := config.StateDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ssh", "errand_ed25519"), nil
}

func recordLeasePeer(ctx context.Context, brokerName string, lease proto.Lease, where, keyFile string, stderr io.Writer) (placementChoice, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return placementChoice{}, err
	}
	configured := map[string]config.Peer{}
	for name, peer := range cfg.Peers {
		if _, isLease := cfg.Leases[name]; !isLease {
			configured[name] = peer
		}
	}
	name, err := config.RecordLease(config.LeaseRecord{Broker: brokerName, ID: lease.ID, Offer: lease.Offer, Target: *lease.Target, CreatedAt: lease.CreatedAt}, configured)
	if err != nil {
		return placementChoice{}, fmt.Errorf("recording lease %s: %w", lease.ID, err)
	}
	peer, peerURL, err := leasePeer(name, *lease.Target)
	if err != nil {
		return placementChoice{}, fmt.Errorf("lease %s has an unusable target: %w", name, err)
	}
	if lease.Target.SSH != "" {
		if err := client.PinSSHIdentity(peerURL, keyFile); err != nil {
			return placementChoice{}, err
		}
	}
	target := client.ConfigureSSHPeer(peerURL, name, peer.RemoteCommand, peer.RemoteSocket)
	info, err := client.ProbeWhereInfo(ctx, target, where, 10*time.Second)
	if err != nil {
		return placementChoice{}, fmt.Errorf("lease %s is ready but this machine cannot reach it: %w", name, err)
	}
	if missing := mustParseWhere(where).Missing(info.Facts); len(missing) > 0 {
		return placementChoice{}, fmt.Errorf("lease %s does not match %s: %s", name, where, strings.Join(missing, "; "))
	}
	fmt.Fprintf(stderr, "errand: lease %s ready (%s)\n", terminalSafeField(name), terminalSafeField(describeMachine(info.Facts)))
	return placementChoice{
		RunCandidate: config.RunCandidate{Name: name, URL: peerURL, RemoteCommand: peer.RemoteCommand, RemoteSocket: peer.RemoteSocket},
		Info:         info,
		Target:       target,
	}, nil
}

// leasePeer resolves a lease target as a peer named name, pinning the host
// key of an ssh target it carries one for.
func leasePeer(name string, t proto.LeaseTarget) (config.Peer, string, error) {
	peer := config.LeasePeer(t)
	peerURL, err := (config.Client{Peers: map[string]config.Peer{name: peer}}).PeerURL(name)
	if err != nil {
		return config.Peer{}, "", err
	}
	if t.HostKey != "" {
		if err := client.PinSSHHost(peerURL, t.HostKey); err != nil {
			return config.Peer{}, "", err
		}
	}
	return peer, peerURL, nil
}

// probeLeaseTarget lets a cloud peer watch the machines it leased, over the
// same transports a client would use.
func probeLeaseTarget(ctx context.Context, t proto.LeaseTarget, where string) (proto.Info, error) {
	const name = "lease"
	peer, peerURL, err := leasePeer(name, t)
	if err != nil {
		return proto.Info{}, err
	}
	target := client.ConfigureSSHPeer(peerURL, name, peer.RemoteCommand, peer.RemoteSocket)
	if where != "" {
		return client.ProbeWhereInfo(ctx, target, where, 10*time.Second)
	}
	return client.ProbeInfo(ctx, target, 10*time.Second)
}

func mustParseWhere(where string) placement.Requirements {
	q, _ := placement.Parse(where) // validated during configuration resolution
	return q
}

func describeMachine(f proto.Facts) string {
	parts := []string{strings.Trim(f.OS+"/"+f.Arch, "/")}
	if f.NumCPU > 0 {
		parts = append(parts, fmt.Sprintf("%d cpu", f.NumCPU))
	}
	if len(f.GPUs) > 0 {
		parts = append(parts, placement.DescribeGPUs(f.GPUs))
	}
	return strings.Join(parts, ", ")
}

// cmdLeases lists or releases the caller's leases on configured cloud peers.
func cmdLeases(args []string, stdout, stderr io.Writer) int {
	verb := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("errand leases "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	on := fs.String("on", "", "only this cloud peer")
	asJSON := fs.Bool("json", false, "print leases as JSON")
	switch verb {
	case "ls", "list":
		setFlagUsage(fs, "errand leases [ls] [--on PEER] [--json]")
	case "rm":
		setFlagUsage(fs, "errand leases rm [--on PEER] LEASE...")
	default:
		fmt.Fprintf(stderr, "errand leases: unknown command %q (use ls or rm)\n", verb)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	cfg, err := config.LoadClient()
	if err != nil {
		fmt.Fprintln(stderr, "errand leases:", err)
		return 1
	}
	if verb == "rm" {
		if fs.NArg() == 0 {
			fmt.Fprintln(stderr, "errand leases rm: name at least one lease")
			return 2
		}
		return releaseLeases(cfg, *on, fs.Args(), stdout, stderr)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand leases: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	brokers, code := leaseBrokers(cfg, *on, stderr)
	if brokers == nil {
		return code
	}
	type row struct {
		Peer   string `json:"peer"`
		Broker string `json:"broker"`
		proto.Lease
	}
	var rows []row
	for _, b := range brokers {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		leases, err := client.ListLeases(ctx, b.target)
		cancel()
		if err != nil {
			fmt.Fprintf(stderr, "errand leases: %s: %v\n", terminalSafeField(b.name), err)
			code = 1
			continue
		}
		active := map[string]bool{}
		for _, l := range leases {
			if l.Active() {
				active[l.ID] = true
			}
			rows = append(rows, row{Peer: leasePeerName(cfg, b.name, l.ID), Broker: b.name, Lease: l})
		}
		config.ForgetLeases(func(_ string, rec config.LeaseRecord) bool { return rec.Broker != b.name || active[rec.ID] })
	}
	if *asJSON {
		if rows == nil {
			rows = []row{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return 1
		}
		return code
	}
	tw := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tCLOUD\tOFFER\tSTATE\tAGE\tRELEASES\tDETAIL")
	now := time.Now()
	for _, r := range rows {
		releases := ""
		switch {
		case r.State == proto.LeaseReady && !r.IdleUntil.IsZero() && r.IdleUntil.Before(r.ExpiresAt):
			releases = "idle " + until(now, r.IdleUntil)
		case r.Active():
			releases = "by " + until(now, r.ExpiresAt)
		}
		detail := r.Error
		if detail == "" && r.Facts != nil {
			detail = describeMachine(*r.Facts)
		}
		peer := r.Peer
		if peer == "" {
			peer = r.ID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", terminalSafeField(peer), terminalSafeField(r.Broker), terminalSafeField(r.Offer), r.State,
			now.Sub(r.CreatedAt).Round(time.Second), releases, terminalSafeField(detail))
	}
	tw.Flush()
	return code
}

func until(now, t time.Time) string {
	if !t.After(now) {
		return "now"
	}
	return "in " + t.Sub(now).Round(time.Second).String()
}

func leasePeerName(cfg config.Client, broker, id string) string {
	for name, rec := range cfg.Leases {
		if rec.Broker == broker && rec.ID == id {
			return name
		}
	}
	return ""
}

type leaseBroker struct{ name, target string }

// leaseBrokers lists configured peers that advertise offers.
func leaseBrokers(cfg config.Client, on string, stderr io.Writer) ([]leaseBroker, int) {
	var names []string
	for name := range cfg.Peers {
		if _, isLease := cfg.Leases[name]; isLease {
			continue
		}
		if on == "" || on == name {
			names = append(names, name)
		}
	}
	if on != "" && len(names) == 0 {
		fmt.Fprintf(stderr, "errand leases: unknown peer %q\n", on)
		return nil, 2
	}
	sort.Strings(names)
	var out []leaseBroker
	for _, name := range names {
		target, err := configuredPeerURL(cfg, name)
		if err != nil {
			continue
		}
		info, err := client.ProbeInfo(context.Background(), target, 2*time.Second)
		if err != nil || len(info.Offers) == 0 {
			if on != "" {
				fmt.Fprintf(stderr, "errand leases: %s has no cloud offers\n", on)
				return nil, 1
			}
			continue
		}
		out = append(out, leaseBroker{name, target})
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "errand leases: no configured peer has cloud offers")
		return nil, 1
	}
	return out, 0
}

// releaseLeases accepts lease peer names (cloud-7f3a) or lease IDs.
func releaseLeases(cfg config.Client, on string, names []string, stdout, stderr io.Writer) int {
	code := 0
	for _, arg := range names {
		broker, id := on, arg
		if rec, ok := cfg.Leases[arg]; ok {
			if on != "" && on != rec.Broker {
				fmt.Fprintf(stderr, "errand leases rm: %s is leased from %s, not %s\n", arg, rec.Broker, on)
				code = 2
				continue
			}
			broker, id = rec.Broker, rec.ID
		} else if !proto.ValidULID(arg) || on == "" {
			fmt.Fprintf(stderr, "errand leases rm: %q is not a known lease; pass a lease peer name, or --on CLOUD with a lease ID\n", arg)
			code = 2
			continue
		}
		target, err := configuredPeerURL(cfg, broker)
		if err != nil {
			fmt.Fprintf(stderr, "errand leases rm: %v\n", err)
			code = 1
			continue
		}
		lease, err := waitReleased(target, id)
		if err != nil {
			fmt.Fprintf(stderr, "errand leases rm: %s: %v\n", arg, err)
			code = 1
			continue
		}
		config.ForgetLeases(func(_ string, rec config.LeaseRecord) bool { return rec.Broker != broker || rec.ID != id })
		fmt.Fprintf(stdout, "%s %s\n", terminalSafeField(arg), lease.State)
	}
	return code
}

func waitReleased(target, id string) (proto.Lease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	lease, err := client.ReleaseLease(ctx, target, id)
	for err == nil && lease.Active() {
		select {
		case <-ctx.Done():
			return lease, errors.New("release is still running on the cloud peer; check errand leases")
		case <-time.After(leasePollInterval):
		}
		lease, err = client.GetLease(ctx, target, id)
	}
	return lease, err
}
