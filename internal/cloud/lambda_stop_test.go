package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// stopper stops acquire at one step. A step is an API request, a state save
// or an SSH command. A crash stops the whole cloud peer, so nothing after it
// happens and only what was persisted survives; an error fails just that
// step and acquire goes on. Either can strike before the step takes effect
// or after, when its result is lost.
type stopper struct {
	mu            sync.Mutex
	steps, at     int
	crash, after  bool
	crashed, off  bool
	cancelAcquire context.CancelFunc
}

var errStopped = errors.New("stopped here")

func (s *stopper) step(do func() error) error {
	s.mu.Lock()
	if s.off {
		s.mu.Unlock()
		return do()
	}
	n := s.steps
	s.steps++
	crashed := s.crashed
	here := n == s.at
	if here && s.crash {
		s.crashed = true
	}
	s.mu.Unlock()
	switch {
	case crashed:
		return errStopped
	case !here:
		return do()
	}
	if s.after {
		do()
	}
	if s.crash {
		s.cancelAcquire()
	}
	return errStopped
}

type stopTransport struct{ s *stopper }

func (t stopTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var resp *http.Response
	err := t.s.step(func() error {
		var err error
		resp, err = http.DefaultTransport.RoundTrip(r)
		return err
	})
	if err != nil {
		if resp != nil {
			resp.Body.Close() // the answer is lost
		}
		return nil, err
	}
	return resp, nil
}

// stopAcquire runs a Lambda acquire that stops at step at (never when at is
// negative), then releases the lease as the broker would, and fails unless
// release ends with no instance of the lease left. It returns the number of
// steps acquire took.
func stopAcquire(t *testing.T, at int, crash, after bool) int {
	t.Helper()
	p, api, ssh := newLambda(t)
	clock := time.Now()
	p.Now = func() time.Time { return clock }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := &stopper{at: at, crash: crash, after: after, cancelAcquire: cancel}
	p.HTTP = &http.Client{Transport: stopTransport{s}}
	p.SSH = func(ctx context.Context, args []string, stdin io.Reader) ([]byte, error) {
		var out []byte
		err := s.step(func() error {
			var err error
			out, err = ssh.run(ctx, args, stdin)
			return err
		})
		return out, err
	}
	// What the broker would hold in memory, and what is on disk.
	var held, persisted json.RawMessage
	id := proto.NewULID()
	machine, err := p.Acquire(ctx, AcquireRequest{
		LeaseID: id, Offer: "h100", Login: "george@github", Progress: func(string) {},
		Save: func(state json.RawMessage) error {
			err := s.step(func() error { persisted = slices.Clone(state); return nil })
			held = state // kept in memory even when the write fails
			return err
		},
	})
	steps := s.steps
	state := held
	switch {
	case crash:
		state = persisted // a restarted cloud peer reads the disk
	case err == nil:
		state = machine.State
	}
	if at < 0 && err != nil {
		t.Fatalf("acquire without stopping: %v", err)
	}

	s.mu.Lock()
	s.off = true // release runs in a healthy cloud peer
	s.mu.Unlock()
	rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rcancel()
	for try := 0; ; try++ {
		err := p.Release(rctx, ReleaseRequest{LeaseID: id, Offer: "h100", State: state})
		if err == nil {
			break
		}
		if try == 2 {
			t.Fatalf("release never finished: %v (state %s)", err, state)
		}
		clock = clock.Add(lambdaLaunchSettle + time.Minute) // the broker retries later
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, in := range api.instances {
		if in.Name == LeaseHostname(id) && !lambdaGone(in.Status) {
			t.Fatalf("release left %s %s (state %s)", in.ID, in.Status, state)
		}
	}
	return steps
}

// Lambda launches cannot be retried safely, so acquire must leave enough
// behind for release to find what it launched wherever it stops. This stops
// it at every step, by a crash or an error, before or after the step takes
// effect, and releases what is left.
func TestLambdaReleaseAfterAnyStop(t *testing.T) {
	steps := stopAcquire(t, -1, false, false)
	if steps < 10 {
		t.Fatalf("acquire took only %d steps", steps)
	}
	for at := range steps {
		for _, crash := range []bool{true, false} {
			for _, after := range []bool{false, true} {
				name := fmt.Sprintf("step %d/%s", at, map[bool]string{true: "crash", false: "error"}[crash])
				if after {
					name += " after"
				}
				t.Run(name, func(t *testing.T) { stopAcquire(t, at, crash, after) })
			}
		}
	}
}
