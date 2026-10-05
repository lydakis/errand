package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

func TestPeersPipedTableOmitsEmptyColumnsAndKeepsZeroCounts(t *testing.T) {
	rows := []peerRow{{Name: "offline", Status: "unreachable", Detail: "connection refused"}}
	var out bytes.Buffer
	writePeers(termui.Plain(&out, &out).Out, rows, false)
	header := strings.Fields(strings.SplitN(out.String(), "\n", 2)[0])
	if strings.Join(header, ",") != "NAME,STATUS,DETAIL" {
		t.Fatalf("empty columns: %s", &out)
	}
	rows = append(rows, peerRow{Name: "online", Default: true, Status: "ready", Info: &proto.Info{Version: "test"}})
	out.Reset()
	writePeers(termui.Plain(&out, &out).Out, rows, false)
	header = strings.Fields(strings.SplitN(out.String(), "\n", 2)[0])
	if strings.Join(header, ",") != "NAME,DEFAULT,STATUS,VERSION,SLOTS,QUEUE,STAGING,DETAIL" || !strings.Contains(out.String(), "0/0") {
		t.Fatalf("missing values or zero counts: %s", &out)
	}
}

func TestPeersOnATerminalMarkTheDefaultAndFlagProblems(t *testing.T) {
	info := func(v string) *proto.Info {
		return &proto.Info{Version: v, MaxJobs: 2, MaxQueued: 8, RunningJobs: 1,
			Facts: proto.Facts{OS: "linux", Arch: "amd64", NumCPU: 4, KVM: true, Tools: map[string]string{"go": "/usr/bin/go", "git": "/usr/bin/git"}}}
	}
	rows := []peerRow{
		{Name: "cabal", Default: true, Status: "ready", Info: info(version)},
		{Name: "mini", Status: "unreachable", Detail: "unreachable: dial tcp: lookup mini: no such host"},
	}
	con, screen := terminal(120)
	writePeers(con.Out, rows, false)
	got := termui.StripANSI(screen.String())
	for _, want := range []string{"* cabal", "● ready", "1/2", "0/8", "linux/amd64 · 4 cpu · kvm", "git go", "○ unreachable", "no such host", "* default"} {
		if !strings.Contains(got, want) {
			t.Fatalf("peers lacks %q:\n%s", want, got)
		}
	}

	rows = []peerRow{{Name: "cabal", Default: true, Status: "ready", Info: info("0.0.1-old")}}
	con, screen = terminal(120)
	writePeers(con.Out, rows, false)
	if got := termui.StripANSI(screen.String()); !strings.Contains(got, "cabal runs errand 0.0.1-old; this CLI is "+version) || !strings.Contains(got, "errand setup") {
		t.Fatalf("version mismatch not called out:\n%s", got)
	}
}
