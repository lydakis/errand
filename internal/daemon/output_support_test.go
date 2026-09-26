package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func TestJobListResolvesShortIDPrefixesAndCountsChanges(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, map[string]string{"f": "x"})
	changed := proto.NewULID()
	rawSubmit(t, ts.URL, changed, root, []string{"/bin/sh", "-c", "echo new > new.txt"}).Body.Close()
	waitTerminal(t, ts.URL, changed)
	other := proto.NewULID()
	rawSubmit(t, ts.URL, other, root, []string{"/bin/echo", "ok"}).Body.Close()
	waitTerminal(t, ts.URL, other)

	list := func(query string) (int, []proto.JobListEntry) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v0/jobs?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var entries []proto.JobListEntry
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, entries
	}
	code, entries := list("prefix=" + strings.ToLower(changed[:12]))
	if code != http.StatusOK || len(entries) != 1 || entries[0].ID != changed {
		t.Fatalf("prefix listing = %d %+v, want only %s", code, entries, changed)
	}
	if entries[0].ChangedPaths != 1 {
		t.Fatalf("changed paths = %d, want 1", entries[0].ChangedPaths)
	}
	if code, entries = list("prefix=" + changed[:4]); code != http.StatusOK || len(entries) != 2 {
		t.Fatalf("a shared prefix lists both jobs: %d %d", code, len(entries))
	}
	if code, _ = list("prefix=not-a-ulid"); code != http.StatusBadRequest {
		t.Fatalf("invalid prefix = %d, want 400", code)
	}
}

func TestQueuedJobsReportPositionAndEveryLifecycleMomentIsLogged(t *testing.T) {
	var mu sync.Mutex
	var events []JobLogEvent
	d, err := New(Config{
		StateDir: t.TempDir(), InsecureNoAuth: true, Version: "test", MaxJobs: 1, MaxQueued: 2,
		JobLog: func(e JobLogEvent) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	root := workspaceWith(t, map[string]string{"f": "x"})

	blocker := proto.NewULID()
	rawSubmit(t, ts.URL, blocker, root, []string{"/bin/sleep", "30"}).Body.Close()
	waitState(t, ts.URL, blocker, proto.StateRunning)
	first, second := proto.NewULID(), proto.NewULID()
	rawSubmit(t, ts.URL, first, root, []string{"/bin/echo", "ok"}).Body.Close()
	waitState(t, ts.URL, first, proto.StateQueued)
	resp := rawSubmit(t, ts.URL, second, root, []string{"/bin/echo", "ok"})
	var admitted proto.JobStatus
	json.NewDecoder(resp.Body).Decode(&admitted)
	resp.Body.Close()
	waitState(t, ts.URL, second, proto.StateQueued)

	ahead := func(id string) *int {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v0/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var details proto.JobDetails
		json.NewDecoder(resp.Body).Decode(&details)
		return details.QueueAhead
	}
	if got := ahead(first); got == nil || *got != 0 {
		t.Fatalf("first queued job ahead = %v, want 0", got)
	}
	if got := ahead(second); got == nil || *got != 1 {
		t.Fatalf("second queued job ahead = %v, want 1", got)
	}
	if got := ahead(blocker); got != nil {
		t.Fatalf("running job has a queue position: %d", *got)
	}

	forceKill(t, ts.URL, blocker)
	waitTerminal(t, ts.URL, first)
	waitTerminal(t, ts.URL, second)
	// Lines are delivered on the log goroutine; wait for the last ones.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		finished := 0
		for _, e := range events {
			if e.Kind == JobLogFinished {
				finished++
			}
		}
		mu.Unlock()
		if finished == 3 {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	kinds := map[string][]JobLogKind{}
	for _, e := range events {
		kinds[e.ID] = append(kinds[e.ID], e.Kind)
		if e.Kind == JobLogFinished && e.Result == nil {
			t.Fatalf("finished event without a result: %+v", e)
		}
	}
	if got := kinds[second]; len(got) != 3 || got[0] != JobLogQueued || got[1] != JobLogStarted || got[2] != JobLogFinished {
		t.Fatalf("second job lifecycle = %v", got)
	}
	if got := kinds[blocker]; len(got) != 2 || got[0] != JobLogStarted || got[1] != JobLogFinished {
		t.Fatalf("blocker lifecycle = %v", got)
	}
}

func TestWorkspaceRemovalReportsWhatItFreed(t *testing.T) {
	_, ts := testDaemon(t)
	payload := strings.Repeat("x", 4096)
	root := workspaceWith(t, map[string]string{"a": payload})
	ws, err := client.CreateWorkspace(client.RunOptions{PeerURL: ts.URL, Root: root}, "freed")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := client.RemoveWorkspace(ts.URL, ws.Name)
	if err != nil {
		t.Fatal(err)
	}
	if removed.ID != ws.ID || removed.Name != "freed" || removed.FreedBytes < int64(len(payload)) {
		t.Fatalf("removal = %+v, want at least %d bytes freed", removed, len(payload))
	}
}

func TestAStalledJobLogNeverHoldsUpAJobOrLosesItsEvents(t *testing.T) {
	stall := make(chan struct{})
	var mu sync.Mutex
	var events []JobLogEvent
	d, err := New(Config{
		StateDir: t.TempDir(), InsecureNoAuth: true, Version: "test",
		JobLog: func(e JobLogEvent) {
			<-stall // a service whose stderr nobody reads, until it does
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(d.Handler())
	t.Cleanup(ts.Close)
	root := workspaceWith(t, nil)
	var ids []string
	for range 3 {
		id := proto.NewULID()
		rawSubmit(t, ts.URL, id, root, []string{"/bin/echo", "ok"}).Body.Close()
		waitTerminal(t, ts.URL, id)
		ids = append(ids, id)
	}
	close(stall)
	d.Close() // delivers everything still queued
	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, e := range events {
		got = append(got, e.ID[len(e.ID)-4:]+" "+string(e.Kind))
	}
	var want []string
	for _, id := range ids {
		want = append(want, id[len(id)-4:]+" started", id[len(id)-4:]+" finished")
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("events after the log unstalled = %v, want %v", got, want)
	}
}

func TestJobLogQueueKeepsOrderAndBoundsClose(t *testing.T) {
	release := make(chan struct{})
	var got []string
	q := newJobLogQueue(func(e JobLogEvent) { <-release; got = append(got, e.ID) })
	for i := range 5000 {
		q.push(JobLogEvent{ID: fmt.Sprint(i)})
	}
	close(release)
	q.close(5 * time.Second)
	if len(got) != 5000 || got[0] != "0" || got[4999] != "4999" {
		t.Fatalf("delivered %d events, first %v", len(got), got[:min(3, len(got))])
	}

	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	stuck := newJobLogQueue(func(JobLogEvent) { <-blocked })
	stuck.push(JobLogEvent{ID: "x"})
	started := time.Now()
	stuck.close(50 * time.Millisecond)
	if time.Since(started) > time.Second {
		t.Fatal("close waited on a stalled consumer")
	}
}
