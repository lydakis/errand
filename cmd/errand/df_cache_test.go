package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestDfDistinguishesUnmeasuredNamedCache(t *testing.T) {
	rows := []dfRow{{Location: "runner", NamedCaches: &proto.NamedCacheStats{Items: 1, Unmeasured: 1}, Details: &proto.StorageDetails{NamedCaches: []proto.NamedCacheStorage{{Name: "packages", BytesUnknown: true}}}}}
	var summary, details bytes.Buffer
	writeDf(&summary, rows)
	writeDfDetails(&details, rows)
	if !strings.Contains(summary.String(), "+ 1 unmeasured") || !strings.Contains(details.String(), "unmeasured") {
		t.Fatalf("unmeasured storage reported as empty:\n%s\n%s", &summary, &details)
	}
}
