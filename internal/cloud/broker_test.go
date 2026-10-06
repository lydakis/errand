package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

type fakeMachine struct {
	gpus    []proto.GPU
	running atomic.Int32
	down    atomic.Bool
	probes  atomic.Int32

	mu       sync.Mutex
	admitted []string // keys added after launch
	held     bool     // refusing new jobs
	refuse   atomic.Bool
}

func (m *fakeMachine) admit(_ context.Context, target proto.LeaseTarget, _ string, keys []string) error {
	if target.SSH == "" {
		return fmt.Errorf("admitting keys over %+v", target)
	}
	if m.refuse.Load() {
		return errors.New("permission denied")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.admitted = append(m.admitted, keys...)
	return nil
}

func (m *fakeMachine) probe(_ context.Context, target proto.LeaseTarget, _, _ string) (proto.Info, error) {
	m.probes.Add(1)
	if target.URL != "http://box:7443" && target.SSH != "ubuntu@box" {
		return proto.Info{}, fmt.Errorf("unexpected target %+v", target)
	}
	if m.down.Load() {
		return proto.Info{}, errors.New("connection refused")
	}
	m.mu.Lock()
	gpus := m.gpus
	m.mu.Unlock()
	return proto.Info{MaxJobs: 1, RunningJobs: int(m.running.Load()), Facts: proto.Facts{OS: "linux", Arch: "amd64", GPUs: gpus}}, nil
}

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

type harness struct {
	dir, log string
	machine  *fakeMachine
	cfg      Config
}

func newHarness(t *testing.T, acquireBody string) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("provider scripts are POSIX shell")
	}
	h := &harness{dir: t.TempDir(), machine: &fakeMachine{gpus: []proto.GPU{{Name: "NVIDIA H100 80GB HBM3", MemoryMiB: 81559}}}}
	h.log = filepath.Join(h.dir, "release.log")
	acquire := script(t, h.dir, "acquire.sh", acquireBody)
	release := script(t, h.dir, "release.sh", fmt.Sprintf("echo \"$ERRAND_LEASE_ID $ERRAND_OFFER $ERRAND_LEASE_STATE\" >> %q\n", h.log))
	h.cfg = Config{
		StateDir: filepath.Join(h.dir, "state"),
		Offers: []Offer{{
			Name:        "h100",
			Facts:       proto.Facts{OS: "linux", Arch: "amd64", GPUs: []proto.GPU{{Name: "H100 80GB", MemoryMiB: 80 << 10}}},
			Provider:    CommandProvider{AcquireCommand: []string{acquire}, ReleaseCommand: []string{release}},
			IdleTimeout: time.Hour, MaxLifetime: time.Hour,
		}},
		AcquireTimeout: 5 * time.Second,
		Probe:          h.machine.probe,
		AdmitKeys:      h.machine.admit,
		Drain:          h.machine.drain,
		ReadyPoll:      10 * time.Millisecond,
		IdlePoll:       10 * time.Millisecond,
	}
	return h
}

const okAcquire = `echo "creating instance for $ERRAND_LEASE_ID ($ERRAND_LEASE_WHERE)" >&2
echo "noise on stdout"
echo '{"url":"http://box:7443","instance":"i-123"}'
`

func (h *harness) start(t *testing.T) *Broker {
	t.Helper()
	b, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func (h *harness) releases() string {
	data, _ := os.ReadFile(h.log)
	return string(data)
}

// wake makes a lease's worker look at the lease again now.
func wake(b *Broker, id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case b.leases[id].wake <- struct{}{}:
	default:
	}
}

