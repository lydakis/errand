package daemon

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// A silent job must still keep its log stream audible, or followers cannot
// tell a quiet command from a runner that went away.
func TestLogStreamHeartbeatsWhileJobIsSilent(t *testing.T) {
	previous := logHeartbeatInterval
	logHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { logHeartbeatInterval = previous })

	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/sh", "-c", "sleep 0.3"})
	resp.Body.Close()

	resp, err := http.Get(ts.URL + "/v0/jobs/" + id + "/logs?from=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	heartbeats := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == ":" {
			heartbeats++
		}
		if strings.HasPrefix(line, "event: status") {
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if heartbeats < 3 {
		t.Fatalf("silent job's log stream carried %d heartbeats, want several", heartbeats)
	}
}

// A follower that stops reading must not pin its log-reader reservation, and
// with it job GC, for as long as its connection stays open.
func TestLogStreamDropsAFollowerThatStopsReading(t *testing.T) {
	previous := logWriteTimeout
	logWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { logWriteTimeout = previous })

	d, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	// Far more output than socket buffers hold, then stay alive.
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/sh", "-c", "head -c 16000000 /dev/zero; sleep 30"})
	resp.Body.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /v0/jobs/%s/logs?from=0 HTTP/1.1\r\nHost: errand\r\n\r\n", id)
	// Never read.

	d.mu.Lock()
	j := d.jobs[id]
	d.mu.Unlock()
	readers := func() int {
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.logReaders
	}
	deadline := time.Now().Add(10 * time.Second)
	for readers() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if readers() == 0 {
		t.Fatal("log follower never started")
	}
	for readers() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := readers(); n != 0 {
		t.Fatalf("stalled follower still holds %d log-reader reservations", n)
	}
	kill, err := http.Post(ts.URL+"/v0/jobs/"+id+"/kill?force=1", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	kill.Body.Close()
	waitTerminal(t, ts.URL, id)
}
