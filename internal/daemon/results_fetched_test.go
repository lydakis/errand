package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// A finished job's retained changes count as work in /v0/info until a client
// downloads them whole, across a restart of the runner; a job that changed
// nothing never counts, and removing the job stops the count. Info reports
// when the runner admitted the most recent such job, and its most recent job
// of any kind, across a restart too.
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
	info := func() proto.Info {
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
		return info
	}
	admitted := func(id string) time.Time {
		t.Helper()
		d.mu.Lock()
		defer d.mu.Unlock()
		j, ok := d.jobs[id]
		if !ok {
			t.Fatalf("no job %s", id)
		}
		return j.Admission.Time
	}
	latest := func(want time.Time) {
		t.Helper()
		if got := info().LatestUnfetched; !got.Equal(want) {
			t.Fatalf("latest unfetched %s, want %s", got, want)
		}
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

	lastAdmitted := func(want time.Time) {
		t.Helper()
		if got := info().LatestAdmitted; !got.Equal(want) {
			t.Fatalf("latest admitted %s, want %s", got, want)
		}
	}

	lastAdmitted(time.Time{})
	unchanged := run("cat value.txt")
	latest(time.Time{}) // a job that changed nothing never counts
	lastAdmitted(admitted(unchanged))
	older := run("echo older > value.txt")
	newer := run("echo newer > value.txt")
	latest(admitted(newer))
	fetch(newer)
	fetch(newer) // a second download changes nothing
	latest(admitted(older))
	lastAdmitted(admitted(newer))
	stop()
	d, ts = start()
	latest(admitted(older))
	lastAdmitted(admitted(newer))
	keep := 0
	postJobGC(t, ts.URL, proto.JobGCRequest{Keep: &keep})
	d.mu.Lock()
	_, ok := d.jobs[older]
	d.mu.Unlock()
	if ok {
		t.Fatal("job GC kept the unfetched job")
	}
	latest(time.Time{})
}

// A download can finish after a job publishes its result but before the job
// is counted as unfetched. The download is still recorded, and the count that
// follows sees it.
func TestFetchBeforeResultsAreCountedIsRecorded(t *testing.T) {
	d, _ := testDaemon(t)
	j := &Job{ID: proto.NewULID(), Dir: t.TempDir()}
	res := &proto.Result{Changes: &proto.ChangeSummary{PathCount: 1}}

	d.resultsFetched(j)
	d.mu.Lock()
	d.noteResultsLocked(j, res)
	n := len(d.unfetched)
	d.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d unfetched after a download that finished before the count, want 0", n)
	}
	if !fetchRecorded(j) {
		t.Fatal("the download left no record")
	}
}