func waitState(t *testing.T, b *Broker, owner, id, state string) proto.Lease {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, ok := b.Get(owner, id)
		if !ok {
			t.Fatalf("lease %s vanished", id)
		}
		if l.State == state {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease state %s, want %s: %+v", l.State, state, l)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A machine admits the login it was launched for, so a renamed tailnet user
// gets a new lease rather than one that would refuse them.
func TestLeaseNotReusedAcrossLogins(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, err := b.Acquire("42", "old@github", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "42", l.ID, proto.LeaseReady)
	if same, err := b.Acquire("42", "old@github", "gpu", "", ""); err != nil || same.ID != l.ID {
		t.Fatalf("same login must reuse: %+v %v", same, err)
	}
	if renamed, err := b.Acquire("42", "new@github", "gpu", "", ""); err != nil || renamed.ID == l.ID {
		t.Fatalf("renamed login must not reuse: %+v %v", renamed, err)
	}
}

// A machine reached over SSH admits only the key of the client that asked
// for it, so another of the owner's machines gets a lease of its own, and
// the provider is told which key to admit.
// A client that lost the answer asks again with the same request ID and
// gets the lease that request started, not a second machine.
func TestRepeatedRequestReturnsItsLease(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.MaxLeases = 3
	b := h.start(t)
	request := proto.NewULID()
	l, err := b.Acquire("george", "", "gpu", "", request)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Release("george", l.ID)
	if again, err := b.Acquire("george", "", "gpu", "", request); err != nil || again.ID != l.ID {
		t.Fatalf("repeated request: %+v %v", again, err)
	}
	if other, err := b.Acquire("someone", "", "gpu", "", request); err != nil || other.ID == l.ID {
		t.Fatalf("another owner's request: %+v %v", other, err)
	}
}

// A release request does not wait on a probe of the machine.
func TestReleaseCancelsHangingProbe(t *testing.T) {
	h := newHarness(t, okAcquire)
	var hang atomic.Bool
	probe := h.cfg.Probe
	h.cfg.Probe = func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error) {
		if hang.Load() {
			<-ctx.Done()
			return proto.Info{}, ctx.Err()
		}
		return probe(ctx, target, identity, where)
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	hang.Store(true)
	time.Sleep(50 * time.Millisecond) // the worker is now inside a probe
	start := time.Now()
	if _, err := b.Release("george", l.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("release waited %s on a probe", took)
	}
}

// A machine reached over the tailnet admits the owner whatever key their
// workstation sent, so its lease is reused from any of them.
func TestTailnetLeaseReusedAcrossClientKeys(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, err := b.Acquire("george", "george@github", "gpu", "ssh-ed25519 bWFj errand", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	if same, err := b.Acquire("george", "george@github", "gpu", "ssh-ed25519 bWluaQ== errand", ""); err != nil || same.ID != l.ID {
		t.Fatalf("an HTTP lease must be reused across keys: %+v %v", same, err)
	}
}

func TestLeaseCarriesClientKey(t *testing.T) {
	h := newHarness(t, `echo "key $ERRAND_LEASE_SSH_KEY" >&2
echo '{"ssh":"ubuntu@box"}'
`)
	h.cfg.MaxLeases = 3
	b := h.start(t)
	const mac, mini = "ssh-ed25519 bWFj errand", "ssh-ed25519 bWluaQ== errand"
	l, err := b.Acquire("george", "", "gpu", mac, "")
	if err != nil {
		t.Fatal(err)
	}
	ready := waitState(t, b, "george", l.ID, proto.LeaseReady)
	if !slices.Contains(ready.Progress, "key "+mac) {
		t.Fatalf("provider was not given the key: %q", ready.Progress)
	}
	if same, err := b.Acquire("george", "", "gpu", mac, ""); err != nil || same.ID != l.ID {
		t.Fatalf("same key must reuse: %+v %v", same, err)
	}
	for _, bad := range []string{"AAAA george@mac", mac + "\n" + mini, `command="sh" ` + mac} {
		var refused *Error
		if _, err := b.Acquire("george", "", "gpu", bad, ""); !errors.As(err, &refused) || refused.Status != http.StatusBadRequest {
			t.Errorf("key %q: %v", bad, err)
		}
	}
}

// A lease is the owner's, not one device's: another device of the owner's
// that asks for it, or names it, reuses it once the worker has added its key
// to the machine. A key that could not be added is retried.
func TestLeaseAdmitsOwnersOtherDevices(t *testing.T) {
	h := newHarness(t, `echo '{"ssh":"ubuntu@box"}'`)
	h.cfg.IdlePoll = 20 * time.Millisecond
	b := h.start(t)
	const mac, mini, air = "ssh-ed25519 bWFj errand", "ssh-ed25519 bWluaQ== errand", "ssh-ed25519 YWly errand"
	l, err := b.Acquire("george", "", "gpu", mac, "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	h.machine.refuse.Store(true)
	other, err := b.Acquire("george", "", "gpu", mini, "")
	if err != nil || other.ID != l.ID || slices.Contains(other.SSHKeys, mini) {
		t.Fatalf("another device must reuse the lease once let in: %+v %v", other, err)
	}
	time.Sleep(100 * time.Millisecond)
	if got, _ := b.Get("george", l.ID); slices.Contains(got.SSHKeys, mini) {
		t.Fatalf("a key the machine refused is listed: %v", got.SSHKeys)
	}
	h.machine.refuse.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for got, _ := b.Get("george", l.ID); !slices.Contains(got.SSHKeys, mini); got, _ = b.Get("george", l.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("key never added: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := b.Admit("george", "", l.ID, air); err != nil {
		t.Fatal(err)
	}
	for got, _ := b.Get("george", l.ID); !slices.Contains(got.SSHKeys, air); got, _ = b.Get("george", l.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("named lease never let the device in: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.machine.mu.Lock()
	admitted := slices.Clone(h.machine.admitted)
	h.machine.mu.Unlock()
	if !slices.Equal(admitted, []string{mini, air}) {
		t.Fatalf("machine was asked to add %q", admitted)
	}
	if _, err := b.Admit("someone", "", l.ID, air); err == nil {
		t.Fatal("let another owner's device in")
	}
	if _, err := b.Admit("george", "", l.ID, "AAAA"); err == nil {
		t.Fatal("admitted a malformed key")
	}
}

// Releasing a lease does not wait for a device's admission still talking
// to the machine.
func TestReleaseCancelsAdmission(t *testing.T) {
	h := newHarness(t, `echo '{"ssh":"ubuntu@box"}'`)
	admitting := make(chan struct{})
	h.cfg.AdmitKeys = func(ctx context.Context, _ proto.LeaseTarget, _ string, _ []string) error {
		close(admitting)
		<-ctx.Done()
		return ctx.Err()
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "ssh-ed25519 bWFj errand", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	if _, err := b.Admit("george", "", l.ID, "ssh-ed25519 bWluaQ== errand"); err != nil {
		t.Fatal(err)
	}
	<-admitting
	start := time.Now()
	if _, err := b.Release("george", l.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("release waited %s for the admission", waited)
	}
}

func TestLeaseLifecycle(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu=h100", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != proto.LeaseLaunching || l.Offer != "h100" {
		t.Fatalf("lease %+v", l)
	}
	again, err := b.Acquire("george", "", "gpu=h100,vram>=80", "", "")
	if err != nil || again.ID != l.ID {
		t.Fatalf("a launching match must be shared: %+v %v", again, err)
	}
	ready := waitState(t, b, "george", l.ID, proto.LeaseReady)
	if ready.Target == nil || ready.Target.URL != "http://box:7443" || ready.Facts == nil || len(ready.Facts.GPUs) != 1 {
		t.Fatalf("ready lease %+v", ready)
	}
	if !strings.Contains(strings.Join(ready.Progress, "\n"), "creating instance for "+l.ID+" (gpu=h100)") {
		t.Fatalf("acquire stderr was not relayed: %q", ready.Progress)
	}
	if reused, err := b.Acquire("george", "", "gpu", "", ""); err != nil || reused.ID != l.ID {
		t.Fatalf("ready lease must be reused: %+v %v", reused, err)
	}
	if _, ok := b.Get("someone-else", l.ID); ok {
		t.Fatal("leases are private to their owner")
	}
	if active := b.Active("george"); len(active) != 1 || active[0].ID != l.ID || active[0].Target == nil || active[0].Progress != nil {
		t.Fatalf("active leases %+v", active)
	}
	if _, err := b.Release("someone-else", l.ID); err == nil {
		t.Fatal("released another owner's lease")
	}
	if _, err := b.Release("george", l.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
	if got := h.releases(); got != l.ID+` h100 {"url":"http://box:7443","instance":"i-123"}`+"\n" {
		t.Fatalf("release saw %q", got)
	}
	if active := b.Active("george"); len(active) != 0 {
		t.Fatalf("released lease still active: %+v", active)
	}
}

func TestAcquireRefusals(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.MaxLeases = 1
	b := h.start(t)
	for where, status := range map[string]int{"*": http.StatusBadRequest, "nonsense": http.StatusBadRequest, "gpu=a100": http.StatusPreconditionFailed, "gpus>=2": http.StatusPreconditionFailed} {
		_, err := b.Acquire("george", "", where, "", "")
		var e *Error
		if !errors.As(err, &e) || e.Status != status {
			t.Errorf("%q: %v", where, err)
		}
	}
	if _, err := b.Acquire("george", "", "gpu", "", ""); err != nil {
		t.Fatal(err)
	}
	_, err := b.Acquire("other", "", "gpu", "", "")
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusTooManyRequests {
		t.Fatalf("max_leases: %v", err)
	}
}

// Lease guarantees 2 and 7: idle and lifetime end a ready lease.
func TestIdleAndExpiredLeasesRelease(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.Offers[0].IdleTimeout = 150 * time.Millisecond
	h.machine.running.Store(1)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	time.Sleep(400 * time.Millisecond)
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReady {
		t.Fatalf("busy lease was released: %+v", got)
	}
	h.machine.running.Store(0)
	released := waitState(t, b, "george", l.ID, proto.LeaseReleased)
	if !strings.Contains(strings.Join(released.Progress, "\n"), "releasing: idle for 150ms") {
		t.Fatalf("progress %q", released.Progress)
	}

	h2 := newHarness(t, okAcquire)
	h2.cfg.Offers[0].MaxLifetime = 200 * time.Millisecond
	h2.machine.running.Store(1)
	b2 := h2.start(t)
	l2, err := b2.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	expired := waitState(t, b2, "george", l2.ID, proto.LeaseReleased)
	if !strings.Contains(strings.Join(expired.Progress, "\n"), "max lifetime") {
		t.Fatalf("a busy lease must still stop at its max lifetime: %q", expired.Progress)
	}
}

// Lease guarantees 2 and 3: a run that gives up while its lease launches
// withdraws, and the launch stops once no run is waiting for it. A ready
// lease is left to the idle rule.
func TestWithdrawCancelsOnlyAnUnwantedLaunch(t *testing.T) {
	h := newHarness(t, "echo booting >&2\nexec sleep 30\n")
	h.cfg.MaxLeases = 3
	b := h.start(t)
	first, second := proto.NewULID(), proto.NewULID()
	l, err := b.Acquire("george", "", "gpu", "", first)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := b.Acquire("george", "", "gpu", "", second); err != nil || again.ID != l.ID {
		t.Fatalf("second request: %+v %v", again, err)
	}
	if _, err := b.Withdraw("someone", second); err == nil {
		t.Fatal("withdrew another owner's request")
	}
	if w, err := b.Withdraw("george", first); err != nil || w.State != proto.LeaseLaunching {
		t.Fatalf("withdrawing a launch another run still waits for: %+v %v", w, err)
	}
	if _, err := b.Withdraw("george", proto.NewULID()); err == nil {
		t.Fatal("withdrew a request the lease was never handed")
	}
	if w, err := b.Withdraw("george", second); err != nil || w.State == proto.LeaseLaunching {
		t.Fatalf("withdrawing the last request: %+v %v", w, err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReleased)

	h = newHarness(t, okAcquire)
	h.cfg.Offers[0].IdleTimeout = time.Hour
	b = h.start(t)
	request := proto.NewULID()
	l, err = b.Acquire("george", "", "gpu", "", request)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	if w, err := b.Withdraw("george", request); err != nil || w.State != proto.LeaseReady {
		t.Fatalf("withdrawing from a ready lease: %+v %v", w, err)
	}
	wake(b, l.ID)
	time.Sleep(100 * time.Millisecond)
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReady {
		t.Fatalf("an idle ready lease ended before its idle timeout: %+v", got)
	}
}

func TestReleaseWhileLaunchingStopsAcquire(t *testing.T) {
	h := newHarness(t, "echo booting >&2\nexec sleep 30\n")
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Release("george", l.ID); err != nil {
		t.Fatal(err)
	}
	released := waitState(t, b, "george", l.ID, proto.LeaseReleased)
	if released.Error != "" || h.releases() != l.ID+" h100 \n" {
		t.Fatalf("lease %+v releases %q", released, h.releases())
	}
}

// A child the acquire command left behind can hold its stderr open; the
// release still goes ahead once the command is stopped.
func TestReleaseWhileLaunchingOutlivesStderrHolder(t *testing.T) {
	h := newHarness(t, "echo booting >&2\nsleep 30 &\nexec sleep 30\n")
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitProgress(t, b, l.ID, "booting")
	start := time.Now()
	if _, err := b.Release("george", l.ID); err != nil {
		t.Fatal(err)
	}
	for {
		if got, _ := b.Get("george", l.ID); got.State == proto.LeaseReleased {
			break
		}
		if time.Since(start) > 15*time.Second {
			t.Fatal("release waited on the acquire command's stderr")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFailedLaunchesAreReleased(t *testing.T) {
	h := newHarness(t, "echo 'out of capacity' >&2\nexit 3\n")
	b := h.start(t)
	l, _ := b.Acquire("george", "", "gpu", "", "")
	failed := waitState(t, b, "george", l.ID, proto.LeaseFailed)
	if !strings.Contains(failed.Error, "acquire command failed") || !strings.Contains(h.releases(), l.ID) {
		t.Fatalf("lease %+v releases %q", failed, h.releases())
	}

	// A machine that never matches the requirements is not handed out.
	h2 := newHarness(t, okAcquire)
	h2.machine.gpus = nil
	// Long enough for the acquire command itself on a loaded machine; the
	// lease fails when it runs out waiting for a matching machine.
	h2.cfg.AcquireTimeout = 2 * time.Second
	b2 := h2.start(t)
	l2, _ := b2.Acquire("george", "", "gpu", "", "")
	failed = waitState(t, b2, "george", l2.ID, proto.LeaseFailed)
	if !strings.Contains(failed.Error, "requires 1 GPU (has none)") {
		t.Fatalf("lease %+v", failed)
	}
}

// Lease guarantees 1 and 2: a launch a restart interrupted is released.
func TestRestartRecoversLeases(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, _ := b.Acquire("george", "", "gpu", "", "")
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Close()

	// A launch interrupted by a crash is released on the next start.
	crashed := record{Owner: "george", Release: h.cfg.Offers[0].Provider.ReleaseSpec(), Lease: proto.Lease{ID: proto.NewULID(), Offer: "h100", Where: "gpu", State: proto.LeaseLaunching, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}
	data, _ := json.Marshal(crashed)
	if err := os.WriteFile(filepath.Join(h.cfg.StateDir, "leases", crashed.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	restarted := time.Now()
	b2 := h.start(t)
	// The ready lease gets a full idle window, and says so.
	if got, ok := b2.Get("george", l.ID); !ok || got.State != proto.LeaseReady || got.IdleUntil.Before(restarted.Add(h.cfg.Offers[0].IdleTimeout)) {
		t.Fatalf("ready lease after restart: %+v", got)
	}
	// Runners list active leases in /v0/info, with the same deadline.
	for _, a := range b2.Active("george") {
		if a.ID == l.ID && a.IdleUntil.IsZero() {
			t.Fatalf("active lease without its idle deadline: %+v", a)
		}
	}
	failed := waitState(t, b2, "george", crashed.ID, proto.LeaseFailed)
	if !strings.Contains(failed.Error, "restarted") || !strings.Contains(h.releases(), crashed.ID) {
		t.Fatalf("crashed launch %+v releases %q", failed, h.releases())
	}
}

func TestReleaseRetriesUntilItSucceeds(t *testing.T) {
	h := newHarness(t, okAcquire)
	marker := filepath.Join(h.dir, "allow-release")
	h.cfg.Offers[0].Provider = CommandProvider{AcquireCommand: h.cfg.Offers[0].Provider.(CommandProvider).AcquireCommand, ReleaseCommand: []string{script(t, h.dir, "flaky.sh", fmt.Sprintf("[ -e %q ] || { echo 'api 503' >&2; exit 1; }\necho \"$ERRAND_LEASE_ID\" >> %q\n", marker, h.log))}}
	b := h.start(t)
	l, _ := b.Acquire("george", "", "gpu", "", "")
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Release("george", l.ID)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got, _ := b.Get("george", l.ID)
		if got.State != proto.LeaseReleasing {
			t.Fatalf("lease %+v", got)
		}
		if strings.Contains(strings.Join(got.Progress, "\n"), "release failed: exit status 1 api 503; retrying") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("release never failed: %+v", got)
		}
	}
	os.WriteFile(marker, nil, 0600)
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
}

// A lease is released the way it was made, so changing or removing its
// offer, or every offer, never strands its machine.
func TestLeaseReleasesAsItWasMade(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, _ := b.Acquire("george", "", "gpu", "", "")
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Close()

	other := script(t, h.dir, "other.sh", fmt.Sprintf("echo other >> %q\n", h.log))
	changed := h.cfg
	changed.Offers = slices.Clone(h.cfg.Offers)
	changed.Offers[0].Provider = CommandProvider{AcquireCommand: []string{other}, ReleaseCommand: []string{other}}
	b2, err := New(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b2.Release("george", l.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, b2, "george", l.ID, proto.LeaseReleased)
	b2.Close()
	if got := h.releases(); !strings.HasPrefix(got, l.ID+" h100 ") || strings.Contains(got, "other") {
		t.Fatalf("released with %q", got)
	}

	// With no offers left, a ready lease still ends when it goes idle.
	h2 := newHarness(t, okAcquire)
	h2.cfg.Offers[0].IdleTimeout = 100 * time.Millisecond
	h2.machine.running.Store(1)
	b3 := h2.start(t)
	l2, _ := b3.Acquire("george", "", "gpu", "", "")
	waitState(t, b3, "george", l2.ID, proto.LeaseReady)
	b3.Close()
	gone := h2.cfg
	gone.Offers = nil
	b4, err := New(gone)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b4.Close)
	if len(b4.Offers()) != 0 {
		t.Fatal("offers without configuration")
	}
	var refused *Error
	if _, err := b4.Acquire("george", "", "gpu", "", ""); !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
		t.Fatalf("leased without offers: %v", err)
	}
	h2.machine.running.Store(0)
	waitState(t, b4, "george", l2.ID, proto.LeaseReleased)
	if !strings.Contains(h2.releases(), l2.ID) {
		t.Fatalf("releases %q", h2.releases())
	}
}

// A record that says nothing of how to release it keeps saying so rather
// than being released some other way.
func TestRecordWithoutReleaseSpec(t *testing.T) {
	h := newHarness(t, okAcquire)
	unnamed := record{Owner: "george", Lease: proto.Lease{ID: proto.NewULID(), Offer: "h100", Where: "gpu", State: proto.LeaseReleasing, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}
	data, _ := json.Marshal(unnamed)
	os.MkdirAll(filepath.Join(h.cfg.StateDir, "leases"), 0700)
	os.WriteFile(filepath.Join(h.cfg.StateDir, "leases", unnamed.ID+".json"), data, 0600)
	b := h.start(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := b.Get("george", unnamed.ID)
		if strings.Contains(strings.Join(got.Progress, "\n"), "no way to release its machines is set") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease %+v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.releases() != "" {
		t.Fatalf("released with the offer's command: %q", h.releases())
	}
}

// max_lifetime is a hard stop even for a launch that never finishes.
func TestLaunchStopsAtMaxLifetime(t *testing.T) {
	h := newHarness(t, "exec sleep 30\n")
	h.cfg.Offers[0].MaxLifetime = 200 * time.Millisecond
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ended := waitState(t, b, "george", l.ID, proto.LeaseFailed)
	if !strings.Contains(ended.Error, "within 200ms") || !strings.Contains(h.releases(), l.ID) {
		t.Fatalf("lease %+v, releases %q", ended, h.releases())
	}
}

// A restart releases a lease whose record still says launching, so a lease
// whose ready state could not be recorded is released, never handed out.
func TestLeaseNotReadyUnlessRecorded(t *testing.T) {
	h := newHarness(t, okAcquire)
	probe := h.cfg.Probe
	h.cfg.Probe = func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error) {
		// Put a directory where the only lease's record goes, so every later
		// write of the record fails, root or not.
		records, _ := filepath.Glob(filepath.Join(h.cfg.StateDir, "leases", "*.json"))
		for _, record := range records {
			if err := os.Remove(record); err == nil {
				os.MkdirAll(filepath.Join(record, "blocked"), 0700)
			}
		}
		return probe(ctx, target, identity, where)
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ended := waitState(t, b, "george", l.ID, proto.LeaseFailed)
	if ended.Target != nil || ended.ReadyAt != (time.Time{}) || !strings.Contains(ended.Error, "recording the ready lease") || !strings.Contains(h.releases(), l.ID) {
		t.Fatalf("lease %+v, releases %q", ended, h.releases())
	}
}

// Ended leases are forgotten after a week by a running broker too.
func TestOldEndedLeasesAreForgotten(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	if _, err := b.Release("george", l.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
	b.mu.Lock()
	b.leases[l.ID].ReleasedAt = time.Now().Add(-endedLeaseHistory - time.Minute)
	b.mu.Unlock()
	if listed := b.List("george"); len(listed) != 0 {
		t.Fatalf("old ended lease still listed: %+v", listed)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.StateDir, "leases", l.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old ended lease still recorded: %v", err)
	}
}

// A release is acknowledged only once it is recorded; otherwise a restart
// would bring the lease back as ready after the client was told it ended.
func TestReleaseNotAcknowledgedUnlessRecorded(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.IdlePoll = time.Hour
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	record := filepath.Join(h.cfg.StateDir, "leases", l.ID+".json")
	saved, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(record, "blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	var refused *Error
	if _, err := b.Release("george", l.ID); !errors.As(err, &refused) || refused.Status != http.StatusInternalServerError {
		t.Fatalf("unrecorded release: %v", err)
	}
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReady || strings.Contains(strings.Join(got.Progress, "\n"), "release requested") || h.releases() != "" {
		t.Fatalf("lease changed by an unrecorded release: %+v, releases %q", got, h.releases())
	}
	os.RemoveAll(record)
	if err := os.WriteFile(record, saved, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Release("george", l.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
}

// The hard stop does not wait on a probe, so a machine that hangs probes
// still ends at its max lifetime.
func TestExpiredLeaseReleasesWithoutProbe(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.Offers[0].MaxLifetime = time.Second
	var hang atomic.Bool
	probe := h.cfg.Probe
	h.cfg.Probe = func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error) {
		if hang.Load() {
			<-ctx.Done()
			return proto.Info{}, ctx.Err()
		}
		return probe(ctx, target, identity, where)
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	hang.Store(true)
	released := waitState(t, b, "george", l.ID, proto.LeaseReleased)
	if late := released.ReleasedAt.Sub(released.ExpiresAt); late > 2*time.Second {
		t.Fatalf("released %s after the hard stop", late)
	}
}

// The idle check destroys a machine only once its release is recorded.
func TestAutomaticReleaseOnlyOnceRecorded(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.IdlePoll = time.Hour
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	record := filepath.Join(h.cfg.StateDir, "leases", l.ID+".json")
	saved, _ := os.ReadFile(record)
	os.Remove(record)
	os.MkdirAll(filepath.Join(record, "blocked"), 0700)
	b.mu.Lock()
	b.leases[l.ID].ExpiresAt = time.Now().Add(-time.Second)
	b.mu.Unlock()
	wake(b, l.ID)
	time.Sleep(100 * time.Millisecond)
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReady || h.releases() != "" {
		t.Fatalf("released without a record: %+v, releases %q", got, h.releases())
	}
	os.RemoveAll(record)
	os.WriteFile(record, saved, 0600)
	wake(b, l.ID)
	if got := waitState(t, b, "george", l.ID, proto.LeaseReleased); !strings.Contains(h.releases(), l.ID) {
		t.Fatalf("expired lease %+v, releases %q", got, h.releases())
	}
}

// A provider that writes an endless progress line still finishes.
func TestAcquireSurvivesHugeProgressLine(t *testing.T) {
	h := newHarness(t, `head -c 200000 /dev/zero | tr '\0' x >&2
echo >&2
echo "after the long line" >&2
echo '{"url":"http://box:7443"}'
`)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
}

// Files a lease record names stay valid whatever directory the runner is
// later started from.
func TestStateDirIsAbsolute(t *testing.T) {
	h := newHarness(t, okAcquire)
	t.Chdir(h.dir)
	lambda := &LambdaProvider{InstanceType: "t", APIKeyFile: "/a"}
	h.cfg.StateDir = "state"
	h.cfg.Offers = append(h.cfg.Offers, Offer{Name: "lambda", Provider: lambda, IdleTimeout: time.Hour, MaxLifetime: time.Hour})
	h.start(t)
	if want := filepath.Join(h.dir, "state", "lambda"); lambda.KeyDir != want {
		t.Fatalf("key dir %q, want %q", lambda.KeyDir, want)
	}
}

// Acquire's stdout is bounded: one that prints more is a failed launch,
// however the output reaches errand.
func TestAcquireRefusesOversizedOutput(t *testing.T) {
	h := newHarness(t, `head -c 100000 /dev/zero | tr '\0' x
echo
echo '{"url":"http://box:7443"}'
`)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitState(t, b, "george", l.ID, proto.LeaseFailed); !strings.Contains(got.Error, "more than 16384 bytes") {
		t.Fatalf("lease %+v", got)
	}
}

// Lease guarantees 4 and 5: an idle probe takes time, and a request may be
// handed the lease while it runs. The probe's answer cannot then release the
// lease: the reason to release is checked against the record as it is when
// the release is made.
func TestReuseDuringIdleProbeKeepsLease(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.IdlePoll = time.Hour // probe only when woken
	h.cfg.Offers[0].IdleTimeout = 200 * time.Millisecond
	// Once the lease is ready, each idle probe waits for the test.
	var hold atomic.Bool
	probing, answer := make(chan struct{}), make(chan struct{})
	h.cfg.Probe = func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error) {
		if hold.Load() {
			probing <- struct{}{}
			<-answer
		}
		return h.machine.probe(ctx, target, identity, where)
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", proto.NewULID())
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	hold.Store(true)
	wake(b, l.ID)
	<-probing                          // an idle probe, which will find the machine idle
	time.Sleep(300 * time.Millisecond) // past the idle deadline
	if again, err := b.Acquire("george", "", "gpu", "", proto.NewULID()); err != nil || again.ID != l.ID {
		t.Fatalf("reuse: %+v %v", again, err)
	}
	hold.Store(false)
	answer <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReady {
		t.Fatalf("a lease handed out during the probe was released: %+v", got)
	}
}

// A ready lease can be handed to any number of runs: none of them is
// recorded, so none can turn later runs away.
func TestReadyLeaseReuseIsUnbounded(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.Offers[0].IdleTimeout = time.Hour
	b := h.start(t)
	l, _ := b.Acquire("george", "", "gpu", "", proto.NewULID())
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	for i := range maxRequests + 2 {
		if again, err := b.Acquire("george", "", "gpu", "", proto.NewULID()); err != nil || again.ID != l.ID {
			t.Fatalf("reuse %d: %+v %v", i, again, err)
		}
	}
}

// A launching lease takes a bounded number of runs waiting for it.
func TestLaunchingLeaseRequestsAreBounded(t *testing.T) {
	h := newHarness(t, "echo booting >&2\nexec sleep 30\n")
	b := h.start(t)
	for i := range maxRequests {
		if _, err := b.Acquire("george", "", "gpu", "", proto.NewULID()); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	_, err := b.Acquire("george", "", "gpu", "", proto.NewULID())
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusTooManyRequests {
		t.Fatalf("request past the bound: %v", err)
	}
}

// A cloud peer leases its cheapest matching offer, priced before unpriced,
// the same order clients rank cloud peers' offers in.
func TestAcquireTakesCheapestMatchingOffer(t *testing.T) {
	h := newHarness(t, okAcquire)
	base := h.cfg.Offers[0]
	pool, dear, cheap := base, base, base
	pool.Name, dear.Name, cheap.Name = "pool", "dear", "cheap"
	dear.PricePerHour, cheap.PricePerHour = 3.5, 1.25
	h.cfg.Offers = []Offer{pool, dear, cheap}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", proto.NewULID())
	if err != nil || l.Offer != "cheap" {
		t.Fatalf("leased %q: %v", l.Offer, err)
	}
}

func waitProgress(t *testing.T, b *Broker, id, line string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if l, _ := b.Get("george", id); slices.Contains(l.Progress, line) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress never showed %q", line)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// drain holds an idle machine, as a runner refuses new jobs once quiesced.
func (m *fakeMachine) drain(_ context.Context, _ proto.LeaseTarget, _ string) (func(context.Context) error, error) {
	if m.down.Load() {
		return nil, errors.New("connection refused")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running.Load() > 0 || m.held {
		return nil, fmt.Errorf("%w: 1 running", ErrRunnerBusy)
	}
	m.held = true
	return func(context.Context) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.held = false
		return nil
	}, nil
}

// testKey is a distinct, well-formed SSH public key line.
func testKey(i int) string {
	return fmt.Sprintf("ssh-ed25519 %s errand", base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "device %d", i)))
}

// blockRecord makes a lease's record unwritable and returns a function that
// restores it.
func blockRecord(t *testing.T, h *harness, id string) func() {
	t.Helper()
	path := filepath.Join(h.cfg.StateDir, "leases", id+".json")
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		os.RemoveAll(path)
		if err := os.WriteFile(path, saved, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// A busy machine keeps its idle deadline moving even when its record
// cannot be written, so the lease is not released soon after the work ends.
func TestBusyObservedWithoutAWrite(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.Offers[0].IdleTimeout = 150 * time.Millisecond
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	h.machine.running.Store(1)
	restore := blockRecord(t, h, l.ID)
	time.Sleep(400 * time.Millisecond)
	got, _ := b.Get("george", l.ID)
	if got.State != proto.LeaseReady || !got.IdleUntil.After(time.Now()) {
		t.Fatalf("busy lease lost its busy observations: %+v", got)
	}
	restore()
	h.machine.running.Store(0)
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
}

// Lease guarantee 5: naming a ready lease to let a device in counts as a
// hand-out, so a lease at its idle deadline is not released while the
// machine admits the device.
func TestAdmissionRestartsIdleWindow(t *testing.T) {
	h := newHarness(t, `echo '{"ssh":"ubuntu@box"}'`)
	h.cfg.IdlePoll = time.Hour // check only when woken
	h.cfg.Offers[0].IdleTimeout = 300 * time.Millisecond
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", testKey(0), "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	time.Sleep(350 * time.Millisecond) // past the idle deadline
	if _, err := b.Admit("george", "", l.ID, testKey(1)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got, _ := b.Get("george", l.ID)
		if got.State != proto.LeaseReady {
			t.Fatalf("lease released while admitting a device: %+v", got)
		}
		if slices.Contains(got.SSHKeys, testKey(1)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device never admitted: %+v", got)
		}
	}
}

// A lease admits a bounded number of keys, so its record stays readable.
func TestLeaseKeysAreBounded(t *testing.T) {
	h := newHarness(t, `echo '{"ssh":"ubuntu@box"}'`)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", testKey(0), "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	for i := 1; i < maxLeaseKeys; i++ {
		if _, err := b.Admit("george", "", l.ID, testKey(i)); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	var refused *Error
	if _, err := b.Admit("george", "", l.ID, testKey(maxLeaseKeys)); !errors.As(err, &refused) || refused.Status != http.StatusTooManyRequests {
		t.Fatalf("key past the bound: %v", err)
	}
	if _, err := b.Acquire("george", "", "gpu", testKey(maxLeaseKeys), ""); !errors.As(err, &refused) || refused.Status != http.StatusTooManyRequests {
		t.Fatalf("hand-out past the bound: %v", err)
	}
	if again, err := b.Admit("george", "", l.ID, testKey(1)); err != nil || again.ID != l.ID {
		t.Fatalf("a key already let in: %+v %v", again, err)
	}
}

// Ended leases have no worker, and only those that ended last are kept, so
// churn grows neither goroutines nor the lease list. A lease made long ago
// that ends now is kept over newer ones that ended before it.
func TestEndedLeasesAreBounded(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Close()
	dir := filepath.Join(h.cfg.StateDir, "leases")
	for range maxEndedLeases + 8 {
		r := record{Owner: "george", Release: h.cfg.Offers[0].Provider.ReleaseSpec(), Lease: proto.Lease{ID: proto.NewULID(), Offer: "h100", State: proto.LeaseReleased, ReleasedAt: time.Now().Add(-time.Hour)}}
		data, _ := json.Marshal(r)
		if err := os.WriteFile(filepath.Join(dir, r.ID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := runtime.NumGoroutine()
	b2 := h.start(t)
	if n := len(b2.List("george")); n != maxEndedLeases+1 {
		t.Fatalf("%d leases kept, want %d ended and the ready one", n, maxEndedLeases)
	}
	if n := runtime.NumGoroutine() - before; n > 3 {
		t.Fatalf("%d goroutines for one ready lease", n)
	}
	b2.Release("george", l.ID)
	waitState(t, b2, "george", l.ID, proto.LeaseReleased)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		files, _ := os.ReadDir(dir)
		listed := b2.List("george")
		if len(listed) == maxEndedLeases && len(files) == maxEndedLeases && slices.ContainsFunc(listed, func(x proto.Lease) bool { return x.ID == l.ID }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d listed, %d recorded", len(listed), len(files))
		}
	}
}

// A request asked again after its answer was lost gets its lease even when
// the cloud peer has since lost its offers.
func TestRepeatedRequestAfterOffersRemoved(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	request := proto.NewULID()
	l, err := b.Acquire("george", "", "gpu", "", request)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Close()
	h.cfg.Offers = nil
	b2 := h.start(t)
	if again, err := b2.Acquire("george", "", "gpu", "", request); err != nil || again.ID != l.ID {
		t.Fatalf("repeated request: %+v %v", again, err)
	}
	var refused *Error
	if _, err := b2.Acquire("george", "", "gpu", "", proto.NewULID()); !errors.As(err, &refused) || refused.Status != http.StatusNotFound {
		t.Fatalf("new request without offers: %v", err)
	}
}

// A request handed a ready lease is tied to it: asked again, it gets that
// lease even after the lease ended, and not another. Only the most recent
// requests are kept.
func TestRepeatedRequestForReadyLease(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.MaxLeases = 3
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", proto.NewULID())
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	request := proto.NewULID()
	if handed, err := b.Acquire("george", "", "gpu", "", request); err != nil || handed.ID != l.ID {
		t.Fatalf("hand-out: %+v %v", handed, err)
	}
	for range maxRequests {
		b.Acquire("george", "", "gpu", "", proto.NewULID())
	}
	b.mu.Lock()
	kept := len(b.leases[l.ID].Requests)
	b.mu.Unlock()
	if kept != maxRequests {
		t.Fatalf("%d requests kept", kept)
	}
	recent := proto.NewULID()
	b.Acquire("george", "", "gpu", "", recent)
	b.Release("george", l.ID)
	if again, err := b.Acquire("george", "", "gpu", "", recent); err != nil || again.ID != l.ID {
		t.Fatalf("repeated request: %+v %v", again, err)
	}
}

// A ready lease is handed out only if the worker's latest probe reached it
// and found it still matching. One that failed is passed over without a new
// idle window, and handed out again once a probe succeeds.
func TestUnusableReadyLeaseNotHandedOut(t *testing.T) {
	for _, tc := range []struct {
		name         string
		break_, mend func(*fakeMachine)
		reason       string
	}{
		{"unreachable", func(m *fakeMachine) { m.down.Store(true) }, func(m *fakeMachine) { m.down.Store(false) }, "this cloud peer cannot reach it"},
		{"mismatched", func(m *fakeMachine) { m.gpus = nil }, func(m *fakeMachine) { m.gpus = []proto.GPU{{Name: "NVIDIA H100 80GB HBM3", MemoryMiB: 81559}} }, "it no longer matches gpu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, okAcquire)
			h.cfg.IdlePoll = time.Hour // probe only when woken
			h.cfg.MaxLeases = 3
			b := h.start(t)
			l, err := b.Acquire("george", "", "gpu", "", proto.NewULID())
			if err != nil {
				t.Fatal(err)
			}
			waitState(t, b, "george", l.ID, proto.LeaseReady)
			// A lease just made ready is handed out without another probe.
			probes := h.machine.probes.Load()
			if again, err := b.Acquire("george", "", "gpu", "", proto.NewULID()); err != nil || again.ID != l.ID {
				t.Fatalf("fresh lease not handed out: %+v %v", again, err)
			}
			if h.machine.probes.Load() > probes+1 { // at most the worker's own
				t.Fatal("hand-out probed the machine")
			}
			h.machine.mu.Lock()
			tc.break_(h.machine)
			h.machine.mu.Unlock()
			waitUnusable(t, b, l.ID, true)
			before, _ := b.Get("george", l.ID)
			other, err := b.Acquire("george", "", "gpu", "", proto.NewULID())
			if err != nil || other.ID == l.ID {
				t.Fatalf("handed out an unusable lease: %+v %v", other, err)
			}
			got, _ := b.Get("george", l.ID)
			if !got.IdleUntil.Equal(before.IdleUntil) {
				t.Fatalf("idle window restarted: %s, was %s", got.IdleUntil, before.IdleUntil)
			}
			if !strings.Contains(strings.Join(got.Progress, "\n"), "not handed out: "+tc.reason) {
				t.Fatalf("progress %q", got.Progress)
			}
			var refused *Error
			if _, err := b.Admit("george", "", l.ID, ""); !errors.As(err, &refused) || refused.Status != http.StatusConflict || !strings.Contains(refused.Msg, tc.reason) {
				t.Fatalf("admission by name: %v", err)
			}
			if got, _ := b.Get("george", l.ID); !got.IdleUntil.Equal(before.IdleUntil) {
				t.Fatalf("admission by name restarted the idle window")
			}
			h.machine.mu.Lock()
			tc.mend(h.machine)
			h.machine.mu.Unlock()
			waitUnusable(t, b, l.ID, false)
			if _, err := b.Release("george", other.ID); err != nil { // newer, so it would be handed out first
				t.Fatal(err)
			}
			if again, err := b.Acquire("george", "", "gpu", "", proto.NewULID()); err != nil || again.ID != l.ID {
				t.Fatalf("mended lease not handed out: %+v %v", again, err)
			}
		})
	}
}

// waitUnusable wakes a lease's worker until its latest probe found the
// machine unusable, or usable.
func waitUnusable(t *testing.T, b *Broker, id string, unusable bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b.mu.Lock()
		got := b.leases[id].unusable != ""
		b.mu.Unlock()
		if got == unusable {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease unusable = %v, want %v", got, unusable)
		}
		wake(b, id)
	}
}

// A job a runner admits after the last idle check keeps its lease: the
// runner is asked to refuse new jobs before the release, and one that has
// taken a job since keeps running.
func TestJobAdmittedAtIdleDeadlineKeepsLease(t *testing.T) {
	h := newHarness(t, `echo '{"ssh":"ubuntu@box"}'`)
	h.cfg.IdlePoll = time.Hour // check only when woken
	h.cfg.Offers[0].IdleTimeout = 100 * time.Millisecond
	var admitLate atomic.Bool
	h.cfg.Probe = func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error) {
		info, err := h.machine.probe(ctx, target, identity, where)
		if admitLate.Load() {
			h.machine.running.Store(1) // admitted just after this answer
		}
		return info, err
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	time.Sleep(150 * time.Millisecond)
	admitLate.Store(true)
	wake(b, l.ID)
	time.Sleep(100 * time.Millisecond)
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReady {
		t.Fatalf("lease released under a job admitted at its idle deadline: %+v", got)
	}
	admitLate.Store(false)
	h.machine.running.Store(0) // the job finished
	wake(b, l.ID)
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
	h.machine.mu.Lock()
	held := h.machine.held
	h.machine.mu.Unlock()
	if !held {
		t.Fatal("released a runner that could still take jobs")
	}
}

// A lease handed out while its runner is being drained keeps the runner,
// which takes jobs again.
func TestHandOutDuringDrainResumesRunner(t *testing.T) {
	h := newHarness(t, `echo '{"ssh":"ubuntu@box"}'`)
	h.cfg.IdlePoll = time.Hour
	h.cfg.Offers[0].IdleTimeout = 100 * time.Millisecond
	draining, proceed := make(chan struct{}), make(chan struct{})
	h.cfg.Drain = func(ctx context.Context, target proto.LeaseTarget, identity string) (func(context.Context) error, error) {
		resume, err := h.machine.drain(ctx, target, identity)
		close(draining)
		<-proceed
		return resume, err
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	time.Sleep(150 * time.Millisecond)
	wake(b, l.ID)
	<-draining
	if again, err := b.Acquire("george", "", "gpu", "", ""); err != nil || again.ID != l.ID {
		t.Fatalf("hand-out: %+v %v", again, err)
	}
	close(proceed)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		h.machine.mu.Lock()
		held := h.machine.held
		h.machine.mu.Unlock()
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner still refuses jobs")
		}
	}
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReady {
		t.Fatalf("lease handed out during the drain was released: %+v", got)
	}
}

// An idle release goes ahead without the hold only when no one can be
// holding the machine: it has no SSH route, its runner refuses the hold, or
// the cloud peer could not reach it. A drain that fails otherwise keeps the
// lease and is retried at the next idle check.
func TestIdleReleaseWhenDrainFails(t *testing.T) {
	ssh := `echo '{"ssh":"ubuntu@box"}'`
	for _, tc := range []struct {
		name, acquire string
		down          bool
		drain         error
		released      bool
	}{
		{"no SSH route", okAcquire, false, errors.New("not asked"), true},
		{"runner refuses", ssh, false, fmt.Errorf("%w: not the runner's user", ErrUndrainable), true},
		{"machine unreachable", ssh, true, errors.New("connection refused"), true},
		{"drain fails", ssh, false, errors.New("ssh: connection timed out"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.acquire)
			h.cfg.IdlePoll = time.Hour // check only when woken
			h.cfg.Offers[0].IdleTimeout = 100 * time.Millisecond
			var drains atomic.Int32
			var failing atomic.Bool
			failing.Store(true)
			h.cfg.Drain = func(ctx context.Context, target proto.LeaseTarget, identity string) (func(context.Context) error, error) {
				drains.Add(1)
				if failing.Load() {
					return nil, tc.drain
				}
				return h.machine.drain(ctx, target, identity)
			}
			b := h.start(t)
			l, err := b.Acquire("george", "", "gpu", testKey(0), "")
			if err != nil {
				t.Fatal(err)
			}
			waitState(t, b, "george", l.ID, proto.LeaseReady)
			h.machine.down.Store(tc.down)
			time.Sleep(150 * time.Millisecond)
			wake(b, l.ID)
			if tc.released {
				waitState(t, b, "george", l.ID, proto.LeaseReleased)
				if tc.acquire == okAcquire && drains.Load() != 0 {
					t.Fatal("asked a runner with no SSH route to hold")
				}
				return
			}
			waitProgress(t, b, l.ID, "could not hold the machine idle before releasing it; retrying: ssh: connection timed out")
			if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReady {
				t.Fatalf("released after a failed drain: %+v", got)
			}
			failing.Store(false)
			wake(b, l.ID)
			waitState(t, b, "george", l.ID, proto.LeaseReleased)
		})
	}
}

// drainProvider reaches its machines' runners over SSH to hold them idle,
// while clients reach them at the URL it names.
type drainProvider struct{ CommandProvider }

func (p drainProvider) Acquire(ctx context.Context, req AcquireRequest) (Machine, error) {
	m, err := p.CommandProvider.Acquire(ctx, req)
	m.Drain = &proto.LeaseTarget{SSH: "ubuntu@box", HostKey: "ssh-ed25519 aG9zdA== box"}
	return m, err
}

// A machine clients reach on the tailnet is held idle through the way its
// provider named for that, which outlives a restart.
func TestDrainUsesProvidersDrainTarget(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.Offers[0].IdleTimeout = 100 * time.Millisecond
	h.cfg.Offers[0].Provider = drainProvider{h.cfg.Offers[0].Provider.(CommandProvider)}
	drained := make(chan proto.LeaseTarget, 1)
	h.cfg.Drain = func(ctx context.Context, target proto.LeaseTarget, identity string) (func(context.Context) error, error) {
		select {
		case drained <- target:
		default:
		}
		return h.machine.drain(ctx, target, identity)
	}
	h.cfg.IdlePoll = time.Hour
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Close()
	b2 := h.start(t)
	time.Sleep(150 * time.Millisecond)
	wake(b2, l.ID)
	waitState(t, b2, "george", l.ID, proto.LeaseReleased)
	if got := <-drained; got.SSH != "ubuntu@box" || got.URL != "" {
		t.Fatalf("drained through %+v", got)
	}
}

// A ready lease restored after a restart is handed out, by request or by
// name, only once the worker's first probe has reached it.
func TestRestoredLeaseWaitsForProbe(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.MaxLeases = 3
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Close()
	answer := make(chan struct{})
	probe := h.cfg.Probe
	h.cfg.Probe = func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error) {
		select {
		case <-answer:
		case <-ctx.Done():
			return proto.Info{}, ctx.Err()
		}
		return probe(ctx, target, identity, where)
	}
	b2 := h.start(t)
	var refused *Error
	if _, err := b2.Admit("george", "", l.ID, ""); !errors.As(err, &refused) || refused.Status != http.StatusConflict || !strings.Contains(refused.Msg, "not checked since the cloud peer restarted") {
		t.Fatalf("admission before the first probe: %v", err)
	}
	if other, err := b2.Acquire("george", "", "gpu", "", ""); err != nil || other.ID == l.ID {
		t.Fatalf("handed out before the first probe: %+v %v", other, err)
	}
	close(answer)
	waitUnusable(t, b2, l.ID, false)
	if again, err := b2.Admit("george", "", l.ID, ""); err != nil || again.ID != l.ID {
		t.Fatalf("admission after the first probe: %+v %v", again, err)
	}
}
