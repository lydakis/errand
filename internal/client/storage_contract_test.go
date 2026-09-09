package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMaintenanceRejectsMissingResponseFields(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		request    func(string) error
	}{
		{"change totals", `{"jobs":{}}`, func(peer string) error { _, err := StorageStats(peer); return err }},
		{"requested details", `{"changes":{},"jobs":{}}`, func(peer string) error { _, err := StorageStatsDetailed(peer); return err }},
		{"cache policy", `{"removed_blobs":0,"freed_bytes":0}`, func(peer string) error { _, err := CacheGC(peer, true); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			if err := tc.request(server.URL); err == nil {
				t.Fatal("accepted incomplete response")
			}
		})
	}
}
