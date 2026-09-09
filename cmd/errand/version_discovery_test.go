package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestMismatchedRunnerRemainsDiscoverable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(proto.Info{Version: "old", Proto: proto.ProtoVersion})
	}))
	defer server.Close()
	provider := stubProvider{peers: []tailnet.Peer{{DNSName: "runner.example.ts.net", HostName: "runner", Online: true}}}
	deps := testDeps(t, filepath.Join(t.TempDir(), "config.toml"), provider)
	deps.probe = func(ctx context.Context, _ string) (proto.Info, error) {
		return client.ProbeInfo(ctx, server.URL, probeTimeout)
	}
	var out, stderr bytes.Buffer
	code := cmdPeersTo([]string{"discover"}, &out, &stderr, deps)
	if code != 0 || !strings.Contains(out.String(), "runner") || strings.Contains(out.String(), "no errand runners") || !strings.Contains(out.String(), "different version") {
		t.Fatalf("discovery: %d %s %s", code, &out, &stderr)
	}
	out.Reset()
	code = cmdDoctorTo([]string{"--url", server.URL, "--json"}, &out, &stderr, deps.probe)
	var report doctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if code != 0 || report.Info == nil || report.Info.Version != "old" || !strings.Contains(out.String(), "Runner old; CLI") {
		t.Fatalf("doctor: %d %s", code, &out)
	}
}
