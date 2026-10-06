// Package cloud turns configured offers into leased runners. A broker hands
// out machines; jobs then run on them directly, never through the broker.
package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	// Unavailable says why the offer cannot be leased from right now, such
	// as a price above the cap. It is advertised like any other, so a
	// request only it would match comes here and is refused with this reason.
	Unavailable string
}

// ProbeFunc asks a leased runner for its info, offering identity (a private
// key file, or empty) over SSH. A nonempty where asks it to measure those
// requirements, as --where selection does.
type ProbeFunc func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error)

// AdmitFunc adds SSH public keys to the keys a leased machine reached over
// SSH lets in, connecting with identity (a private key file) when set.
type AdmitFunc func(ctx context.Context, target proto.LeaseTarget, identity string, keys []string) error

const admitTimeout = 30 * time.Second

type Config struct {
	StateDir string // leases are persisted under StateDir/leases
	Offers   []Offer
	// Catalog lists further offers, such as a cloud account's instance
	// types with capacity, refreshed every CatalogRefresh (default 2m).
	Catalog        Catalog
	CatalogRefresh time.Duration
	// Version is this errand's version, for providers that install errand.
	Version        string
	MaxLeases      int
	AcquireTimeout time.Duration
	ReleaseTimeout time.Duration
	Probe          ProbeFunc
	AdmitKeys      AdmitFunc
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
	// Requests are the runs this lease was handed while it was launching
	// that have not withdrawn. A request asked again gets the same lease,
	// and a launch every request withdrew from is cancelled. A ready lease
	// is ended only by the idle and lifetime rules (see "Lease guarantees"
	// in docs/DESIGN.md).
	Requests []string `json:"requests,omitempty"`
	// PendingKeys are SSH keys of the owner's devices that asked for the
	// lease after it was launched, for the worker to add to the machine
	// once it is ready. Added keys move to SSHKeys.
	PendingKeys []string `json:"pending_keys,omitempty"`
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
	provider Provider           // the offer's provider when the lease was made
	stop     context.CancelFunc // cancels the running acquire or probe
	wake     chan struct{}
}

