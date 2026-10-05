// Package cloud turns configured offers into leased runners. A broker hands
// out machines; jobs then run on them directly, never through the broker.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/durable"
	"github.com/lydakis/errand/internal/placement"
	"github.com/lydakis/errand/internal/proto"
)

// Offer is a machine shape and the provider that acquires and releases one.
type Offer struct {
	Name         string
	Facts        proto.Facts
	Provider     Provider
	PricePerHour float64 // USD, informational
	IdleTimeout  time.Duration
	MaxLifetime  time.Duration
}

// ProbeFunc asks a leased runner for its info. A nonempty where asks it to
// measure those requirements, as --where selection does.
type ProbeFunc func(ctx context.Context, target proto.LeaseTarget, where string) (proto.Info, error)

type Config struct {
	StateDir       string // leases are persisted under StateDir/leases
	Offers         []Offer
	MaxLeases      int
	AcquireTimeout time.Duration
	ReleaseTimeout time.Duration
	Probe          ProbeFunc
	// ReadyPoll paces readiness checks while launching; IdlePoll paces idle
	// checks and release retries.
	ReadyPoll, IdlePoll time.Duration
}

const (
	maxProgressLines  = 100
	maxProgressLine   = 300
	maxAcquireOutput  = 64 << 10
	endedLeaseHistory = 7 * 24 * time.Hour
)

// Error carries the HTTP status a refusal maps to.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

type record struct {
	proto.Lease
	Owner         string          `json:"owner"`
	Login         string          `json:"login,omitempty"`    // admitted by the machine
	SSHKey        string          `json:"ssh_key,omitempty"`  // the caller's public key, admitted by the machine
	Provider      string          `json:"provider,omitempty"` // kind of provider that acquired it
	ProviderState json.RawMessage `json:"provider_state,omitempty"`
	LastBusy      time.Time       `json:"last_busy,omitzero"`
}

type lease struct {
	record
	cancelLaunch context.CancelFunc
	launching    bool // the launch goroutine still owns the lease
	releasing    bool // a release command is running
}

