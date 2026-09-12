package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
)

func TestCLIWhereFallsBackAfterCapacityRace(t *testing.T) {
	if os.Getenv("ERRAND_WHERE_ENTRYPOINT") == "1" {
		os.Args = []string{"errand", "--where", "*", "--no-apply", "--", "/bin/cat", "input.txt"}
		main()
		return
	}
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: version})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var rejections, admissions atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/info" {
			json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, Placement: true, MaxJobs: 2})
			return
		}
		if r.Method == http.MethodPut {
			rejections.Add(1)
			http.Error(w, "runner filled after probe", 429)
			return
		}
		http.NotFound(w, r)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/info" {
			json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, Placement: true, MaxJobs: 2, RunningJobs: 1})
			return
		}
		if r.Method == http.MethodPut {
			admissions.Add(1)
		}
		d.Handler().ServeHTTP(w, r)
	}))
	defer second.Close()
	writeClientConfig(t, fmt.Sprintf("[peers.first]\nurl=%q\n[peers.second]\nurl=%q\n", first.URL, second.URL))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("selected runner received the snapshot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCLIWhereFallsBackAfterCapacityRace$")
	command.Dir = root
	command.Env = append(os.Environ(), "ERRAND_WHERE_ENTRYPOINT=1")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		t.Fatalf("run: %v %s", err, &output)
	}
	if rejections.Load() != 1 || admissions.Load() != 1 || !strings.Contains(output.String(), "selected runner received the snapshot") {
		t.Fatalf("rejections=%d admissions=%d output=%s", rejections.Load(), admissions.Load(), &output)
	}
	for _, text := range []string{"selected first", "selected second"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("missing selection explanation: %s", &output)
		}
	}
}

func TestWorkspaceWhereCreationReportsSelectedPeer(t *testing.T) {
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true, Version: version})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/info" {
			json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, Placement: true, MaxJobs: 2})
			return
		}
		http.Error(w, "requirements changed", 412)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/info" {
			json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, Placement: true, MaxJobs: 2, RunningJobs: 1})
			return
		}
		d.Handler().ServeHTTP(w, r)
	}))
	defer second.Close()
	writeClientConfig(t, fmt.Sprintf("[peers.first]\nurl=%q\n[peers.second]\nurl=%q\n[profiles.dev.run]\nworkspace='existing'\nwhere='*'\n", first.URL, second.URL))
	t.Chdir(t.TempDir())
	var out, stderr bytes.Buffer
	if code := cmdWorkspacesTo([]string{"create", "--profile", "dev", "--no-snapshot", "--json", "experiment"}, &out, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	var result struct {
		proto.Workspace
		Peer, URL string
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Peer != "second" || result.URL != second.URL || result.Name != "experiment" {
		t.Fatalf("result=%+v", result)
	}
	// A script can immediately address the workspace using the returned alias.
	out.Reset()
	stderr.Reset()
	if code := cmdWorkspacesTo([]string{"list", "--on", result.Peer, "--json"}, &out, &stderr); code != 0 || !strings.Contains(out.String(), result.ID) {
		t.Fatalf("list: code=%d %s %s", code, &out, &stderr)
	}
}

func TestWorkspaceWhereFinalRejectionPrintedOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/info" {
			json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: version, Placement: true, MaxJobs: 2})
			return
		}
		http.Error(w, "requirements changed", 412)
	}))
	defer server.Close()
	writeClientConfig(t, fmt.Sprintf("[peers.runner]\nurl=%q\n", server.URL))
	t.Chdir(t.TempDir())
	var out, stderr bytes.Buffer
	if code := cmdWorkspacesTo([]string{"create", "--where", "*", "--no-snapshot", "experiment"}, &out, &stderr); code != 1 || strings.Count(stderr.String(), "requirements changed") != 1 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
}
