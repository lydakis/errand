package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/placement"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestWhereAdmissionRevalidationAndReplay(t *testing.T) {
	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	m, err := snapshot.Build(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "go")
	// The daemon's normal PATH contains Go; this job explicitly removes it.
	spec := proto.Spec{Where: "go", Argv: []string{"/bin/true"}, ManifestRoot: m.RootHash(), Limits: proto.DefaultLimits(), Env: map[string]string{"PATH": dir}, EnvSources: map[string]string{"PATH": "literal"}}
	id := proto.NewULID()
	resp := rawSubmitSpec(t, ts.URL, id, root, spec, m)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("missing tool: %s %s", resp.Status, body)
	}
	d.mu.Lock()
	n := len(d.jobs)
	d.mu.Unlock()
	if n != 0 {
		t.Fatal("requirements rejection admitted job")
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	resp = rawSubmitSpec(t, ts.URL, id, root, spec, m)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("matching job: %s %s", resp.Status, body)
	}
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	resp = rawSubmitSpec(t, ts.URL, id, root, spec, m)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("replayed admitted job was re-rejected: %s %s", resp.Status, body)
	}
	r := proto.NewReceiptSpec(spec)
	if r.Where != "go" || r.SpecWithoutEnv().Where != "go" {
		t.Fatal("lost requirements in receipt")
	}
}

func TestWhereToolFactsRequireWorkingContainerRuntime(t *testing.T) {
	d, _ := testDaemon(t)
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	docker := filepath.Join(dir, "docker")
	for _, tc := range []struct {
		script string
		want   bool
	}{
		{"exit 1", false}, {`test "$1" = info && test -z "$CHECK_RUNTIME"`, true},
	} {
		if err := os.WriteFile(docker, []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		f := d.measurePlacementFacts(context.Background(), placement.Requirements{Tools: []string{"docker"}}, []string{"PATH=" + dir, "CHECK_RUNTIME=yes"})
		if (f.Tools["docker"] != "") != tc.want {
			t.Fatalf("runtime availability=%+v", f)
		}
	}
}

func TestWhereDoesNotExecuteSubmittedRuntime(t *testing.T) {
	d, _ := testDaemon(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf ran > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f := d.measurePlacementFacts(context.Background(), placement.Requirements{Tools: []string{"docker"}}, []string{"PATH=" + dir})
	if f.Tools["docker"] != "" {
		t.Fatal("attested a submitted runtime")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("submitted runtime executed: %v", err)
	}
}

func TestWhereChecksEffectiveExecutePermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can execute group-executable files")
	}
	d, _ := testDaemon(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "go")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0010); err != nil {
		t.Fatal(err)
	}
	f := d.measurePlacementFacts(context.Background(), placement.Requirements{Tools: []string{"go"}}, []string{"PATH=" + dir})
	if f.Tools["go"] != "" {
		t.Fatal("accepted tool without effective execute permission")
	}
	if _, err := resolveExecutable("go", dir, dir); err == nil {
		t.Fatal("job resolver accepted same unexecutable tool")
	}
}

func TestWhereInfoAndWorkspaceCreation(t *testing.T) {
	_, ts := testDaemon(t)
	info, err := client.ProbeWhereInfo(context.Background(), ts.URL, "os="+runtime.GOOS, time.Second)
	if err != nil || !info.Placement || info.Facts.OS != runtime.GOOS {
		t.Fatalf("info=%+v %v", info, err)
	}
	wrongOS := "linux"
	if runtime.GOOS == "linux" {
		wrongOS = "darwin"
	}
	_, err = client.CreateWorkspace(client.RunOptions{Root: t.TempDir(), PeerURL: ts.URL, NoSnapshot: true, Where: "os=" + wrongOS}, "wrong")
	if err == nil || !strings.Contains(err.Error(), "requires os=") {
		t.Fatalf("created incompatible workspace: %v", err)
	}
	rows, err := client.ListWorkspaces(ts.URL)
	if err != nil || len(rows) != 0 {
		t.Fatalf("published rejected workspace: %+v %v", rows, err)
	}
}

func TestWhereSkipsRuntimeProbeForQuiesceAndReplay(t *testing.T) {
	d, ts := testDaemon(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "probes")
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf x >> '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	root := workspaceWith(t, nil)
	m, err := snapshot.Build(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := proto.Spec{Where: "docker", Argv: []string{"/bin/true"}, ManifestRoot: m.RootHash(), Limits: proto.DefaultLimits()}
	id := proto.NewULID()
	d.mu.Lock()
	d.setupQuiesceToken, d.setupQuiesceUntil = "test", time.Now().Add(time.Minute)
	d.mu.Unlock()
	resp := rawSubmitSpec(t, ts.URL, id, root, spec, m)
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("quiesce: %s", resp.Status)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("probe ran during quiesce: %v", err)
	}
	d.mu.Lock()
	d.setupQuiesceToken = ""
	d.mu.Unlock()
	for _, want := range []int{201, 200} {
		resp = rawSubmitSpec(t, ts.URL, id, root, spec, m)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("submission: %s %s", resp.Status, body)
		}
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "x" {
		t.Fatalf("replay executed probe: %q %v", data, err)
	}
}

func TestWhereRuntimeDeadlinesAndProbeCapacity(t *testing.T) {
	d, _ := testDaemon(t)
	dir := t.TempDir()
	for tool, script := range map[string]string{"docker": "exec /bin/sleep 10", "podman": "exit 0"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	q := placement.Requirements{Tools: []string{"docker", "podman"}}
	f := d.measurePlacementFacts(context.Background(), q, (&Job{}).buildEnv())
	if f.Tools["podman"] == "" || !strings.Contains(f.ToolErrors["docker"], "deadline") {
		t.Fatalf("slow Docker starved Podman or lost timeout reason: %+v", f)
	}
	for i := 0; i < cap(d.placementSlots); i++ {
		d.placementSlots <- struct{}{}
	}
	defer func() {
		for len(d.placementSlots) > 0 {
			<-d.placementSlots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	f = d.measurePlacementFacts(ctx, placement.Requirements{Tools: []string{"podman"}}, (&Job{}).buildEnv())
	if f.Tools["podman"] != "" || !strings.Contains(f.ToolErrors["podman"], "waiting for capacity") {
		t.Fatalf("ignored probe capacity: %+v", f)
	}
}