type Broker struct {
	cfg    Config
	offers map[string]Offer
	dir    string

	mu     sync.Mutex
	leases map[string]*lease
	closed bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(cfg Config) (*Broker, error) {
	if len(cfg.Offers) == 0 {
		return nil, fmt.Errorf("cloud broker needs at least one offer")
	}
	if cfg.Probe == nil {
		return nil, fmt.Errorf("cloud broker needs a runner probe")
	}
	if cfg.MaxLeases <= 0 {
		cfg.MaxLeases = 2
	}
	if cfg.AcquireTimeout <= 0 {
		cfg.AcquireTimeout = 15 * time.Minute
	}
	if cfg.ReleaseTimeout <= 0 {
		cfg.ReleaseTimeout = 5 * time.Minute
	}
	if cfg.ReadyPoll <= 0 {
		cfg.ReadyPoll = 3 * time.Second
	}
	if cfg.IdlePoll <= 0 {
		cfg.IdlePoll = 30 * time.Second
	}
	b := &Broker{cfg: cfg, offers: map[string]Offer{}, dir: filepath.Join(cfg.StateDir, "leases"), leases: map[string]*lease{}}
	for _, o := range cfg.Offers {
		if o.Name == "" || o.Provider == nil || o.IdleTimeout <= 0 || o.MaxLifetime <= 0 {
			return nil, fmt.Errorf("cloud offer %q is incomplete", o.Name)
		}
		if _, dup := b.offers[o.Name]; dup {
			return nil, fmt.Errorf("cloud offer %q is defined twice", o.Name)
		}
		// Providers that keep files of their own keep them with the leases.
		if k, ok := o.Provider.(interface{ useStateDir(string) }); ok {
			k.useStateDir(cfg.StateDir)
		}
		b.offers[o.Name] = o
	}
	if err := os.MkdirAll(b.dir, 0700); err != nil {
		return nil, err
	}
	if err := b.recover(); err != nil {
		return nil, err
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.wg.Add(1)
	go b.reap()
	return b, nil
}

// providerKind names the kind of provider, which must stay the same for an
// offer while it has active leases.
func providerKind(p Provider) string {
	switch p.(type) {
	case *LambdaProvider:
		return "lambda"
	case CommandProvider, *CommandProvider:
		return "command"
	}
	return fmt.Sprintf("%T", p)
}

// recover reloads persisted leases. A launch cut short by a restart may
// already have created a machine, so it is released rather than resumed.
func (b *Broker) recover() error {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !proto.ValidULID(id) {
			continue
		}
		path := filepath.Join(b.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r record
		if err := json.Unmarshal(data, &r); err != nil || r.ID != id {
			return fmt.Errorf("lease record %s is unreadable; inspect or remove it", path)
		}
		switch r.State {
		case proto.LeaseLaunching:
			r.State = proto.LeaseReleasing
			r.Error = "broker restarted while the machine was launching"
		case proto.LeaseReady:
			r.LastBusy = now // grant a full idle window after a restart
		case proto.LeaseReleased, proto.LeaseFailed:
			if now.Sub(r.ReleasedAt) > endedLeaseHistory {
				_ = os.Remove(path)
				continue
			}
		}
		// Only the kind of provider that acquired a machine can release it.
		if r.State != proto.LeaseReleased && r.State != proto.LeaseFailed {
			offer, ok := b.offers[r.Offer]
			if !ok {
				return fmt.Errorf("lease %s is still active on cloud offer %q, which is no longer configured; restore the offer until the lease is released", id, r.Offer)
			}
			// Every record names its provider, so one that does not is not
			// trusted to any provider either.
			if r.Provider != providerKind(offer.Provider) {
				return fmt.Errorf("lease %s on cloud offer %q was acquired by a %q provider, but the offer now uses %s; restore it until the lease is released", id, r.Offer, r.Provider, providerKind(offer.Provider))
			}
		}
		b.leases[id] = &lease{record: r}
		if err := b.persist(&r); err != nil {
			return err
		}
	}
	return nil
}

// Offers describes what this broker can lease.
func (b *Broker) Offers() []proto.Offer {
	out := make([]proto.Offer, 0, len(b.cfg.Offers))
	for _, o := range b.cfg.Offers {
		out = append(out, proto.Offer{Name: o.Name, Facts: o.Facts, PricePerHour: o.PricePerHour, IdleTimeoutSec: int64(o.IdleTimeout / time.Second), MaxLifetimeSec: int64(o.MaxLifetime / time.Second)})
	}
	return out
}

// Acquire returns the owner's matching ready or launching lease, or starts
// a new one from the first matching offer. login is the caller's tailnet
// login, when it has one.
// sshKey is the caller's SSH public key, or empty; a machine reached over SSH
// admits only that key.
func (b *Broker) Acquire(owner, login, where, sshKey string) (proto.Lease, error) {
	q, err := placement.Parse(where)
	if err != nil {
		return proto.Lease{}, &Error{http.StatusBadRequest, err.Error()}
	}
	if q.Any() {
		return proto.Lease{}, &Error{http.StatusBadRequest, "refusing to lease for where '*'; name the capability you need"}
	}
	if owner == "" {
		return proto.Lease{}, &Error{http.StatusForbidden, "caller has no ownership identity"}
	}
	if sshKey != "" && !ValidSSHPublicKey(sshKey) {
		return proto.Lease{}, &Error{http.StatusBadRequest, "ssh_key is not one SSH public key"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return proto.Lease{}, &Error{http.StatusServiceUnavailable, "cloud broker is shutting down"}
	}
	var launching *lease
	active := 0
	for _, l := range b.sorted() {
		if l.Active() {
			active++
		}
		// The machine admits the login and key it was launched for. The login
		// can change while the owner (a tailnet user ID) stays the same, and
		// each machine the owner runs errand from has its own key.
		if l.Owner != owner || l.Login != login || l.SSHKey != sshKey {
			continue
		}
		switch {
		case l.State == proto.LeaseReady && l.Facts != nil && len(q.Missing(*l.Facts)) == 0:
			return l.view(), nil
		case l.State == proto.LeaseLaunching && launching == nil && len(q.Missing(b.offers[l.Offer].Facts)) == 0:
			launching = l
		}
	}
	if launching != nil {
		return launching.view(), nil
	}
	var offer *Offer
	var reasons []string
	for i := range b.cfg.Offers {
		o := &b.cfg.Offers[i]
		missing := q.Missing(o.Facts)
		if len(missing) == 0 {
			offer = o
			break
		}
		reasons = append(reasons, o.Name+": "+strings.Join(missing, "; "))
	}
	if offer == nil {
		return proto.Lease{}, &Error{http.StatusPreconditionFailed, fmt.Sprintf("no offer matches %q: %s", where, strings.Join(reasons, "; "))}
	}
	if active >= b.cfg.MaxLeases {
		return proto.Lease{}, &Error{http.StatusTooManyRequests, fmt.Sprintf("lease limit reached (%d active, max_leases = %d)", active, b.cfg.MaxLeases)}
	}
	now := time.Now()
	l := &lease{record: record{Owner: owner, Login: login, SSHKey: sshKey, Provider: providerKind(offer.Provider), Lease: proto.Lease{
		ID: proto.NewULID(), Offer: offer.Name, Where: where, State: proto.LeaseLaunching,
		CreatedAt: now, ExpiresAt: now.Add(offer.MaxLifetime),
	}}}
	l.addProgress("launching " + offer.Name)
	// Persist before acquiring so a crash cannot forget a machine being paid for.
	if err := b.persist(&l.record); err != nil {
		return proto.Lease{}, &Error{http.StatusInternalServerError, "recording lease: " + err.Error()}
	}
	// The hard stop holds while launching too: a launch still running at the
	// lease's max lifetime is canceled and released.
	ctx, cancel := context.WithTimeout(b.ctx, b.launchLimit(*offer))
	l.cancelLaunch, l.launching = cancel, true
	b.leases[l.ID] = l
	b.wg.Add(1)
	go b.launch(ctx, l, *offer)
	return l.view(), nil
}

func (b *Broker) launch(ctx context.Context, l *lease, offer Offer) {
	defer b.wg.Done()
	defer l.cancelLaunch()
	machine, err := offer.Provider.Acquire(ctx, AcquireRequest{
		LeaseID: l.ID, Offer: offer.Name, Where: l.Where, Login: l.Login, SSHKey: l.SSHKey,
		Progress: func(line string) { b.progress(l, line) },
		Save: func(state json.RawMessage) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			// Kept in memory even if the write fails, for this process's release.
			l.ProviderState = state
			return b.persist(&l.record)
		},
	})
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("acquire did not finish within %s", b.launchLimit(offer))
		} else {
			err = fmt.Errorf("acquire canceled")
		}
	}
	target := machine.Target
	if err == nil {
		b.mu.Lock()
		l.ProviderState = machine.State
		err = b.persist(&l.record)
		b.mu.Unlock()
		if err != nil {
			err = fmt.Errorf("recording the machine: %w", err)
		} else {
			err = checkTarget(target)
		}
	}
	var facts proto.Facts
	if err == nil {
		b.progress(l, "waiting for errand on the machine")
		facts, err = b.waitReady(ctx, l, target)
	}
	b.mu.Lock()
	if b.closed {
		// Shutdown is not a failure; recovery releases the launch on restart.
		b.mu.Unlock()
		return
	}
	now := time.Now()
	switch {
	case l.State != proto.LeaseLaunching:
		// Released by its owner while launching.
	case err != nil:
		l.State = proto.LeaseReleasing
		l.Error = err.Error()
		l.addProgress("launch failed: " + err.Error())
	default:
		l.State = proto.LeaseReady
		l.Target = &target
		l.Facts = &facts
		l.ReadyAt = now
		l.LastBusy = now
		l.IdleUntil = now.Add(offer.IdleTimeout)
		l.addProgress(fmt.Sprintf("ready after %s", now.Sub(l.CreatedAt).Round(time.Second)))
		// A restart releases a lease recorded as launching, so the lease is
		// ready only once the record says so.
		if perr := b.persist(&l.record); perr != nil {
			l.State = proto.LeaseReleasing
			l.Target, l.Facts = nil, nil
			l.ReadyAt, l.IdleUntil = time.Time{}, time.Time{}
			l.Error = "recording the ready lease: " + perr.Error()
			l.addProgress("launch failed: " + l.Error)
		}
	}
	_ = b.persist(&l.record)
	l.launching = false
	release := l.State == proto.LeaseReleasing
	b.mu.Unlock()
	if release {
		b.release(l)
	}
}

