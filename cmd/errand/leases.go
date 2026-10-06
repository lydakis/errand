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
	"slices"
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

// leaseAdmitWait bounds how long a device waits for a cloud peer to add its
// key to a ready machine: a few tries of one SSH command.
var leaseAdmitWait = 2 * time.Minute

func init() { config.FindLeasePeer = findLeasePeer }

const leaseRequestTimeout = 30 * time.Second

// matchingOffer returns the first offer whose declared facts satisfy q.
func matchingOffer(q placement.Requirements, offers []proto.Offer) (proto.Offer, bool) {
	var best proto.Offer
	found := false
	for _, o := range offers {
		if len(q.Missing(o.Facts)) == 0 && (!found || placement.CheaperOffer(o, best)) {
			best, found = o, true
		}
	}
	return best, found
}

// describeOffer names an offer with its price when the cloud peer sets one.
func describeOffer(o proto.Offer) string {
	if o.PricePerHour > 0 {
		return fmt.Sprintf("%s ($%.2f/h)", o.Name, o.PricePerHour)
	}
	return o.Name
}

// leasePeer is a machine the caller leased, used as a peer. Clients keep no
// record of their leases: cloud peers list them in their info.
type leasePeer struct {
	Name   string
	Broker string
	Lease  proto.Lease
	Peer   config.Peer
}

// leasePeersOf names the ready leases a cloud peer reported as peers, and
// trusts their host keys for this process.
func leasePeersOf(cfg config.Client, broker string, info proto.Info) []leasePeer {
	ids := make([]string, 0, len(info.Leases))
	for _, l := range info.Leases {
		ids = append(ids, l.ID)
	}
	identity, publicKey := clientLeaseIdentity()
	var out []leasePeer
	for _, l := range info.Leases {
		if l.State != proto.LeaseReady || l.Target == nil || !proto.ValidULID(l.ID) {
			continue
		}
		// A machine reached over SSH lets in a device of the owner's only
		// once the cloud peer added its key.
		if !admits(l, publicKey) {
			continue
		}
		peer, err := leaseTargetPeer(*l.Target, identity)
		if err != nil {
			continue
		}
		out = append(out, leasePeer{Name: leasePeerName(cfg, broker, l.ID, ids), Broker: broker, Lease: l, Peer: peer})
	}
	return out
}

// admits reports whether a ready lease lets in the device with publicKey.
func admits(l proto.Lease, publicKey string) bool {
	return l.Target == nil || l.Target.SSH == "" || len(l.SSHKeys) == 0 || slices.Contains(l.SSHKeys, publicKey)
}

// admitLeaseKeys lets more of a lease owner's devices into a machine reached
// over SSH, for a cloud peer. Each key goes on its own line of the login's
// authorized_keys, once.
func admitLeaseKeys(ctx context.Context, t proto.LeaseTarget, identity string, keys []string) error {
	if _, err := leaseTargetPeer(t, identity); err != nil {
		return err
	}
	// A last line without its newline would swallow the first added key.
	const script = `umask 077 && mkdir -p ~/.ssh && touch ~/.ssh/authorized_keys && { [ ! -s ~/.ssh/authorized_keys ] || [ -z "$(tail -c 1 ~/.ssh/authorized_keys)" ] || echo >> ~/.ssh/authorized_keys; } && while IFS= read -r key; do grep -qxF "$key" ~/.ssh/authorized_keys || printf '%s\n' "$key" >> ~/.ssh/authorized_keys || exit 1; done`
	return client.RunSSH(ctx, t.SSH, script, strings.NewReader(strings.Join(keys, "\n")+"\n"))
}

// leasePeerName names a lease after its cloud peer and the end of its ID,
// such as cloud-7f3a: as short as keeps it apart from configured peers and
// the cloud peer's other leases, which ids lists.
func leasePeerName(cfg config.Client, broker, id string, ids []string) string {
	id = strings.ToLower(id)
	for n := 4; n < len(id); n++ {
		suffix := id[len(id)-n:]
		name := broker + "-" + suffix
		if _, taken := cfg.Peers[name]; taken {
			continue
		}
		if !slices.ContainsFunc(ids, func(other string) bool {
			other = strings.ToLower(other)
			return other != id && strings.HasSuffix(other, suffix)
		}) {
			return name
		}
	}
	return broker + "-" + id
}

