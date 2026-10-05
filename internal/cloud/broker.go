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

// ProbeFunc asks a leased runner for its info, offering identity (a private
// key file, or empty) over SSH. A nonempty where asks it to measure those
// requirements, as --where selection does.
type ProbeFunc func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error)

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
	maxAcquireOutput  = 16 << 10 // release gets it in one environment variable, which Windows caps at 32K characters
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
	Owner string `json:"owner"`
	Login string `json:"login,omitempty"` // admitted by the machine
	// RequestID names the request that made the lease, so a client that
	// lost the answer can ask again and get the same lease.
	RequestID string `json:"request_id,omitempty"`
	// Unwanted marks a ready lease whose request was withdrawn: it is
	// released at the next check unless a job is running on it.
	Unwanted bool `json:"unwanted,omitempty"`
	// Release and IdleTimeout are fixed when the lease is made, so the lease
	// ends the way it was made whatever later happens to its offer.
	Release       ReleaseSpec     `json:"release"`
	IdleTimeout   time.Duration   `json:"idle_timeout"`
	ProviderState json.RawMessage `json:"provider_state,omitempty"`
	// Identity is the private key file this cloud peer reaches the machine
	// with, when the provider made one.
	Identity string    `json:"identity,omitempty"`
	LastBusy time.Time `json:"last_busy,omitzero"`
}