// launchLimit is how long a launch of offer may take.
func (b *Broker) launchLimit(offer Offer) time.Duration {
	return min(b.cfg.AcquireTimeout, offer.MaxLifetime)
}

// waitReady polls until the machine runs errand and its measured facts
// satisfy the lease's requirements; an offer's facts are only a claim.
func (b *Broker) waitReady(ctx context.Context, l *lease, target proto.LeaseTarget) (proto.Facts, error) {
	q, _ := placement.Parse(l.Where)
	last := ""
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := b.cfg.Probe(probeCtx, target, l.Where)
		cancel()
		reason := ""
		if err != nil {
			reason = "not answering yet: " + err.Error()
		} else if missing := q.Missing(info.Facts); len(missing) > 0 {
			reason = "machine does not match: " + strings.Join(missing, "; ")
		} else {
			return info.Facts, nil
		}
		if reason != last {
			b.progress(l, reason)
			last = reason
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return proto.Facts{}, fmt.Errorf("machine was not ready within %s (%s)", b.launchLimit(b.offers[l.Offer]), last)
			}
			return proto.Facts{}, fmt.Errorf("launch canceled")
		case <-time.After(b.cfg.ReadyPoll):
		}
	}
}

// Release ends an owner's lease. A launching lease stops its acquire first.
func (b *Broker) Release(owner, id string) (proto.Lease, error) {
	b.mu.Lock()
	l, ok := b.leases[id]
	if !ok || l.Owner != owner {
		b.mu.Unlock()
		return proto.Lease{}, &Error{http.StatusNotFound, "no such lease"}
	}
	// The release is acknowledged only once it is recorded, so a restart
	// cannot turn it back into a usable lease.
	requestRelease := func(note string) error {
		was, progress := l.State, slices.Clone(l.Progress)
		l.State = proto.LeaseReleasing
		l.addProgress(note)
		if err := b.persist(&l.record); err != nil {
			l.State, l.Progress = was, progress
			return &Error{http.StatusInternalServerError, "recording the release: " + err.Error()}
		}
		return nil
	}
	switch l.State {
	case proto.LeaseLaunching:
		if err := requestRelease("release requested while launching"); err != nil {
			b.mu.Unlock()
			return proto.Lease{}, err
		}
		// launch stops on the canceled context, then releases with any
		// provider state acquire printed before it stopped.
		l.cancelLaunch()
		v := l.view()
		b.mu.Unlock()
		return v, nil
	case proto.LeaseReady:
		if err := requestRelease("release requested"); err != nil {
			b.mu.Unlock()
			return proto.Lease{}, err
		}
		if !b.closed {
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				b.release(l)
			}()
		}
	}
	v := l.view()
	b.mu.Unlock()
	return v, nil
}

