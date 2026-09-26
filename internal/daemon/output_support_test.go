package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