// A lease is owned by one worker goroutine, which makes every provider call
// for it. Everything else only asks for a state change through update and
// wakes the worker.
type lease struct {
	record
	stop context.CancelFunc // cancels the running acquire or probe
	wake chan struct{}
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

// New starts a broker and a worker for every lease it recorded before. With
// no offers it leases nothing new but still ends the leases it has.
func New(cfg Config) (*Broker, error) {
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
		if _, err := o.Provider.ReleaseSpec().provider(); err != nil {
			return nil, fmt.Errorf("cloud offer %q: %w", o.Name, err)
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
	records, err := b.load()
	if err != nil {
		return nil, err
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	now := time.Now()
	for _, r := range records {
		l := &lease{record: r, wake: make(chan struct{}, 1)}
		switch r.State {
		case proto.LeaseLaunching:
			// An acquire cut short by a restart cannot be resumed, and may
			// already have created a machine.
			if err := b.update(l, func(r *record) bool {
				r.State = proto.LeaseReleasing
				r.Error = "broker restarted while the machine was launching"
				r.addProgress(r.Error)
				return true
			}); err != nil {
				b.cancel()
				return nil, fmt.Errorf("recording lease %s: %w", r.ID, err)
			}
		case proto.LeaseReady:
			l.LastBusy = now // a full idle window after a restart
		}
		b.leases[r.ID] = l
	}
	for _, l := range b.leases {
		b.start(l)
	}
	return b, nil
}

// load reads the recorded leases, dropping ended ones past their history.
func (b *Broker) load() ([]record, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	var records []record
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !proto.ValidULID(id) {
			continue
		}
		path := filepath.Join(b.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var r record
		if err := json.Unmarshal(data, &r); err != nil || r.ID != id {
			return nil, fmt.Errorf("lease record %s is unreadable; inspect or remove it", path)
		}
		if !r.Active() && time.Since(r.ReleasedAt) > endedLeaseHistory {
			_ = os.Remove(path)
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

// Offers describes what this broker can lease.
func (b *Broker) Offers() []proto.Offer {
	out := make([]proto.Offer, 0, len(b.cfg.Offers))
	for _, o := range b.cfg.Offers {
		out = append(out, proto.Offer{Name: o.Name, Facts: o.Facts, PricePerHour: o.PricePerHour, IdleTimeoutSec: int64(o.IdleTimeout / time.Second), MaxLifetimeSec: int64(o.MaxLifetime / time.Second)})
	}
	return out
}

// Acquire returns the lease an earlier request with the same requestID
// made, the owner's matching ready or launching lease, or a new one from the
// first matching offer. login is the caller's tailnet login, when it has
// one. sshKey is the caller's SSH public key, or empty; a machine reached
// over SSH admits only that key.
func (b *Broker) Acquire(owner, login, where, sshKey, requestID string) (proto.Lease, error) {
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
	if len(b.offers) == 0 {
		return proto.Lease{}, &Error{http.StatusNotFound, "this runner has no cloud offers"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return proto.Lease{}, &Error{http.StatusServiceUnavailable, "cloud broker is shutting down"}
	}
	var launching *lease
	active := 0
	for _, l := range b.sorted() {
		if requestID != "" && l.Owner == owner && l.RequestID == requestID {
			return l.view(), nil
		}
	}
	// A lease handed to a second request is no longer its first
	// requester's to withdraw.
	share := func(l *lease) (proto.Lease, error) {
		err := b.applyLocked(l, func(r *record) bool {
			if r.Shared {
				return false
			}
			r.Shared, r.Unwanted = true, false
			return true
		}, true)
		if err != nil {
			return proto.Lease{}, &Error{http.StatusInternalServerError, "recording the lease: " + err.Error()}
		}
		return l.view(), nil
	}
	for _, l := range b.sorted() {
		if l.Active() {
			active++
		}
		// The machine admits the login it was launched for, and over SSH the
		// key. The login can change while the owner (a tailnet user ID)
		// stays the same, and each machine the owner runs errand from has
		// its own key. Until a lease is ready its transport is unknown.
		reachedBySSH := l.Target == nil || l.Target.SSH != ""
		if l.Owner != owner || l.Login != login || reachedBySSH && l.SSHKey != sshKey {
			continue
		}
		switch {
		case l.State == proto.LeaseReady && l.Facts != nil && len(q.Missing(*l.Facts)) == 0:
			return share(l)
		case l.State == proto.LeaseLaunching && launching == nil && len(q.Missing(b.offers[l.Offer].Facts)) == 0:
			launching = l
		}
	}
	if launching != nil {
		return share(launching)
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
	l := &lease{wake: make(chan struct{}, 1), record: record{
		Owner: owner, Login: login, RequestID: requestID, Release: offer.Provider.ReleaseSpec(), IdleTimeout: offer.IdleTimeout,
		Lease: proto.Lease{ID: proto.NewULID(), Offer: offer.Name, Where: where, SSHKey: sshKey, State: proto.LeaseLaunching, CreatedAt: now, ExpiresAt: now.Add(offer.MaxLifetime)},
	}}
	l.addProgress("launching " + offer.Name)
	// Recorded before acquiring, so a crash cannot forget a machine being paid for.
	if err := b.persist(&l.record); err != nil {
		return proto.Lease{}, &Error{http.StatusInternalServerError, "recording lease: " + err.Error()}
	}
	b.leases[l.ID] = l
	b.start(l)
	return l.view(), nil
}

// Release ends an owner's lease. A launching lease stops its acquire first.
func (b *Broker) Release(owner, id string) (proto.Lease, error) {
	return b.end(owner, id, "release requested")
}

// EndAll releases every lease of owner that may still hold a machine.
func (b *Broker) EndAll(owner, why string) error {
	var errs []error
	for _, l := range b.Active(owner) {
		if _, err := b.end(owner, l.ID, "releasing: "+why); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *Broker) end(owner, id, note string) (proto.Lease, error) {
	b.mu.Lock()
	l, ok := b.leases[id]
	b.mu.Unlock()
	if !ok || l.Owner != owner {
		return proto.Lease{}, &Error{http.StatusNotFound, "no such lease"}
	}
	// The release is acknowledged only once it is recorded, so a restart
	// cannot turn it back into a usable lease.
	err := b.update(l, func(r *record) bool {
		switch r.State {
		case proto.LeaseLaunching:
			r.addProgress(note + " while launching")
		case proto.LeaseReady:
			r.addProgress(note)
		default:
			return false
		}
		r.State = proto.LeaseReleasing
		return true
	})
	if err != nil {
		return proto.Lease{}, &Error{http.StatusInternalServerError, "recording the release: " + err.Error()}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.stop != nil {
		l.stop()
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return l.view(), nil
}

// Withdraw is the client's answer to an interrupted run: the request that
// asked for a lease no longer needs it. The lease ends only if that request
// made it and no other request was handed it; a ready one is released at
// the next check unless a job is running on it.
func (b *Broker) Withdraw(owner, requestID string) (proto.Lease, error) {
	b.mu.Lock()
	var l *lease
	for _, c := range b.leases {
		if requestID != "" && c.Owner == owner && c.RequestID == requestID {
			l = c
		}
	}
	b.mu.Unlock()
	if l == nil {
		return proto.Lease{}, &Error{http.StatusNotFound, "no lease was made for that request"}
	}
	err := b.update(l, func(r *record) bool {
		if r.Shared {
			return false
		}
		switch r.State {
		case proto.LeaseLaunching:
			r.State = proto.LeaseReleasing
			r.addProgress("release requested while launching")
		case proto.LeaseReady:
			if r.Unwanted {
				return false
			}
			r.Unwanted = true
		default:
			return false
		}
		return true
	})
	if err != nil {
		return proto.Lease{}, &Error{http.StatusInternalServerError, "recording the withdrawal: " + err.Error()}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.stop != nil && l.State == proto.LeaseReleasing {
		l.stop()
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return l.view(), nil
}

// update is the only way a lease changes. change edits a copy of the record,
// or returns false to leave it as it is. The copy is written to disk before
// the lease takes it, so neither callers nor a restart ever see a state the
// disk does not have.
func (b *Broker) update(l *lease, change func(*record) bool) error {
	return b.apply(l, change, true)
}

// advance is update for a change that goes ahead even if it cannot be
// recorded. That is safe only where the state on disk already leads to the
// same end: a lease recorded as launching or releasing is released on the
// next start either way.
func (b *Broker) advance(l *lease, change func(*record) bool) {
	_ = b.apply(l, change, false)
}

func (b *Broker) apply(l *lease, change func(*record) bool, strict bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.applyLocked(l, change, strict)
}

func (b *Broker) applyLocked(l *lease, change func(*record) bool, strict bool) error {
	next := l.record
	next.Progress = slices.Clone(l.Progress)
	if !change(&next) {
		return nil
	}
	if next.ID != l.ID || !canMove(l.State, next.State) {
		panic(fmt.Sprintf("lease %s: invalid change from %s to %s", l.ID, l.State, next.State))
	}
	err := b.persist(&next)
	if err == nil || !strict {
		l.record = next
	}
	return err
}

// canMove reports whether a lease may go from one state to another. Leases
// only move forward, and a launch that never became ready skips ready.
func canMove(from, to string) bool {
	next := map[string][]string{
		proto.LeaseLaunching: {proto.LeaseReady, proto.LeaseReleasing},
		proto.LeaseReady:     {proto.LeaseReleasing},
		proto.LeaseReleasing: {proto.LeaseReleased, proto.LeaseFailed},
	}
	return from == to || slices.Contains(next[from], to)
}

// note shows a line to clients following the lease.
func (b *Broker) note(l *lease, line string) {
	b.advance(l, func(r *record) bool {
		r.addProgress(line)
		return true
	})
}

func (b *Broker) start(l *lease) {
	b.wg.Add(1)
	go b.work(l)
}

// work drives one lease through its states until it is forgotten.
func (b *Broker) work(l *lease) {
	defer b.wg.Done()
	for b.ctx.Err() == nil {
		b.mu.Lock()
		state := l.State
		b.mu.Unlock()
		switch state {
		case proto.LeaseLaunching:
			b.launch(l)
		case proto.LeaseReady:
			b.watch(l)
		case proto.LeaseReleasing:
			b.release(l)
		default:
			b.forget(l)
			return
		}
	}
}

// launch acquires the machine and waits for errand on it. It returns once
// the lease is ready or releasing, or the broker is closing, so it never
// acquires twice.
func (b *Broker) launch(l *lease) {
	offer := b.offers[l.Offer] // only this process's leases are launching
	b.mu.Lock()
	if l.State != proto.LeaseLaunching {
		b.mu.Unlock()
		return // released before the acquire started
	}
	// The hard stop holds while launching too.
	deadline := earlier(l.CreatedAt.Add(b.cfg.AcquireTimeout), l.ExpiresAt)
	ctx, cancel := context.WithDeadline(b.ctx, deadline)
	l.stop = cancel
	id, where, login, sshKey := l.ID, l.Where, l.Login, l.SSHKey
	b.mu.Unlock()
	defer cancel()

	machine, err := offer.Provider.Acquire(ctx, AcquireRequest{
		LeaseID: id, Offer: offer.Name, Where: where, Login: login, SSHKey: sshKey,
		Progress: func(line string) { b.note(l, line) },
		Save: func(state json.RawMessage) error {
			return b.update(l, func(r *record) bool {
				r.ProviderState = state
				return true
			})
		},
	})
	if ctx.Err() != nil {
		err = errors.New("acquire canceled")
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("acquire did not finish within %s", deadline.Sub(l.CreatedAt))
		}
	}
	if err == nil {
		if err = b.update(l, func(r *record) bool {
			r.ProviderState, r.Identity = machine.State, machine.Identity
			return true
		}); err != nil {
			err = fmt.Errorf("recording the machine: %w", err)
		} else {
			err = checkTarget(machine.Target)
		}
	}
	var facts proto.Facts
	if err == nil {
		b.note(l, "waiting for errand on the machine")
		facts, err = b.waitReady(ctx, l, machine)
	}
	if b.ctx.Err() != nil {
		return // the record still says launching, so the next start releases it
	}
	if err == nil {
		// A restart releases a lease recorded as launching, so the lease is
		// ready only once the record says so.
		if err = b.update(l, func(r *record) bool {
			if r.State != proto.LeaseLaunching {
				return false // released while launching
			}
			now := time.Now()
			r.State = proto.LeaseReady
			r.Target, r.Facts = &machine.Target, &facts
			r.ReadyAt, r.LastBusy, r.IdleUntil = now, now, now.Add(r.IdleTimeout)
			r.addProgress(fmt.Sprintf("ready after %s", now.Sub(r.CreatedAt).Round(time.Second)))
			return true
		}); err == nil {
			return
		}
		err = fmt.Errorf("recording the ready lease: %w", err)
	}
	b.advance(l, func(r *record) bool {
		if r.State != proto.LeaseLaunching {
			return false
		}
		r.State = proto.LeaseReleasing
		r.Error = err.Error()
		r.addProgress("launch failed: " + r.Error)
		return true
	})
}

// waitReady polls until the machine runs errand and its measured facts
// satisfy the lease's requirements; an offer's facts are only a claim.
func (b *Broker) waitReady(ctx context.Context, l *lease, m Machine) (proto.Facts, error) {
	q, _ := placement.Parse(l.Where)
	last := ""
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := b.cfg.Probe(probeCtx, m.Target, m.Identity, l.Where)
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
			b.note(l, reason)
			last = reason
		}
		select {
		case <-ctx.Done():
			return proto.Facts{}, fmt.Errorf("machine was not ready in time (%s)", last)
		case <-time.After(b.cfg.ReadyPoll):
		}
	}
}

// watch releases a ready lease once it is idle or out of lifetime. An
// unreachable machine does not count as busy, so a lost box ends after one
// idle window instead of running until its hard stop.
func (b *Broker) watch(l *lease) {
	for {
		b.mu.Lock()
		r := l.record
		b.mu.Unlock()
		if r.State != proto.LeaseReady {
			return
		}
		now := time.Now()
		reason := ""
		switch {
		case !now.Before(r.ExpiresAt):
			reason = fmt.Sprintf("max lifetime of %s reached", r.ExpiresAt.Sub(r.CreatedAt).Round(time.Second))
		case b.busy(l, r):
			// In use, so no longer unwanted: from here the idle rule decides.
			_ = b.update(l, func(r *record) bool {
				r.LastBusy, r.IdleUntil, r.Unwanted = now, now.Add(r.IdleTimeout), false
				return r.State == proto.LeaseReady
			})
		case r.Unwanted:
			reason = "the run that asked for it was interrupted"
		case !now.Before(r.LastBusy.Add(r.IdleTimeout)):
			reason = fmt.Sprintf("idle for %s", r.IdleTimeout)
		}
		// The machine is destroyed only once the release is recorded;
		// otherwise a restart would hand out a lease whose machine is gone.
		if reason != "" && b.update(l, func(r *record) bool {
			if r.State != proto.LeaseReady {
				return false
			}
			r.State = proto.LeaseReleasing
			r.addProgress("releasing: " + reason)
			return true
		}) == nil {
			return
		}
		wait := b.cfg.IdlePoll
		if left := time.Until(r.ExpiresAt); left > 0 && left < wait {
			wait = left
		}
		if !b.sleep(l, wait) {
			return
		}
	}
}

// busy reports whether the machine has work, asking no later than the
// lease's hard stop.
func (b *Broker) busy(l *lease, r record) bool {
	ctx, cancel := context.WithDeadline(b.ctx, earlier(time.Now().Add(5*time.Second), r.ExpiresAt))
	defer cancel()
	// A release request cancels the probe rather than wait for it.
	b.mu.Lock()
	l.stop = cancel
	stopped := l.State != proto.LeaseReady
	b.mu.Unlock()
	if stopped {
		return false
	}
	info, err := b.cfg.Probe(ctx, *r.Target, r.Identity, "")
	return err == nil && info.StagingJobs+info.StartingJobs+info.RunningJobs+info.QueuedJobs > 0
}

// release tries once to destroy the machine and records the outcome.
func (b *Broker) release(l *lease) {
	b.mu.Lock()
	r := l.record
	b.mu.Unlock()
	provider, err := b.releaser(r)
	if err == nil {
		ctx, cancel := context.WithTimeout(b.ctx, b.cfg.ReleaseTimeout)
		err = provider.Release(ctx, ReleaseRequest{LeaseID: r.ID, Offer: r.Offer, State: r.ProviderState})
		cancel()
	}
	if b.ctx.Err() != nil {
		return
	}
	var pending *ReleasePending
	if errors.As(err, &pending) {
		b.note(l, "release: "+pending.Msg)
		b.sleep(l, b.cfg.ReadyPoll)
		return
	}
	if err != nil {
		b.note(l, fmt.Sprintf("release failed: %v; retrying", err))
		b.sleep(l, b.cfg.IdlePoll)
		return
	}
	// Release may run again after a restart, so this need not be recorded.
	b.advance(l, func(r *record) bool {
		r.State = proto.LeaseReleased
		if r.Error != "" {
			r.State = proto.LeaseFailed
		}
		r.ReleasedAt, r.IdleUntil = time.Now(), time.Time{}
		r.addProgress("released")
		return true
	})
}

// releaser is the provider that releases r's machine: its offer's, while
// that offer still releases the way r was made, or else one built from the
// release settings r recorded.
func (b *Broker) releaser(r record) (Provider, error) {
	if o, ok := b.offers[r.Offer]; ok && o.Provider.ReleaseSpec().equal(r.Release) {
		return o.Provider, nil
	}
	return r.Release.provider()
}

// forget drops an ended lease once its history has been kept long enough.
func (b *Broker) forget(l *lease) {
	for {
		b.mu.Lock()
		left := time.Until(l.ReleasedAt.Add(endedLeaseHistory))
		b.mu.Unlock()
		if left <= 0 {
			break
		}
		if !b.sleep(l, left) {
			return
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.Remove(filepath.Join(b.dir, l.ID+".json")); err == nil || errors.Is(err, os.ErrNotExist) {
		delete(b.leases, l.ID)
	}
}

// sleep waits for d, or until the lease is woken. It reports false once the
// broker is closing.
func (b *Broker) sleep(l *lease, d time.Duration) bool {
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
		case <-l.wake:
		case <-timer.C:
		}
	}
	return b.ctx.Err() == nil
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

// Active lists the owner's leases that may still hold a machine, without
// their progress, newest first. Clients reach their leased machines through
// this list.
func (b *Broker) Active(owner string) []proto.Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []proto.Lease
	for _, l := range b.sorted() {
		if l.Owner == owner && l.Active() {
			v := l.Lease
			v.Progress = nil
			out = append(out, v)
		}
	}
	return out
}

// Close stops background work without releasing anything: leases persist
// and the next broker start picks them up.
func (b *Broker) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (b *Broker) sorted() []*lease {
	out := make([]*lease, 0, len(b.leases))
	for _, l := range b.leases {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (r *record) addProgress(line string) {
	line = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, line)
	if len(line) > maxProgressLine {
		line = line[:maxProgressLine]
	}
	if len(r.Progress) >= maxProgressLines {
		r.Progress = append(r.Progress[:0:0], r.Progress[1:]...)
	}
	r.Progress = append(r.Progress, line)
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
