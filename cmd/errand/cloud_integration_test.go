package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/cloud"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

// A cloud peer leases a machine for a requirement none of the caller's
// runners meets; the job then runs on the leased runner directly.
func TestCLIWhereLeasesFromCloudPeer(t *testing.T) {
	if raw := os.Getenv("ERRAND_CLOUD_ARGS"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
		// TestMain gives every process its own state; the client's lease
		// key must carry over between these CLI runs.
		os.Setenv("XDG_STATE_HOME", os.Getenv("ERRAND_CLOUD_STATE"))
		os.Exit(runCLI(args))
	}
	if runtime.GOOS == "windows" {
		t.Skip("provider scripts are POSIX shell")
	}
	h100 := func(context.Context) []proto.GPU {
		return []proto.GPU{{Name: "NVIDIA H100 80GB HBM3", MemoryMiB: 81559}}
	}
	box, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: version, GPUProbe: h100})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	var boxJobs atomic.Int32
	boxServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v0/jobs/") {
			boxJobs.Add(1)
		}
		box.Handler().ServeHTTP(w, r)
	}))
	defer boxServer.Close()

	scripts := t.TempDir()
	releaseLog := filepath.Join(scripts, "released")
	acquire := filepath.Join(scripts, "acquire.sh")
	release := filepath.Join(scripts, "release.sh")
	sentKey := filepath.Join(scripts, "ssh-key")
	os.WriteFile(acquire, []byte(fmt.Sprintf("#!/bin/sh\necho \"booting $ERRAND_OFFER\" >&2\necho \"$ERRAND_LEASE_SSH_KEY\" > %q\necho '{\"url\":%q}'\n", sentKey, boxServer.URL)), 0700)
	os.WriteFile(release, []byte(fmt.Sprintf("#!/bin/sh\necho \"$ERRAND_LEASE_ID\" >> %q\n", releaseLog)), 0700)
	brokerCfg, err := config.DaemonCloud{Offers: []config.CloudOffer{{
		Name: "h100", OS: runtime.GOOS, GPU: "H100 80GB", VRAM: 80, Price: 2.49,
		Acquire: []string{acquire}, Release: []string{release},
	}}}.Broker()
	if err != nil {
		t.Fatal(err)
	}
	brokerCfg.Probe, brokerCfg.AdmitKeys = probeLeaseTarget, admitLeaseKeys
	brokerCfg.ReadyPoll = 10 * time.Millisecond
	broker, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: version, GPUProbe: func(context.Context) []proto.GPU { return nil }, Cloud: brokerCfg})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	brokerServer := httptest.NewServer(broker.Handler())
	defer brokerServer.Close()

	writeClientConfig(t, fmt.Sprintf("[peers.cloud]\nurl=%q\n", brokerServer.URL))
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("ERRAND_CLOUD_STATE", state)
	leasePollInterval = 10 * time.Millisecond
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600)
	os.WriteFile(filepath.Join(root, "input.txt"), []byte("trained on the leased box\n"), 0600)
	cli := func(args ...string) (string, error) {
		raw, _ := json.Marshal(args)
		command := exec.Command(os.Args[0], "-test.run=^TestCLIWhereLeasesFromCloudPeer$")
		command.Dir = root
		command.Env = append(os.Environ(), "ERRAND_CLOUD_ARGS="+string(raw))
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		err := command.Run()
		return output.String(), err
	}

	// A run that cannot start locally rents nothing.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)
	out, err := cli("--where", "gpu=h100", "--no-apply", "-L", port+":3000", "--", "/bin/cat", "input.txt")
	busy.Close()
	if err == nil || strings.Contains(out, "leasing") || strings.Contains(out, "booting") {
		t.Fatalf("run with a busy local port: %v\n%s", err, out)
	}

	out, err = cli("--where", "gpu=h100", "--no-apply", "--", "/bin/cat", "input.txt")
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, out)
	}
	name := regexp.MustCompile(`lease (cloud-[0-9a-z]{4}) ready`).FindStringSubmatch(out)
	for _, want := range []string{"no runner of yours matches gpu=h100; leasing h100 ($2.49/h) from cloud", "cloud: booting h100", "trained on the leased box"} {
		if !strings.Contains(out, want) || name == nil {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if boxJobs.Load() != 1 {
		t.Fatalf("leased runner admitted %d jobs", boxJobs.Load())
	}
	// The client sent the public half of its own errand key.
	sent, _ := os.ReadFile(sentKey)
	public, err := os.ReadFile(filepath.Join(state, "errand", "ssh", "errand_ed25519.pub"))
	if err != nil || !strings.HasPrefix(string(sent), "ssh-ed25519 ") || strings.TrimSpace(string(sent)) != strings.TrimSpace(string(public)) {
		t.Fatalf("lease key %q, client key %q %v", sent, public, err)
	}

	// The ready lease is now an ordinary peer: matched directly, no new lease.
	out, err = cli("--where", "gpu=h100", "--no-apply", "--", "/bin/cat", "input.txt")
	if err != nil || strings.Contains(out, "leasing") || !strings.Contains(out, "selected "+name[1]+" for gpu=h100") {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	if out, err = cli("--on", name[1], "--no-apply", "--", "/bin/cat", "input.txt"); err != nil || !strings.Contains(out, "trained on the leased box") {
		t.Fatalf("--on lease peer: %v\n%s", err, out)
	}
	if out, err = cli("peers"); err != nil || !strings.Contains(out, "lease: h100 from cloud") || !strings.Contains(out, "1x NVIDIA H100 80GB HBM3 (80 GiB)") {
		t.Fatalf("peers: %v\n%s", err, out)
	}
	// Anything listed as a peer can be inspected as one.
	if out, err = cli("peers", "--on", name[1]); err != nil || !strings.Contains(out, "lease: h100 from cloud") || strings.Count(out, "\n") != 2 {
		t.Fatalf("peers --on lease peer: %v\n%s", err, out)
	}
	if out, err = cli("leases"); err != nil || !regexp.MustCompile(name[1]+`\s+cloud\s+h100\s+ready`).MatchString(out) {
		t.Fatalf("leases: %v\n%s", err, out)
	}
	// Fleet reads include the leased machine.
	if out, err = cli("ps", "-a"); err != nil || !strings.Contains(out, name[1]) {
		t.Fatalf("ps: %v\n%s", err, out)
	}
	// The cloud peer ends the lease on its own (idle, lifetime) while its
	// machine still answers: the run must not go to the ended lease.
	out, err = cli("leases", "--json")
	var listed []struct {
		Peer string `json:"peer"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed) != 1 || listed[0].Peer != name[1] {
		t.Fatalf("leases --json: %v\n%s", err, out)
	}
	ended := listed[0].ID
	req, _ := http.NewRequest(http.MethodDelete, brokerServer.URL+"/v0/leases/"+ended, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if data, _ := os.ReadFile(releaseLog); bytes.Contains(data, []byte(ended)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("release command did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out, err = cli("--on", name[1], "--no-apply", "--", "/bin/true"); err == nil || !strings.Contains(out, "no ready lease of yours") {
		t.Fatalf("--on ended lease: %v\n%s", err, out)
	}
	out, err = cli("--where", "gpu=h100", "--no-apply", "--", "/bin/cat", "input.txt")
	if err != nil || strings.Contains(out, "selected "+name[1]) || !strings.Contains(out, "leasing h100") {
		t.Fatalf("run after the lease ended: %v\n%s", err, out)
	}
	name = regexp.MustCompile(`lease (cloud-[0-9a-z]{4}) ready`).FindStringSubmatch(out)
	if name == nil {
		t.Fatalf("no new lease:\n%s", out)
	}

	// The wildcard never rents, and a capability no offer has fails plainly.
	if out, err = cli("--where", "gpus>=8", "--no-apply", "--", "/bin/true"); err == nil || !strings.Contains(out, "no runner matches") {
		t.Fatalf("unmatched: %v\n%s", err, out)
	}

	if out, err = cli("leases", "release", name[1]); err != nil || !strings.Contains(out, name[1]+" released") {
		t.Fatalf("leases release: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(releaseLog); bytes.Count(data, []byte("\n")) != 2 {
		t.Fatalf("releases %q", data)
	}
	if out, err = cli("leases", "release", name[1]); err == nil || !strings.Contains(out, "no active lease of yours") {
		t.Fatalf("leases release twice: %v\n%s", err, out)
	}
}

func TestCloudPeerRefusesWildcardLeases(t *testing.T) {
	brokerCfg := &cloud.Config{Offers: []cloud.Offer{{Name: "x", Provider: cloud.CommandProvider{AcquireCommand: []string{"/bin/false"}, ReleaseCommand: []string{"/bin/true"}}, IdleTimeout: time.Minute, MaxLifetime: time.Minute}},
		Probe:     func(context.Context, proto.LeaseTarget, string, string) (proto.Info, error) { return proto.Info{}, nil },
		AdmitKeys: admitLeaseKeys}
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: version, Cloud: brokerCfg})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v0/leases", "application/json", strings.NewReader(`{"where":"*"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
