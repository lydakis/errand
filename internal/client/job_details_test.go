package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestInspectJobDetailsBoundsConcurrencyAndPreservesMissingReceipts(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	entered := make(chan struct{}, 16)
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-gate
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(proto.JobDetails{JobStatus: proto.JobStatus{ID: strings.TrimPrefix(r.URL.Path, "/v0/jobs/")}})
	}))
	defer server.Close()
	defer release()
	ids := []string{"missing"}
	for range 15 {
		ids = append(ids, proto.NewULID())
	}
	done := make(chan []JobDetailResult, 1)
	go func() { done <- InspectJobDetails(server.URL, ids) }()
	for range 8 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("detail requests were not concurrent")
		}
	}
	release()
	results := <-done
	if peak.Load() > 8 {
		t.Fatalf("unbounded concurrency: %d", peak.Load())
	}
	if !IsNotFound(results[0].Err) {
		t.Fatalf("missing receipt: %v", results[0].Err)
	}
	for i := 1; i < len(ids); i++ {
		if results[i].Err != nil || results[i].Details.ID != ids[i] {
			t.Fatalf("result %d: %+v", i, results[i])
		}
	}
}
