package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
}

func (m *fakeMachine) probe(_ context.Context, target proto.LeaseTarget, _ string) (proto.Info, error) {
	m.probes.Add(1)
	if target.URL != "http://box:7443" {
		return proto.Info{}, fmt.Errorf("unexpected target %+v", target)
	}
	if m.down.Load() {
		return proto.Info{}, errors.New("connection refused")
	}
	return proto.Info{MaxJobs: 1, RunningJobs: int(m.running.Load()), Facts: proto.Facts{OS: "linux", Arch: "amd64", GPUs: m.gpus}}, nil
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
	l, err := b.Acquire("42", "old@github", "gpu")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "42", l.ID, proto.LeaseReady)
	if same, err := b.Acquire("42", "old@github", "gpu"); err != nil || same.ID != l.ID {
		t.Fatalf("same login must reuse: %+v %v", same, err)
	}
	if renamed, err := b.Acquire("42", "new@github", "gpu"); err != nil || renamed.ID == l.ID {
		t.Fatalf("renamed login must not reuse: %+v %v", renamed, err)
	}
}

func TestLeaseLifecycle(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu=h100")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != proto.LeaseLaunching || l.Offer != "h100" {
		t.Fatalf("lease %+v", l)
	}
	again, err := b.Acquire("george", "", "gpu=h100,vram>=80")
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
	if reused, err := b.Acquire("george", "", "gpu"); err != nil || reused.ID != l.ID {
		t.Fatalf("ready lease must be reused: %+v %v", reused, err)
	}
	if _, ok := b.Get("someone-else", l.ID); ok {
		t.Fatal("leases are private to their owner")
	}
	if ids := b.ActiveIDs("george"); len(ids) != 1 || ids[0] != l.ID {
		t.Fatalf("active ids %v", ids)
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
	if ids := b.ActiveIDs("george"); len(ids) != 0 {
		t.Fatalf("released lease still active: %v", ids)
	}
}

func TestAcquireRefusals(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.MaxLeases = 1
	b := h.start(t)
	for where, status := range map[string]int{"*": http.StatusBadRequest, "nonsense": http.StatusBadRequest, "gpu=a100": http.StatusPreconditionFailed, "gpus>=2": http.StatusPreconditionFailed} {
		_, err := b.Acquire("george", "", where)
		var e *Error
		if !errors.As(err, &e) || e.Status != status {
			t.Errorf("%q: %v", where, err)
		}
	}
	if _, err := b.Acquire("george", "", "gpu"); err != nil {
		t.Fatal(err)
	}
	_, err := b.Acquire("other", "", "gpu")
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusTooManyRequests {
		t.Fatalf("max_leases: %v", err)
	}
}

func TestIdleAndExpiredLeasesRelease(t *testing.T) {
	h := newHarness(t, okAcquire)
	h.cfg.Offers[0].IdleTimeout = 150 * time.Millisecond
	h.machine.running.Store(1)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu")
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
	l2, err := b2.Acquire("george", "", "gpu")
	if err != nil {
		t.Fatal(err)
	}
	expired := waitState(t, b2, "george", l2.ID, proto.LeaseReleased)
	if !strings.Contains(strings.Join(expired.Progress, "\n"), "max lifetime") {
		t.Fatalf("a busy lease must still stop at its max lifetime: %q", expired.Progress)
	}
}

