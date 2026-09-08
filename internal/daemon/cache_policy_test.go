package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestCacheGCReportsEffectivePolicies(t *testing.T) {
	for _, test := range []struct {
		name            string
		cfg             Config
		snapshot, named proto.CacheGCPolicy
	}{
		{"defaults", Config{}, proto.CacheGCPolicy{MaxBytes: 5 << 30, TTLSeconds: 14 * 86400}, proto.CacheGCPolicy{MaxBytes: 5 << 30, TTLSeconds: 14 * 86400}},
		{"separate overrides", Config{CacheMaxBytes: 123, CacheTTL: 2 * time.Hour, NamedCacheMaxBytes: 456, NamedCacheTTL: 3 * time.Hour}, proto.CacheGCPolicy{MaxBytes: 123, TTLSeconds: 7200}, proto.CacheGCPolicy{MaxBytes: 456, TTLSeconds: 10800}},
		{"snapshot disabled", Config{CacheDisabled: true}, proto.CacheGCPolicy{}, proto.CacheGCPolicy{MaxBytes: 5 << 30, TTLSeconds: 14 * 86400}},
		// Disabling new cache bindings preserves collection of existing named caches.
		{"named admission disabled", Config{NamedCacheDisabled: true}, proto.CacheGCPolicy{MaxBytes: 5 << 30, TTLSeconds: 14 * 86400}, proto.CacheGCPolicy{MaxBytes: 5 << 30, TTLSeconds: 14 * 86400}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.cfg.StateDir = t.TempDir()
			d, err := New(test.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			for _, body := range []string{`{"dry_run":true}`, `{}`} {
				rec := httptest.NewRecorder()
				d.handleCacheGC(rec, httptest.NewRequest(http.MethodPost, "/v0/cache/gc", strings.NewReader(body)), Identity{})
				if rec.Code != http.StatusOK {
					t.Fatalf("GC = %d: %s", rec.Code, rec.Body.String())
				}
				var result proto.CacheGCResult
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Policies == nil || result.Policies.Named == nil || *result.Policies.Named != test.named {
					t.Fatalf("named policy = %+v, want %+v", result.Policies, test.named)
				}
				if test.cfg.CacheDisabled {
					if result.Policies.Snapshot != nil {
						t.Fatal("disabled snapshot cache reported a policy")
					}
				} else if result.Policies.Snapshot == nil || *result.Policies.Snapshot != test.snapshot {
					t.Fatalf("snapshot policy = %+v, want %+v", result.Policies.Snapshot, test.snapshot)
				}
			}
		})
	}
}