// splitLeaseName reads a lease peer name as a cloud peer's name and the end
// of a lease ID. Any unambiguous ending of four or more characters works,
// so a name stays valid as the cloud peer's leases come and go.
func splitLeaseName(cfg config.Client, name string) (broker, suffix string, ok bool) {
	for i := len(name) - 5; i > 0; i-- {
		if name[i] != '-' {
			continue
		}
		broker, suffix = name[:i], name[i+1:]
		if _, configured := cfg.Peers[broker]; !configured && broker != "local" {
			continue
		}
		if strings.Trim(suffix, "0123456789abcdefghjkmnpqrstvwxyz") == "" {
			return broker, suffix, true
		}
	}
	return "", "", false
}

// findLeasePeer resolves a lease peer name by asking its cloud peer, for
// config.FindLeasePeer.
func findLeasePeer(cfg config.Client, name string) (config.Peer, bool, error) {
	lp, ok, err := leasePeerNamed(cfg, name)
	return lp.Peer, ok, err
}

// leasePeerNamed finds the ready lease a name such as cloud-7f3a stands for.
// It reports false for a name that is not a lease name.
func leasePeerNamed(cfg config.Client, name string) (leasePeer, bool, error) {
	broker, suffix, ok := splitLeaseName(cfg, name)
	if !ok {
		return leasePeer{}, false, nil
	}
	target, err := configuredPeerURL(cfg, broker)
	if err != nil {
		return leasePeer{}, false, err
	}
	info, err := client.ProbeInfo(context.Background(), target, 10*time.Second)
	if err != nil {
		return leasePeer{}, false, fmt.Errorf("asking %s for your leases: %w", broker, err)
	}
	// The name is matched against every ready lease, including ones this
	// device is not let into yet, so it never stands for one of two.
	var match *proto.Lease
	for i, l := range info.Leases {
		if l.State != proto.LeaseReady || !proto.ValidULID(l.ID) || !strings.HasSuffix(strings.ToLower(l.ID), suffix) {
			continue
		}
		if match != nil {
			return leasePeer{}, false, fmt.Errorf("more than one lease on %s ends in %s; use a longer name", broker, suffix)
		}
		match = &info.Leases[i]
	}
	if match == nil {
		return leasePeer{}, false, fmt.Errorf("%s has no ready lease of yours whose ID ends in %s; see errand leases", broker, suffix)
	}
	for _, lp := range leasePeersOf(cfg, broker, info) {
		if lp.Lease.ID == match.ID {
			return lp, true, nil
		}
	}
	if _, publicKey := clientLeaseIdentity(); admits(*match, publicKey) {
		return leasePeer{}, false, fmt.Errorf("lease %s has an unusable target", match.ID)
	}
	// One of the owner's other devices asked for it: this one is let in by
	// naming it.
	return admitThisDevice(cfg, broker, target, match.ID, info)
}

// admitThisDevice asks a cloud peer to let this device into the owner's
// ready lease id, and waits until it has.
func admitThisDevice(cfg config.Client, broker, target, id string, info proto.Info) (leasePeer, bool, error) {
	keyFile, err := leaseKeyFile()
	if err != nil {
		return leasePeer{}, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), leaseAdmitWait)
	defer cancel()
	sshKey, err := client.EnsureSSHKey(ctx, keyFile, "errand")
	if err != nil {
		return leasePeer{}, false, err
	}
	fmt.Fprintf(os.Stderr, "errand: asking %s to let this device into lease %s\n", terminalSafeField(broker), id)
	lease, err := client.AdmitLeaseKey(ctx, target, id, sshKey)
	for err == nil && lease.State == proto.LeaseReady && !admits(lease, sshKey) {
		select {
		case <-ctx.Done():
			return leasePeer{}, false, fmt.Errorf("%s has not let this device into lease %s after %s (errand leases shows why)", broker, id, leaseAdmitWait)
		case <-time.After(leasePollInterval):
		}
		lease, err = client.GetLease(ctx, target, id)
	}
	if err != nil {
		return leasePeer{}, false, fmt.Errorf("letting this device into lease %s: %w", id, err)
	}
	if lease.State != proto.LeaseReady {
		return leasePeer{}, false, fmt.Errorf("lease %s is %s", id, lease.State)
	}
	info.Leases = []proto.Lease{lease}
	for _, lp := range leasePeersOf(cfg, broker, info) {
		return lp, true, nil
	}
	return leasePeer{}, false, fmt.Errorf("lease %s has an unusable target", id)
}

