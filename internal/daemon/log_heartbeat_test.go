package daemon

import (
	"bufio"
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