func TestReleaseWhileLaunchingStopsAcquire(t *testing.T) {
	h := newHarness(t, "echo booting >&2\nexec sleep 30\n")
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu")
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

func TestFailedLaunchesAreReleased(t *testing.T) {
	h := newHarness(t, "echo 'out of capacity' >&2\nexit 3\n")
	b := h.start(t)
	l, _ := b.Acquire("george", "", "gpu")
	failed := waitState(t, b, "george", l.ID, proto.LeaseFailed)
	if !strings.Contains(failed.Error, "acquire command failed") || !strings.Contains(h.releases(), l.ID) {
		t.Fatalf("lease %+v releases %q", failed, h.releases())
	}

	// A machine that never matches the requirements is not handed out.
	h2 := newHarness(t, okAcquire)
	h2.machine.gpus = nil
	h2.cfg.AcquireTimeout = 200 * time.Millisecond
	b2 := h2.start(t)
	l2, _ := b2.Acquire("george", "", "gpu")
	failed = waitState(t, b2, "george", l2.ID, proto.LeaseFailed)
	if !strings.Contains(failed.Error, "requires 1 GPU (has none)") {
		t.Fatalf("lease %+v", failed)
	}
}

func TestRestartRecoversLeases(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, _ := b.Acquire("george", "", "gpu")
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Close()

	// A launch interrupted by a crash is released on the next start.
	crashed := record{Owner: "george", Provider: "command", Lease: proto.Lease{ID: proto.NewULID(), Offer: "h100", Where: "gpu", State: proto.LeaseLaunching, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}
	data, _ := json.Marshal(crashed)
	if err := os.WriteFile(filepath.Join(h.cfg.StateDir, "leases", crashed.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	b2 := h.start(t)
	if got, ok := b2.Get("george", l.ID); !ok || got.State != proto.LeaseReady {
		t.Fatalf("ready lease after restart: %+v", got)
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
	l, _ := b.Acquire("george", "", "gpu")
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Release("george", l.ID)
	time.Sleep(100 * time.Millisecond)
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReleasing || !strings.Contains(strings.Join(got.Progress, "\n"), "release failed: exit status 1 api 503; retrying") {
		t.Fatalf("lease %+v", got)
	}
	os.WriteFile(marker, nil, 0600)
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
}

// Only the kind of provider that acquired an active lease can release it, so
// the broker refuses to start when that offer is gone or changed kind.
func TestRestartRefusesChangedProvider(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, _ := b.Acquire("george", "", "gpu")
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	b.Close()

	changed := h.cfg
	changed.Offers = slices.Clone(h.cfg.Offers)
	changed.Offers[0].Provider = &LambdaProvider{}
	if _, err := New(changed); err == nil || !strings.Contains(err.Error(), `acquired by a "command" provider, but the offer now uses lambda`) {
		t.Fatalf("changed provider: %v", err)
	}
	// A record that does not name its provider is trusted to none.
	unnamed := record{Owner: "george", Lease: proto.Lease{ID: proto.NewULID(), Offer: "h100", Where: "gpu", State: proto.LeaseReady, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}
	data, _ := json.Marshal(unnamed)
	path := filepath.Join(h.cfg.StateDir, "leases", unnamed.ID+".json")
	os.WriteFile(path, data, 0600)
	if _, err := New(h.cfg); err == nil || !strings.Contains(err.Error(), `acquired by a "" provider`) {
		t.Fatalf("record without a provider: %v", err)
	}
	os.Remove(path)
	gone := h.cfg
	gone.Offers = []Offer{h.cfg.Offers[0]}
	gone.Offers[0].Name = "a100"
	if _, err := New(gone); err == nil || !strings.Contains(err.Error(), "no longer configured") {
		t.Fatalf("removed offer: %v", err)
	}
	b2 := h.start(t) // the original configuration still works
	if got, ok := b2.Get("george", l.ID); !ok || got.State != proto.LeaseReady {
		t.Fatalf("lease after restart: %+v", got)
	}
}

// max_lifetime is a hard stop even for a launch that never finishes.
func TestLaunchStopsAtMaxLifetime(t *testing.T) {
	h := newHarness(t, "exec sleep 30\n")
	h.cfg.Offers[0].MaxLifetime = 200 * time.Millisecond
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu")
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
	h.cfg.Probe = func(ctx context.Context, target proto.LeaseTarget, where string) (proto.Info, error) {
		// Put a directory where the only lease's record goes, so every later
		// write of the record fails, root or not.
		records, _ := filepath.Glob(filepath.Join(h.cfg.StateDir, "leases", "*.json"))
		for _, record := range records {
			if err := os.Remove(record); err == nil {
				os.MkdirAll(filepath.Join(record, "blocked"), 0700)
			}
		}
		return probe(ctx, target, where)
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu")
	if err != nil {
		t.Fatal(err)
	}
	ended := waitState(t, b, "george", l.ID, proto.LeaseFailed)
	if ended.Target != nil || ended.ReadyAt != (time.Time{}) || !strings.Contains(ended.Error, "recording the ready lease") || !strings.Contains(h.releases(), l.ID) {
		t.Fatalf("lease %+v, releases %q", ended, h.releases())
	}
}

// Ended leases are forgotten after a week by a running broker too, so its
// lease list does not grow without bound.
func TestReaperForgetsOldEndedLeases(t *testing.T) {
	h := newHarness(t, okAcquire)
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu")
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
	b.reapOnce()
	if _, ok := b.Get("george", l.ID); ok {
		t.Fatal("old ended lease still listed")
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
	l, err := b.Acquire("george", "", "gpu")
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
	h.cfg.IdlePoll = time.Hour
	var hang atomic.Bool
	probe := h.cfg.Probe
	h.cfg.Probe = func(ctx context.Context, target proto.LeaseTarget, where string) (proto.Info, error) {
		if hang.Load() {
			<-ctx.Done()
			return proto.Info{}, ctx.Err()
		}
		return probe(ctx, target, where)
	}
	b := h.start(t)
	l, err := b.Acquire("george", "", "gpu")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "george", l.ID, proto.LeaseReady)
	hang.Store(true)
	b.mu.Lock()
	b.leases[l.ID].ExpiresAt = time.Now().Add(-time.Second)
	b.mu.Unlock()
	start := time.Now()
	b.reapOnce()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("expired lease waited %s on a probe", took)
	}
	if got, _ := b.Get("george", l.ID); got.State != proto.LeaseReleased {
		t.Fatalf("expired lease %+v", got)
	}
}