// leaseTargetPeer is the peer entry for a lease target. The host key of an
// ssh target is trusted for this process, with identity offered when set.
func leaseTargetPeer(t proto.LeaseTarget, identity string) (config.Peer, error) {
	peer := config.LeasePeer(t)
	if err := config.ValidatePeer("lease", peer); err != nil {
		return config.Peer{}, err
	}
	if t.HostKey != "" {
		if err := client.TrustSSHHost(t.SSH, t.HostKey, identity); err != nil {
			return config.Peer{}, err
		}
	}
	return peer, nil
}

// leaseCandidate is a lease peer as a run candidate.
func leaseCandidate(lp leasePeer) config.RunCandidate {
	url, _ := (config.Client{Peers: map[string]config.Peer{lp.Name: lp.Peer}}).PeerURL(lp.Name)
	return config.RunCandidate{Name: lp.Name, URL: url, RemoteCommand: lp.Peer.RemoteCommand, RemoteSocket: lp.Peer.RemoteSocket}
}

// leaseRunner acquires a machine from the first cloud peer among options
// that does not turn the request down, waits until it runs errand, and
// returns it as an ordinary placement choice. A failure or Ctrl-C before
// the machine is ready withdraws the run's lease request, which cancels a
// launch no other run is waiting for. From ready on, the cloud peer's idle
// rule decides when the lease ends.
func leaseRunner(options []leaseOption, where string, stderr io.Writer) (placementChoice, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A machine reached over SSH admits this client by its own errand key.
	keyFile, err := leaseKeyFile()
	if err != nil {
		return placementChoice{}, err
	}
	sshKey, err := client.EnsureSSHKey(ctx, keyFile, "errand")
	if err != nil {
		return placementChoice{}, err
	}
	var broker placementChoice
	var lease proto.Lease
	requestID := proto.NewULID()
	for i, opt := range options {
		broker = opt.Broker
		fmt.Fprintf(stderr, "errand: no runner of yours matches %s; leasing %s from %s\n", terminalSafeField(where), terminalSafeField(describeOffer(opt.Offer)), terminalSafeField(broker.Name))
		// Ctrl-C does not cut the request short: the cloud peer may already
		// be launching, and a withdrawal that overtook the request would
		// find nothing to withdraw.
		lease, err = client.AcquireLease(context.WithoutCancel(ctx), broker.Target, requestID, where, sshKey, leaseRequestTimeout)
		if err == nil {
			break
		}
		err = fmt.Errorf("leasing from %s: %w", terminalSafeField(broker.Name), err)
		// Another supplier is asked only when this one started nothing.
		if !client.LeaseRefused(err) || i == len(options)-1 || ctx.Err() != nil {
			return placementChoice{}, err
		}
		fmt.Fprintf(stderr, "errand: %v\n", err)
	}
	choice, err := followLease(ctx, broker, lease, where, keyFile, sshKey, stderr)
	if err != nil {
		outcome := withdrawLease(broker, requestID, lease.ID)
		if ctx.Err() != nil {
			return placementChoice{}, fmt.Errorf("interrupted; %s", outcome)
		}
		return placementChoice{}, fmt.Errorf("%w; %s", err, outcome)
	}
	return choice, nil
}