func (b *Broker) release(l *lease) {
	b.mu.Lock()
	if l.releasing || l.launching || l.State != proto.LeaseReleasing || b.closed {
		b.mu.Unlock()
		return
	}
	l.releasing = true
	offer := b.offers[l.Offer]
	state := string(l.ProviderState)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(b.ctx, b.cfg.ReleaseTimeout)
	defer cancel()
	err := offer.Provider.Release(ctx, ReleaseRequest{LeaseID: l.ID, Offer: l.Offer, State: json.RawMessage(state)})

	b.mu.Lock()
	defer b.mu.Unlock()
	l.releasing = false
	if b.closed {
		return
	}
	if err != nil {
		l.addProgress(fmt.Sprintf("release failed: %v; retrying", err))
		_ = b.persist(&l.record)
		return
	}
	l.State = proto.LeaseReleased
	if l.Error != "" {
		l.State = proto.LeaseFailed
	}
	l.ReleasedAt = time.Now()
	l.IdleUntil = time.Time{}
	l.addProgress("released")
	_ = b.persist(&l.record)
}

// reap releases idle and expired leases and retries failed releases.
func (b *Broker) reap() {
	defer b.wg.Done()
	ticker := time.NewTicker(b.cfg.IdlePoll)
	defer ticker.Stop()
	for {
		b.reapOnce()
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Broker) reapOnce() {
	b.mu.Lock()
	var ready, releasing []*lease
	for id, l := range b.leases {
		switch {
		case (l.State == proto.LeaseReleased || l.State == proto.LeaseFailed) && time.Since(l.ReleasedAt) > endedLeaseHistory:
			if err := os.Remove(filepath.Join(b.dir, id+".json")); err == nil || errors.Is(err, os.ErrNotExist) {
				delete(b.leases, id)
			}
		case l.State == proto.LeaseReady:
			ready = append(ready, l)
		case l.State == proto.LeaseReleasing && !l.releasing && !l.launching:
			releasing = append(releasing, l)
		}
	}
	b.mu.Unlock()
	for _, l := range releasing {
		b.release(l)
	}
	// Probes run together, so one slow or lost machine cannot hold back the
	// idle check, or the hard stop, of the others.
	var probes sync.WaitGroup
	for _, l := range ready {
		b.mu.Lock()
		expired := !time.Now().Before(l.ExpiresAt)
		target := *l.Target
		b.mu.Unlock()
		if expired {
			b.endReady(l, nil, nil)
			continue
		}
		probes.Add(1)
		go func() {
			defer probes.Done()
			ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
			info, err := b.cfg.Probe(ctx, target, "")
			cancel()
			b.endReady(l, &info, err)
		}()
	}
	probes.Wait()
}

// endReady applies one idle check to a ready lease, releasing it once it is
// idle or out of lifetime. A nil info skips the probe result.
func (b *Broker) endReady(l *lease, info *proto.Info, probeErr error) {
	now := time.Now()
	b.mu.Lock()
	if l.State != proto.LeaseReady {
		b.mu.Unlock()
		return
	}
	offer := b.offers[l.Offer]
	// An unreachable machine does not count as busy, so a lost box ends
	// after one idle window instead of running until its hard stop.
	if info != nil && probeErr == nil && info.StagingJobs+info.StartingJobs+info.RunningJobs+info.QueuedJobs > 0 {
		l.LastBusy = now
	}
	l.IdleUntil = l.LastBusy.Add(offer.IdleTimeout)
	reason := ""
	switch {
	case !now.Before(l.ExpiresAt):
		reason = fmt.Sprintf("max lifetime of %s reached", offer.MaxLifetime)
	case !now.Before(l.IdleUntil):
		reason = fmt.Sprintf("idle for %s", offer.IdleTimeout)
	}
	if reason != "" {
		l.State = proto.LeaseReleasing
		l.addProgress("releasing: " + reason)
	}
	_ = b.persist(&l.record)
	b.mu.Unlock()
	if reason != "" {
		b.release(l)
	}
}

// Get returns one of the owner's leases.
func (b *Broker) Get(owner, id string) (proto.Lease, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.leases[id]
	if !ok || l.Owner != owner {
		return proto.Lease{}, false
	}
	return l.view(), true
}

// List returns the owner's leases, newest first, including recently ended ones.
func (b *Broker) List(owner string) []proto.Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []proto.Lease
	for _, l := range b.sorted() {
		if l.Owner == owner {
			out = append(out, l.view())
		}
	}
	return out
}

