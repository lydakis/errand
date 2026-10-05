package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

// writeDf renders a finished df table and its footnotes, off a terminal.
func writeDf(w io.Writer, rows []dfRow) {
	con := termui.Plain(w, w)
	view := newDfView(con, nil, false, false)
	for _, row := range rows {
		view.row(row)
	}
	view.done()
	view.footer(rows)
}

func TestDfDistinguishesUnmeasuredNamedCache(t *testing.T) {
	rows := []dfRow{{Location: "runner", NamedCaches: &proto.NamedCacheStats{Items: 1, Unmeasured: 1}, Details: &proto.StorageDetails{NamedCaches: []proto.NamedCacheStorage{{Name: "packages", BytesUnknown: true}}}}}
	var summary, details bytes.Buffer
	writeDf(&summary, rows)
	writeDfDetails(termui.Plain(&details, &details).Out, rows)
	if !strings.Contains(summary.String(), "+ 1 unmeasured cache") || !strings.Contains(details.String(), "not measured") {
		t.Fatalf("unmeasured storage reported as empty:\n%s\n%s", &summary, &details)
	}
}