type Broker struct {
	cfg    Config
	offers map[string]Offer
	dir    string

	mu     sync.Mutex
	leases map[string]*lease
	closed bool
	// catalog is the latest listing from cfg.Catalog, cheapest first.
	// catalogMu lets one listing run at a time, so an older one cannot
	// land after a newer one.
	catalogMu sync.Mutex
	catalog   []Offer
	catalogAt time.Time
	// catalogTried is when the last listing ended, failed or not; catalogMu
	// guards it.
	catalogTried time.Time
	// listed is closed once the catalog's first listing has finished, or
	// at once without a catalog.
	listed chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New starts a broker and a worker for every lease it recorded before. With
// no offers it leases nothing new but still ends the leases it has.
func New(cfg Config) (*Broker, error) {
	if cfg.Probe == nil || cfg.AdmitKeys == nil {
		return nil, fmt.Errorf("cloud broker needs a runner probe and a way to admit SSH keys")
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
	if cfg.CatalogRefresh <= 0 {
		cfg.CatalogRefresh = 2 * time.Minute
	}
	// Paths kept in lease records, such as the identity a machine is reached
	// with, must not depend on the directory the runner was started from.
	dir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	cfg.StateDir = dir
	b := &Broker{cfg: cfg, offers: map[string]Offer{}, dir: filepath.Join(cfg.StateDir, "leases"), leases: map[string]*lease{}, listed: make(chan struct{})}
	if cfg.Catalog == nil {
		close(b.listed)
	}
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
		b.setUp(o.Provider)
		b.offers[o.Name] = o
	}
	if cfg.Catalog != nil {
		b.setUp(cfg.Catalog)
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
	if cfg.Catalog != nil {
		b.wg.Add(1)
		go b.refreshCatalog()
	}
	return b, nil
}

// setUp hands a provider or catalog what it may need from the broker: a
// directory for files of its own, kept with the leases, and errand's version.
func (b *Broker) setUp(v any) {
	if k, ok := v.(interface{ useStateDir(string) }); ok {
		k.useStateDir(b.cfg.StateDir)
	}
	if k, ok := v.(interface{ useVersion(string) }); ok {
		k.useVersion(b.cfg.Version)
	}
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

// Offers describes what this broker can lease: its configured offers, then
// the catalog's as of its last refresh. Right after the broker starts it
// waits for the catalog's first listing, or until ctx ends, so a client
// asking then is not told there is nothing to lease.
func (b *Broker) Offers(ctx context.Context) []proto.Offer {
	select {
	case <-b.listed:
	case <-ctx.Done():
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []proto.Offer
	for _, o := range b.offersLocked() {
		out = append(out, o.Offer())
	}
	return out
}

// offersLocked is every offer that can be leased from right now.
func (b *Broker) offersLocked() []Offer {
	return append(slices.Clone(b.cfg.Offers), b.catalog...)
}

// offerLocked finds an offer by name among those that can be leased from.
func (b *Broker) offerLocked(name string) (Offer, bool) {
	if o, ok := b.offers[name]; ok {
		return o, true
	}
	for _, o := range b.catalog {
		if o.Name == name {
			return o, true
		}
	}
	return Offer{}, false
}

// Offer is how o is advertised.
func (o *Offer) Offer() proto.Offer {
	return proto.Offer{Name: o.Name, Facts: o.Facts, PricePerHour: o.PricePerHour, IdleTimeoutSec: int64(o.IdleTimeout / time.Second), MaxLifetimeSec: int64(o.MaxLifetime / time.Second)}
}

// Acquire returns the lease an earlier request with the same requestID
// made, the owner's matching ready or launching lease, or a new one from the
// cheapest matching offer. login is the caller's tailnet login, when it has
// one. sshKey is the caller's SSH public key, or empty; a machine reached
// over SSH admits the key of each of the owner's devices that asks for it.
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
	if len(b.offers) == 0 && b.cfg.Catalog == nil {
		return proto.Lease{}, &Error{http.StatusNotFound, "this runner has no cloud offers"}
	}
	// A catalog that may have changed since the client saw it is listed
	// again, so a lease is not made from an offer that is gone.
	b.freshenCatalog()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return proto.Lease{}, &Error{http.StatusServiceUnavailable, "cloud broker is shutting down"}
	}
	if requestID == "" {
		requestID = proto.NewULID()
	}
	var launching *lease
	active := 0
	// A request asked again, after its answer was lost, gets the same lease.
	for _, l := range b.sorted() {
		if l.Owner == owner && slices.Contains(l.Requests, requestID) {
			return l.view(), nil
		}
	}
	// A launching lease handed to another request is waited on by that
	// request too. A ready lease handed out starts a full idle window, so
	// the request has time to submit its work.
	share := func(l *lease) (proto.Lease, error) {
		if l.State == proto.LeaseLaunching && len(l.Requests) >= maxRequests {
			return proto.Lease{}, &Error{http.StatusTooManyRequests, fmt.Sprintf("lease %s already has %d runs waiting for it", l.ID, maxRequests)}
		}
		err := b.applyLocked(l, func(r *record) bool {
			if r.State == proto.LeaseReady {
				r.LastBusy = time.Now()
			} else {
				r.Requests = append(slices.Clone(r.Requests), requestID)
			}
			r.admit(sshKey)
			return true
		}, true)
		if err != nil {
			return proto.Lease{}, &Error{http.StatusInternalServerError, "recording the lease: " + err.Error()}
		}
		b.wakeLocked(l)
		return l.view(), nil
	}
	for _, l := range b.sorted() {
		if l.Active() {
			active++
		}
		// The machine admits the login it was launched for. The login can
		// change while the owner (a tailnet user ID) stays the same. Over
		// SSH, another device of the owner's has its key added.
		if l.Owner != owner || l.Login != login {
			continue
		}
		switch {
		case l.State == proto.LeaseReady && l.Facts != nil && len(q.Missing(*l.Facts)) == 0:
			return share(l)
		case l.State == proto.LeaseLaunching && launching == nil && len(q.Missing(l.offerFacts(b))) == 0:
			launching = l
		}
	}
	if launching != nil {
		return share(launching)
	}
	var offer *Offer
	var reasons []string
	candidates := b.offersLocked()
	for i := range candidates {
		o := &candidates[i]
		missing := q.Missing(o.Facts)
		if len(missing) == 0 && o.Unavailable != "" {
			missing = []string{o.Unavailable}
		}
		if len(missing) == 0 {
			if offer == nil || placement.CheaperOffer(o.Offer(), offer.Offer()) {
				offer = o
			}
			continue
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
	l := &lease{wake: make(chan struct{}, 1), provider: offer.Provider, record: record{
		Owner: owner, Login: login, Requests: []string{requestID}, Release: offer.Provider.ReleaseSpec(), IdleTimeout: offer.IdleTimeout,
		Lease: proto.Lease{ID: proto.NewULID(), Offer: offer.Name, Where: where, SSHKeys: keyList(sshKey), State: proto.LeaseLaunching, CreatedAt: now, ExpiresAt: now.Add(offer.MaxLifetime)},
	}}
	l.addProgress("launching " + offer.Name)
	// Recorded before acquiring, so a crash cannot forget a machine being paid for.
	if err := b.persist(&l.record); err != nil {
		return proto.Lease{}, &Error{http.StatusInternalServerError, "recording lease: " + err.Error()}
	}
	log.Printf("lease %s (%s): launching for where %q", l.ID, l.Offer, where)
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

// Admit has an active lease of the owner's let in another of the owner's
// devices by its SSH key, for a device that names the lease rather than
// asking for a machine. The worker adds the key; the answer lists it in
// SSHKeys once it has.
func (b *Broker) Admit(owner, login, id, sshKey string) (proto.Lease, error) {
	if !ValidSSHPublicKey(sshKey) {
		return proto.Lease{}, &Error{http.StatusBadRequest, "ssh_key is not one SSH public key"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.leases[id]
	if l == nil || l.Owner != owner || l.Login != login || !l.Active() {
		return proto.Lease{}, &Error{http.StatusNotFound, "no active lease of yours has that ID"}
	}
	if err := b.applyLocked(l, func(r *record) bool {
		before := len(r.PendingKeys)
		r.admit(sshKey)
		return len(r.PendingKeys) != before
	}, true); err != nil {
		return proto.Lease{}, &Error{http.StatusInternalServerError, "recording the key: " + err.Error()}
	}
	b.wakeLocked(l)
	return l.view(), nil
}

// Withdraw is a run's answer when it gives up before the lease it was
// handed is ready. A launch every request withdrew from is cancelled. A
// lease that is already ready is left to the idle rule.
func (b *Broker) Withdraw(owner, requestID string) (proto.Lease, error) {
	b.mu.Lock()
	var l *lease
	for _, c := range b.leases {
		if c.Owner == owner && requestID != "" && slices.Contains(c.Requests, requestID) {
			l = c
		}
	}
	b.mu.Unlock()
	if l == nil {
		return proto.Lease{}, &Error{http.StatusNotFound, "no lease was handed to that request"}
	}
	err := b.update(l, func(r *record) bool {
		i := slices.Index(r.Requests, requestID)
		if i < 0 || r.State != proto.LeaseLaunching {
			return false
		}
		r.Requests = slices.Delete(slices.Clone(r.Requests), i, i+1)
		if len(r.Requests) == 0 {
			r.State = proto.LeaseReleasing
			r.addProgress("release requested while launching")
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
		if next.State != l.State && len(next.Progress) > 0 {
			log.Printf("lease %s (%s): %s: %s", l.ID, l.Offer, next.State, next.Progress[len(next.Progress)-1])
		}
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
		log.Printf("lease %s (%s): %s", r.ID, r.Offer, line)
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
	b.mu.Lock()
	if l.State != proto.LeaseLaunching {
		b.mu.Unlock()
		return // released before the acquire started
	}
	// Only this process's leases are launching, and each keeps the provider
	// of the offer it was made from, which a catalog may since have dropped.
	provider, offerName := l.provider, l.Offer
	// The hard stop holds while launching too.
	created := l.CreatedAt
	deadline := earlier(created.Add(b.cfg.AcquireTimeout), l.ExpiresAt)
	ctx, cancel := context.WithDeadline(b.ctx, deadline)
	l.stop = cancel
	id, where, login, sshKey := l.ID, l.Where, l.Login, ""
	if len(l.SSHKeys) > 0 {
		sshKey = l.SSHKeys[0] // the device that asked first
	}
	b.mu.Unlock()
	defer cancel()

	machine, err := provider.Acquire(ctx, AcquireRequest{
		LeaseID: id, Offer: offerName, Where: where, Login: login, SSHKey: sshKey,
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
			err = fmt.Errorf("acquire did not finish within %s", deadline.Sub(created))
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
			r.ReadyAt, r.LastBusy = now, now
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
		if len(r.PendingKeys) > 0 {
			b.admitPending(l, r)
		}
		busy := time.Now().Before(r.ExpiresAt) && b.busy(l, r)
		// The probe took time, and requests may have changed the record
		// meanwhile, so the decision is made against the record as it is
		// now. The machine is destroyed only once the release is recorded;
		// otherwise a restart would hand out a lease whose machine is gone.
		released := false
		err := b.update(l, func(r *record) bool {
			if r.State != proto.LeaseReady {
				return false
			}
			now := time.Now()
			reason := r.releaseReason(now, busy)
			if reason == "" {
				if !busy {
					return false
				}
				r.LastBusy = now
				return true
			}
			r.State = proto.LeaseReleasing
			r.addProgress("releasing: " + reason)
			released = true
			return true
		})
		if released && err == nil {
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

// admitPending adds the keys of the owner's devices that asked for a ready
// lease after its launch to the machine, so they can reach it too. A key
// that could not be added stays pending for the next check.
func (b *Broker) admitPending(l *lease, r record) {
	keys := slices.Clone(r.PendingKeys)
	if r.Target.SSH != "" {
		ctx, cancel, ok := b.readyCall(l, r, admitTimeout)
		if !ok {
			cancel()
			return
		}
		err := b.cfg.AdmitKeys(ctx, *r.Target, r.Identity, keys)
		cancel()
		if err != nil {
			if b.stopped(l) {
				return // released meanwhile; nothing to retry
			}
			b.note(l, "adding another device's SSH key failed, retrying: "+err.Error())
			return
		}
	}
	_ = b.update(l, func(r *record) bool {
		if r.State != proto.LeaseReady {
			return false
		}
		for _, k := range keys {
			r.PendingKeys = slices.DeleteFunc(slices.Clone(r.PendingKeys), func(p string) bool { return p == k })
			if r.Target.SSH != "" && !slices.Contains(r.SSHKeys, k) {
				r.SSHKeys = append(slices.Clone(r.SSHKeys), k)
			}
		}
		if r.Target.SSH != "" {
			r.addProgress(fmt.Sprintf("admitted %d more of your devices", len(keys)))
		}
		return true
	})
}

// releaseReason says why a ready lease should be released now, or nothing.
// busy is whether its machine was just seen with work.
func (r *record) releaseReason(now time.Time, busy bool) string {
	switch {
	case !now.Before(r.ExpiresAt):
		return fmt.Sprintf("max lifetime of %s reached", r.ExpiresAt.Sub(r.CreatedAt).Round(time.Second))
	case busy:
		return ""
	case !now.Before(r.LastBusy.Add(r.IdleTimeout)):
		return fmt.Sprintf("idle for %s", r.IdleTimeout)
	}
	return ""
}

// readyCall bounds a worker's call to a ready lease's machine by timeout and
// the lease's hard stop, and lets a release cancel it rather than wait for
// it. ok is false when the lease is no longer ready.
func (b *Broker) readyCall(l *lease, r record, timeout time.Duration) (ctx context.Context, cancel context.CancelFunc, ok bool) {
	ctx, cancel = context.WithDeadline(b.ctx, earlier(time.Now().Add(timeout), r.ExpiresAt))
	b.mu.Lock()
	defer b.mu.Unlock()
	l.stop = cancel
	return ctx, cancel, l.State == proto.LeaseReady
}

// stopped reports whether a lease is no longer ready.
func (b *Broker) stopped(l *lease) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return l.State != proto.LeaseReady
}

// busy reports whether the machine has work, asking no later than the
// lease's hard stop.
func (b *Broker) busy(l *lease, r record) bool {
	ctx, cancel, ok := b.readyCall(l, r, 5*time.Second)
	defer cancel()
	if !ok {
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
		r.ReleasedAt = time.Now()
		r.addProgress("released")
		return true
	})
}

// releaser is the provider that releases r's machine: its offer's, while
// that offer still releases the way r was made, or else one built from the
// release settings r recorded.
func (b *Broker) releaser(r record) (Provider, error) {
	b.mu.Lock()
	o, ok := b.offerLocked(r.Offer)
	b.mu.Unlock()
	if ok && o.Provider.ReleaseSpec().equal(r.Release) {
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
			v := l.view()
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

// maxRequests bounds the runs waiting for one launching lease.
const maxRequests = 32

func (l *lease) view() proto.Lease {
	v := l.Lease
	v.Progress = append([]string(nil), l.Progress...)
	// The idle deadline follows from when the lease was last busy, which a
	// restart resets, so it is derived here rather than stored.
	v.IdleUntil = time.Time{}
	if v.State == proto.LeaseReady {
		v.IdleUntil = l.LastBusy.Add(l.IdleTimeout)
	}
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

// admit has the machine let in another of the owner's devices by its SSH
// key: at once if the machine is not reached over SSH or already admits
// it, otherwise once the worker has added it.
func (r *record) admit(sshKey string) {
	if sshKey == "" || slices.Contains(r.SSHKeys, sshKey) || slices.Contains(r.PendingKeys, sshKey) {
		return
	}
	if r.Target != nil && r.Target.SSH == "" {
		return
	}
	r.PendingKeys = append(slices.Clone(r.PendingKeys), sshKey)
}

func keyList(sshKey string) []string {
	if sshKey == "" {
		return nil
	}
	return []string{sshKey}
}

// wakeLocked has a lease's worker look at it again.
func (b *Broker) wakeLocked(l *lease) {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}