// ActiveIDs lists the owner's leases that may still hold a machine.
func (b *Broker) ActiveIDs(owner string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range b.sorted() {
		if l.Owner == owner && l.Active() {
			out = append(out, l.ID)
		}
	}
	return out
}

// Close stops background work without releasing anything: leases persist
// and are recovered by the next broker start.
func (b *Broker) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
}

func (b *Broker) sorted() []*lease {
	out := make([]*lease, 0, len(b.leases))
	for _, l := range b.leases {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (b *Broker) progress(l *lease, line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l.addProgress(line)
	_ = b.persist(&l.record)
}

func (l *lease) addProgress(line string) {
	line = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, line)
	if len(line) > maxProgressLine {
		line = line[:maxProgressLine]
	}
	if len(l.Progress) >= maxProgressLines {
		l.Progress = append(l.Progress[:0:0], l.Progress[1:]...)
	}
	l.Progress = append(l.Progress, line)
}

func (l *lease) view() proto.Lease {
	v := l.Lease
	v.Progress = append([]string(nil), l.Progress...)
	return v
}

func (b *Broker) persist(r *record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dest := filepath.Join(b.dir, r.ID+".json")
	f, err := os.CreateTemp(b.dir, ".lease-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := durable.Sync(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	dir, err := os.Open(b.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return durable.Sync(dir)
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if room := w.limit - w.Len(); room < len(p) {
		w.overflow = true
		if room > 0 {
			w.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return w.Buffer.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)
