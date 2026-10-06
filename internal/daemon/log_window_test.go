package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// readLogEvents reads one finite log response and returns the decoded output
// and the name of the closing event.
func readLogEvents(t *testing.T, url string) (output, closing string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, event := range strings.Split(string(body), "\n\n") {
		var name, data string
		for _, line := range strings.Split(event, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		switch name {
		case "log":
			var f proto.LogFrame
			if err := json.Unmarshal([]byte(data), &f); err != nil {
				t.Fatal(err)
			}
			raw, _ := base64.StdEncoding.DecodeString(f.DataB64)
			out.Write(raw)
		case "":
		default:
			closing = name
		}
	}
	return out.String(), closing
}

// A finite read of a running job returns what it has written so far and ends,
// instead of following until the job exits.
func TestLogReplayWithoutFollowEndsWhileTheJobRuns(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/sh", "-c", "printf 'one\\ntwo\\nthree\\n'; sleep 30"})
	resp.Body.Close()
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v0/jobs/"+id+"/kill", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		waitTerminal(t, ts.URL, id)
	})

	logs := ts.URL + "/v0/jobs/" + id + "/logs?from=0&follow=0"
	deadline := time.Now().Add(10 * time.Second)
	for {
		output, closing := readLogEvents(t, logs)
		if closing != "end" {
			t.Fatalf("finite read closed with %q, want end", closing)
		}
		if output == "one\ntwo\nthree\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("finite read never showed the job's output; last %q", output)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if output, _ := readLogEvents(t, logs+"&tail=2"); output != "two\nthree\n" {
		t.Fatalf("tail=2 replayed %q", output)
	}
	if output, _ := readLogEvents(t, logs+"&tail=0"); output != "" {
		t.Fatalf("tail=0 replayed %q", output)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	if output, _ := readLogEvents(t, logs+"&since="+strconv.FormatInt(future, 10)); output != "" {
		t.Fatalf("since an hour from now replayed %q", output)
	}
	resp, err := http.Get(ts.URL + "/v0/jobs/" + id + "/logs?from=0&tail=-3")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte("tail")) {
		t.Fatalf("negative tail = %s %s, want 400", resp.Status, body)
	}
	resp, err = http.Get(ts.URL + "/v0/jobs/" + id + "/logs?follow=0&from=" + strconv.FormatInt(math.MaxInt64, 10))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte("from")) {
		t.Fatalf("from past every sequence number = %s %s, want 400", resp.Status, body)
	}
}

// Following from a since later than everything written so far holds back
// output written before that time, live output included.
func TestLogFollowHoldsBackOutputBeforeAFutureSince(t *testing.T) {
	_, ts := testDaemon(t)
	root := workspaceWith(t, nil)
	id := proto.NewULID()
	resp := rawSubmit(t, ts.URL, id, root, []string{"/bin/sh", "-c", "printf 'one\\n'; sleep 1; printf 'two\\n'"})
	resp.Body.Close()
	t.Cleanup(func() { waitTerminal(t, ts.URL, id) })

	future := time.Now().Add(time.Hour).UnixMilli()
	output, closing := readLogEvents(t, ts.URL+"/v0/jobs/"+id+"/logs?from=0&since="+strconv.FormatInt(future, 10))
	if closing != "status" {
		t.Fatalf("follow closed with %q, want status", closing)
	}
	if output != "" {
		t.Fatalf("since an hour from now followed %q", output)
	}
}
