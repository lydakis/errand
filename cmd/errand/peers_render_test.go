package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestPeersOmitEmptyColumnsAndKeepZeroCounts(t *testing.T) {
	rows := []peerRow{{Name: "offline", Status: "unreachable", Detail: "connection refused"}}
	var out bytes.Buffer
	writePeers(&out, rows)
	header := strings.Fields(strings.SplitN(out.String(), "\n", 2)[0])
	if strings.Join(header, ",") != "NAME,STATUS,DETAIL" {
		t.Fatalf("empty columns: %s", &out)
	}
	rows = append(rows, peerRow{Name: "online", Default: true, Status: "ready", Info: &proto.Info{Version: "test"}})
	out.Reset()
	writePeers(&out, rows)
	header = strings.Fields(strings.SplitN(out.String(), "\n", 2)[0])
	if strings.Join(header, ",") != "NAME,DEFAULT,STATUS,VERSION,SLOTS,QUEUE,STAGING,DETAIL" || !strings.Contains(out.String(), "0/0") {
		t.Fatalf("missing values or zero counts: %s", &out)
	}
}
