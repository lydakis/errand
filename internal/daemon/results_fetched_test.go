package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// A finished job's retained changes count as work in /v0/info until a client
// downloads them whole, across a restart of the runner; a job that changed
// nothing never counts, and removing the job stops the count.
func TestInfoCountsUnfetchedResults(t *testing.T) {
	state := t.TempDir()
	start := func() (*Daemon, *httptest.Server) {
		d, err := New(Config{StateDir: state, InsecureNoAuth: true, Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return d, httptest.NewServer(d.Handler())
	}
	d, ts := start()
	stop := func() {
		ts.Close()
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { stop() }()
	unfetched := func() int {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v0/info")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var info proto.Info
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			t.Fatal(err)
		}
		return info.Unfetched
	}
	run := func(script string) string {
		t.Helper()
		root := workspaceWith(t, map[string]string{"value.txt": "initial\n"})
		id := proto.NewULID()
		resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/sh", "-c", script})
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("submit = %s", resp.Status)
		}
		waitTerminal(t, ts.URL, id)
		return id
	}
	fetch := func(id string) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v0/jobs/" + id + "/changes")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("fetch = %s %v", resp.Status, err)
		}
	}

	run("cat value.txt")
	if n := unfetched(); n != 0 {
		t.Fatalf("a job that changed nothing counts as %d unfetched", n)
	}
	fetched := run("echo fetched > value.txt")
	kept := run("echo kept > value.txt")
	if n := unfetched(); n != 2 {
		t.Fatalf("%d unfetched, want 2", n)
	}
	fetch(fetched)
	fetch(fetched) // a second download changes nothing
	if n := unfetched(); n != 1 {
		t.Fatalf("%d unfetched after one fetch, want 1", n)
	}
	stop()
	d, ts = start()
	if n := unfetched(); n != 1 {
		t.Fatalf("%d unfetched after a restart, want 1", n)
	}
	keep := 0
	postJobGC(t, ts.URL, proto.JobGCRequest{Keep: &keep})
	d.mu.Lock()
	_, ok := d.jobs[kept]
	d.mu.Unlock()
	if ok {
		t.Fatal("job GC kept the unfetched job")
	}
	if n := unfetched(); n != 0 {
		t.Fatalf("%d unfetched after the jobs were removed, want 0", n)
	}
}
