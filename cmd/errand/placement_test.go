package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

func TestWhereSelectionFiltersAndBalances(t *testing.T) {
	e := config.EffectiveRun{Where: "os=linux,go"}
	for _, name := range []string{"offline", "wrong-os", "unsupported", "busy", "loaded", "available"} {
		e.Candidates = append(e.Candidates, config.RunCandidate{Name: name, URL: name})
	}
	selection, err := chooseRunners(context.Background(), e, func(_ context.Context, url, where string, _ time.Duration) (proto.Info, error) {
		i := proto.Info{Placement: true, MaxJobs: 4, Facts: proto.Facts{OS: "linux", Tools: map[string]string{"go": "/bin/go"}}}
		switch url {
		case "offline":
			return i, fmt.Errorf("unreachable")
		case "wrong-os":
			i.Facts.OS = "darwin"
		case "unsupported":
			i.Placement = false
		case "busy":
			i.Busy = true
		case "loaded":
			i.StagingJobs = 3
		}
		return i, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	choices := selection.Choices
	if len(choices) != 2 || choices[0].Name != "available" {
		t.Fatalf("choices=%+v", choices)
	}
	var errOut bytes.Buffer
	selection.printExcluded(&errOut)
	for _, s := range []string{"offline", "wrong-os", "unsupported", "busy"} {
		if !strings.Contains(errOut.String(), s) {
			t.Fatalf("missing exclusion %s: %s", s, &errOut)
		}
	}
}

func TestWhereProbeFanoutAndCancellation(t *testing.T) {
	e := config.EffectiveRun{Where: "*"}
	for i := 0; i < 20; i++ {
		e.Candidates = append(e.Candidates, config.RunCandidate{Name: fmt.Sprint(i), URL: fmt.Sprint(i)})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var active, peak atomic.Int32
	reached := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := chooseRunners(ctx, e, func(ctx context.Context, _, _ string, _ time.Duration) (proto.Info, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			if n == 8 {
				select {
				case reached <- struct{}{}:
				default:
				}
			}
			<-ctx.Done()
			return proto.Info{}, ctx.Err()
		})
		done <- err
	}()
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("probes did not run concurrently")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled probes matched")
		}
	case <-time.After(time.Second):
		t.Fatal("probes ignored cancellation")
	}
	if peak.Load() > 8 {
		t.Fatalf("unbounded probes: %d", peak.Load())
	}
}

func TestDoctorWhereReportsChoiceWithoutSubmitting(t *testing.T) {
	writeClientConfig(t, "[peers.runner]\nurl='http://runner.invalid'\n[peers.offline]\nurl='http://offline.invalid'\n")
	t.Chdir(t.TempDir())
	var out, errOut bytes.Buffer
	var calls atomic.Int32
	code := cmdDoctorWith([]string{"--where", "go", "--json"}, &out, &errOut, doctorServices{where: func(_ context.Context, target, where string, _ time.Duration) (proto.Info, error) {
		calls.Add(1)
		if target == "http://offline.invalid" {
			return proto.Info{}, fmt.Errorf("unreachable test peer")
		}
		if target != "http://runner.invalid" || where != "go" {
			t.Errorf("probe %s %s", target, where)
		}
		return proto.Info{Placement: true, Version: version, MaxJobs: 1, Facts: proto.Facts{Tools: map[string]string{"go": "/bin/go"}}}, nil
	}, probe: func(context.Context, string) (proto.Info, error) {
		t.Fatal("redundant probe")
		return proto.Info{}, nil
	}})
	if code != 0 || calls.Load() != 2 || !strings.Contains(out.String(), `"peer": "runner"`) || !strings.Contains(out.String(), `"reason": "unreachable test peer"`) {
		t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
	}
}

func TestDoctorWhereKeepsSSHDisplayURL(t *testing.T) {
	writeClientConfig(t, "[peers.runner]\nssh='host'\nremote_command='/custom/errand'\n")
	t.Chdir(t.TempDir())
	var out, errOut bytes.Buffer
	code := cmdDoctorWith([]string{"--where", "*", "--json"}, &out, &errOut, doctorServices{where: func(_ context.Context, target, _ string, _ time.Duration) (proto.Info, error) {
		if target == "ssh://host" {
			t.Error("custom SSH transport was not registered")
		}
		return proto.Info{Placement: true, Version: version, MaxJobs: 1}, nil
	}, ssh: func(context.Context, string) error {
		t.Fatal("repeated SSH inspection after successful info probe")
		return nil
	}})
	if code != 0 || !strings.Contains(out.String(), `"url": "ssh://host"`) {
		t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
	}
}