// followLease waits until a lease is ready, lets this device in, and this
// device reaches it.
func followLease(ctx context.Context, broker placementChoice, lease proto.Lease, where, keyFile, sshKey string, stderr io.Writer) (placementChoice, error) {
	brokerName := terminalSafeField(broker.Name)
	shown := 0
	var admitBy time.Time
	if lease.State == proto.LeaseReady {
		// An existing lease of yours already matches; its history is old news.
		shown = len(lease.Progress)
		fmt.Fprintf(stderr, "errand: %s: reusing your ready lease %s\n", brokerName, lease.ID)
	}
	for ctx.Err() == nil {
		for ; shown < len(lease.Progress); shown++ {
			fmt.Fprintf(stderr, "errand: %s: %s\n", brokerName, terminalSafeField(lease.Progress[shown]))
		}
		if lease.State != proto.LeaseLaunching && (lease.State != proto.LeaseReady || admits(lease, sshKey)) {
			break
		}
		if lease.State == proto.LeaseReady {
			if admitBy.IsZero() {
				admitBy = time.Now().Add(leaseAdmitWait)
				fmt.Fprintf(stderr, "errand: %s: waiting for lease %s to let this device in\n", brokerName, lease.ID)
			} else if time.Now().After(admitBy) {
				return placementChoice{}, fmt.Errorf("lease %s from %s has not let this device in after %s (errand leases shows why)", lease.ID, broker.Name, leaseAdmitWait)
			}
		}
		select {
		case <-ctx.Done():
			continue
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
	if ctx.Err() != nil {
		return placementChoice{}, ctx.Err()
	}
	if lease.State != proto.LeaseReady || lease.Target == nil {
		detail := lease.Error
		if detail == "" {
			detail = "lease " + lease.State
		}
		return placementChoice{}, fmt.Errorf("lease %s from %s did not become ready: %s", lease.ID, broker.Name, detail)
	}
	return readyLease(ctx, broker, lease, where, keyFile, stderr)
}

// withdrawLease tells the cloud peer a run no longer needs the lease its
// request was given, and says what became of it.
func withdrawLease(broker placementChoice, requestID, id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lease, err := client.WithdrawLeaseRequest(ctx, broker.Target, requestID)
	switch {
	case err != nil:
		return fmt.Sprintf("withdrawing lease %s failed: %v (errand leases release --on %s %s releases it)", id, err, broker.Name, id)
	case lease.State == proto.LeaseLaunching:
		return fmt.Sprintf("lease %s keeps launching for another run", id)
	case lease.State == proto.LeaseReady:
		return fmt.Sprintf("lease %s stays until idle", id)
	}
	return fmt.Sprintf("released lease %s", id)
}

// leaseKeyFile is the SSH key this client sends with lease requests.
func leaseKeyFile() (string, error) {
	dir, err := config.StateDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ssh", "errand_ed25519"), nil
}

// clientLeaseIdentity is this client's lease key file and its public key,
// once it has one.
func clientLeaseIdentity() (keyFile, publicKey string) {
	keyFile, err := leaseKeyFile()
	if err != nil {
		return "", ""
	}
	public, err := os.ReadFile(keyFile + ".pub")
	if err != nil {
		return "", ""
	}
	if _, err := os.Stat(keyFile); err != nil {
		return "", ""
	}
	return keyFile, strings.TrimSpace(string(public))
}

// readyLease checks that this machine reaches a ready lease and that it
// matches, and returns it as a placement choice.
func readyLease(ctx context.Context, broker placementChoice, lease proto.Lease, where, keyFile string, stderr io.Writer) (placementChoice, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return placementChoice{}, err
	}
	peer, err := leaseTargetPeer(*lease.Target, keyFile)
	if err != nil {
		return placementChoice{}, fmt.Errorf("lease %s has an unusable target: %w", lease.ID, err)
	}
	ids := []string{lease.ID}
	for _, l := range broker.Info.Leases {
		ids = append(ids, l.ID)
	}
	candidate := leaseCandidate(leasePeer{Name: leasePeerName(cfg, broker.Name, lease.ID, ids), Peer: peer})
	name := candidate.Name
	target := client.ConfigureSSHPeer(candidate.URL, name, candidate.RemoteCommand, candidate.RemoteSocket)
	info, err := client.ProbeWhereInfo(ctx, target, where, 10*time.Second)
	if err != nil {
		return placementChoice{}, fmt.Errorf("lease %s is ready but this machine cannot reach it: %w", name, err)
	}
	if missing := mustParseWhere(where).Missing(info.Facts); len(missing) > 0 {
		return placementChoice{}, fmt.Errorf("lease %s does not match %s: %s", name, where, strings.Join(missing, "; "))
	}
	fmt.Fprintf(stderr, "errand: lease %s ready (%s)\n", terminalSafeField(name), terminalSafeField(describeMachine(info.Facts)))
	return placementChoice{RunCandidate: candidate, Info: info, Target: target}, nil
}

// probeLeaseTarget lets a cloud peer watch the machines it leased, over the
// same transports a client would use, offering identity over SSH.
func probeLeaseTarget(ctx context.Context, t proto.LeaseTarget, identity, where string) (proto.Info, error) {
	peer, err := leaseTargetPeer(t, identity)
	if err != nil {
		return proto.Info{}, err
	}
	c := leaseCandidate(leasePeer{Name: "lease", Peer: peer})
	target := client.ConfigureSSHPeer(c.URL, c.Name, c.RemoteCommand, c.RemoteSocket)
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
	case "release":
		setFlagUsage(fs, "errand leases release [--on PEER] LEASE...")
	default:
		fmt.Fprintf(stderr, "errand leases: unknown command %q (use ls or release)\n", verb)
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
	if verb == "release" {
		if fs.NArg() == 0 {
			fmt.Fprintln(stderr, "errand leases release: name at least one lease")
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
		var active []string
		for _, l := range leases {
			if l.Active() {
				active = append(active, l.ID)
			}
		}
		for _, l := range leases {
			r := row{Broker: b.name, Lease: l}
			if l.State == proto.LeaseReady {
				r.Peer = leasePeerName(cfg, b.name, l.ID, active)
			}
			rows = append(rows, r)
		}
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

type leaseBroker struct{ name, target string }

// leaseBrokers lists configured peers that advertise offers or hold leases
// of the caller's.
func leaseBrokers(cfg config.Client, on string, stderr io.Writer) ([]leaseBroker, int) {
	// Placement can rent from the implicit local peer too.
	cfg = cfg.WithLocalPeer()
	var names []string
	for name := range cfg.Peers {
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
		// A cloud peer whose offers were removed still lists, and ends,
		// the leases it made.
		if err != nil || len(info.Offers) == 0 && len(info.Leases) == 0 {
			if on != "" {
				fmt.Fprintf(stderr, "errand leases: %s has no cloud offers or leases of yours\n", on)
				return nil, 1
			}
			continue
		}
		out = append(out, leaseBroker{name, target})
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "errand leases: no configured peer has cloud offers or leases of yours")
		return nil, 1
	}
	return out, 0
}

// releaseLeases accepts lease peer names (cloud-7f3a), or lease IDs with
// --on.
// leaseOwnerPeer finds the cloud peer that made the lease with this ID.
func leaseOwnerPeer(cfg config.Client, id string) (string, error) {
	brokers, _ := leaseBrokers(cfg, "", io.Discard)
	for _, b := range brokers {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		leases, err := client.ListLeases(ctx, b.target)
		cancel()
		if err != nil {
			continue
		}
		for _, l := range leases {
			if l.ID == id {
				return b.name, nil
			}
		}
	}
	return "", fmt.Errorf("no cloud peer that answered has a lease of yours with this ID; pass --on CLOUD")
}

func releaseLeases(cfg config.Client, on string, names []string, stdout, stderr io.Writer) int {
	code := 0
	for _, arg := range names {
		broker, id := on, arg
		if !proto.ValidULID(arg) {
			b, suffix, ok := splitLeaseName(cfg, arg)
			if !ok || on != "" && on != b {
				fmt.Fprintf(stderr, "errand leases release: %q is not a lease name; pass one such as cloud-7f3a, or --on CLOUD with a lease ID\n", arg)
				code = 2
				continue
			}
			var err error
			if broker, id, err = findLeaseID(cfg, b, suffix); err != nil {
				fmt.Fprintf(stderr, "errand leases release: %s: %v\n", terminalSafeField(arg), err)
				code = 1
				continue
			}
		} else if on == "" {
			var err error
			if broker, err = leaseOwnerPeer(cfg, id); err != nil {
				fmt.Fprintf(stderr, "errand leases release: %s: %v\n", arg, err)
				code = 1
				continue
			}
		}
		target, err := configuredPeerURL(cfg, broker)
		if err != nil {
			fmt.Fprintf(stderr, "errand leases release: %v\n", err)
			code = 1
			continue
		}
		lease, err := waitReleased(target, id)
		if err != nil {
			fmt.Fprintf(stderr, "errand leases release: %s: %v\n", terminalSafeField(arg), err)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "%s %s\n", terminalSafeField(arg), lease.State)
	}
	return code
}

// findLeaseID finds the caller's active lease on broker whose ID ends in
// suffix, launching ones included.
func findLeaseID(cfg config.Client, broker, suffix string) (string, string, error) {
	target, err := configuredPeerURL(cfg, broker)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	leases, err := client.ListLeases(ctx, target)
	if err != nil {
		return "", "", err
	}
	var ids []string
	for _, l := range leases {
		if l.Active() && strings.HasSuffix(strings.ToLower(l.ID), suffix) {
			ids = append(ids, l.ID)
		}
	}
	switch len(ids) {
	case 0:
		return "", "", fmt.Errorf("%s has no active lease of yours whose ID ends in %s", broker, suffix)
	case 1:
		return broker, ids[0], nil
	}
	return "", "", fmt.Errorf("more than one lease on %s ends in %s; use a longer name", broker, suffix)
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
