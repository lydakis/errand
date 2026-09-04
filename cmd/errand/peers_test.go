package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

type stubProvider struct {
	self  tailnet.Self
	peers []tailnet.Peer
}

func (p stubProvider) Name() string { return "stub" }
func (p stubProvider) WhoIs(context.Context, string, string) (tailnet.WhoIs, error) {
	return tailnet.WhoIs{}, errors.New("unused")
}
func (p stubProvider) SelfIPs(context.Context) ([]string, error)     { return p.self.IPs, nil }
func (p stubProvider) Self(context.Context) (tailnet.Self, error)    { return p.self, nil }
func (p stubProvider) Peers(context.Context) ([]tailnet.Peer, error) { return p.peers, nil }

func fakeRunner(t *testing.T, forbid bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/info" {
			http.NotFound(w, r)
			return
		}
		if forbid {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(proto.APIError{Error: "caller nobody@example.com (laptop) holds no errand authorization"})
			return
		}
		json.NewEncoder(w).Encode(proto.Info{Version: "test", MaxJobs: 2, Facts: proto.Facts{OS: "linux", Arch: "amd64", NumCPU: 4}})
	}))
	t.Cleanup(ts.Close)
	return ts
}

func testDeps(t *testing.T, cfgPath string, provider tailnet.Provider) peersDeps {
	t.Helper()
	return peersDeps{
		configPath: func() (string, error) { return cfgPath, nil },
		load: func() (config.Client, error) {
			var c config.Client
			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				return c, nil
			}
			_, err = toml.Decode(string(raw), &c)
			return c, err
		},
		probe: func(ctx context.Context, peerURL string) (proto.Info, error) {
			return client.ProbeInfo(ctx, peerURL, probeTimeout)
		},
		provider: func() (tailnet.Provider, error) { return provider, nil },
	}
}

func TestPeerTargetNormalization(t *testing.T) {
	cases := map[string]string{
		"cabal":                "http://cabal:7443",
		"cabal.example.ts.net": "http://cabal.example.ts.net:7443",
		"100.64.0.9":           "http://100.64.0.9:7443",
		"fd7a:115c:a1e0::1":    "http://[fd7a:115c:a1e0::1]:7443",
		"[fd7a:115c:a1e0::1]":  "http://[fd7a:115c:a1e0::1]:7443",
		"cabal:9000":           "http://cabal:9000",
		"http://cabal:7443/":   "http://cabal:7443",
	}
	for in, want := range cases {
		p, err := parsePeerTarget(in, false, "", "")
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := peerURLOf(p); got != want {
			t.Fatalf("%q → %q, want %q", in, got, want)
		}
	}
	if p, err := parsePeerTarget("mini", true, "/opt/bin/errand", ""); err != nil || p.SSH != "mini" || p.RemoteCommand != "/opt/bin/errand" {
		t.Fatalf("ssh target = %+v, %v", p, err)
	}
	for _, bad := range []string{"", "ftp://x", "ssh://mini?cmd=/opt/bin/errand", "a b", "host/path", "cabal:not-a-port"} {
		if _, err := parsePeerTarget(bad, false, "", ""); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestPeersAddSSHSupportsCustomSocketAndValidatesBeforeWriting(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	deps := testDeps(t, cfgPath, stubProvider{})
	var out, errb bytes.Buffer
	args := []string{
		"add", "--ssh", "--no-verify",
		"--remote-command", "/usr/local/bin/errand",
		"--remote-socket", "/srv/errand/errand.sock",
		"cabal", "george@cabal",
	}
	if code := cmdPeersTo(args, &out, &errb, deps); code != 0 {
		t.Fatalf("ssh add exit %d: %s", code, errb.String())
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ssh = "george@cabal"`,
		`remote_command = "/usr/local/bin/errand"`,
		`remote_socket = "/srv/errand/errand.sock"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("config missing %q:\n%s", want, raw)
		}
	}

	badPath := filepath.Join(t.TempDir(), "config.toml")
	badDeps := testDeps(t, badPath, stubProvider{})
	for _, badArgs := range [][]string{
		{"add", "--ssh", "--no-verify", "bad", "host with spaces"},
		{"add", "--ssh", "--no-verify", "--remote-command", "bin/errand", "bad", "cabal"},
		{"add", "--ssh", "--no-verify", "--remote-socket", "run/errand.sock", "bad", "cabal"},
	} {
		out.Reset()
		errb.Reset()
		if code := cmdPeersTo(badArgs, &out, &errb, badDeps); code != 2 {
			t.Fatalf("invalid add %v exit %d, want 2; stderr=%s", badArgs, code, errb.String())
		}
		if _, err := os.Stat(badPath); !os.IsNotExist(err) {
			t.Fatalf("invalid add %v wrote config", badArgs)
		}
	}
}

func TestPeersAddVerifiesThenWritesAndSetsDefault(t *testing.T) {
	runner := fakeRunner(t, false)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	deps := testDeps(t, cfgPath, stubProvider{})
	var out, errb bytes.Buffer
	if code := cmdPeersTo([]string{"add", "cabal", runner.URL}, &out, &errb, deps); code != 0 {
		t.Fatalf("add exit %d: %s", code, errb.String())
	}
	raw, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), `default_peer = "cabal"`) || !strings.Contains(string(raw), `url = "`+runner.URL+`"`) {
		t.Fatalf("config after add:\n%s", raw)
	}
	if !strings.Contains(out.String(), "now the default peer") {
		t.Fatalf("stdout: %s", out.String())
	}
	out.Reset()
	if code := cmdPeersTo([]string{"add", "mini", runner.URL + "/"}, &out, &errb, deps); code != 0 {
		t.Fatalf("second add exit %d: %s", code, errb.String())
	}
	raw, _ = os.ReadFile(cfgPath)
	if strings.Count(string(raw), "default_peer") != 1 || !strings.Contains(string(raw), "[peers.mini]") {
		t.Fatalf("config after second add:\n%s", raw)
	}
	if code := cmdPeersTo([]string{"add", "mini", runner.URL}, &out, &errb, deps); code == 0 {
		t.Fatal("duplicate add must fail without --force")
	}
	if code := cmdPeersTo([]string{"add", "-f", "mini", "127.0.0.1:1"}, &out, &errb, deps); code == 0 {
		t.Fatal("force add must still verify the new target")
	}
	if code := cmdPeersTo([]string{"add", "-f", "--no-verify", "mini", "cabal:9000"}, &out, &errb, deps); code != 0 {
		t.Fatalf("force add exit %d: %s", code, errb.String())
	}
	raw, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), `url = "http://cabal:9000"`) || strings.Count(string(raw), "[peers.mini]") != 1 {
		t.Fatalf("config after force replace:\n%s", raw)
	}
}

func TestPeersAddRefusesForbiddenRunnerWithRemedyAndWritesNothing(t *testing.T) {
	runner := fakeRunner(t, true)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	deps := testDeps(t, cfgPath, stubProvider{self: tailnet.Self{Login: "george@example.com"}})
	var out, errb bytes.Buffer
	if code := cmdPeersTo([]string{"add", "cabal", runner.URL}, &out, &errb, deps); code != 1 {
		t.Fatalf("forbidden add exit %d, want 1; stderr %s", code, errb.String())
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatal("a refused peer must not be written")
	}
	if !strings.Contains(errb.String(), `add "george@example.com" to allow_users`) ||
		!strings.Contains(errb.String(), "config used by its errand service") ||
		!strings.Contains(errb.String(), "same `--config` value") ||
		strings.Contains(errb.String(), "~/.config/errand/errandd.toml") ||
		strings.Contains(errb.String(), "setup --allow-user") {
		t.Fatalf("remedy missing: %s", errb.String())
	}
	if code := cmdPeersTo([]string{"add", "--no-verify", "cabal", runner.URL}, &out, &errb, deps); code != 0 {
		t.Fatalf("no-verify add exit %d: %s", code, errb.String())
	}
}

func TestPeersRejectUnexpectedArgumentsBeforeIO(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "unexpected"},
		{"discover", "--json", "unexpected"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			called := false
			deps := peersDeps{
				load: func() (config.Client, error) {
					called = true
					return config.Client{}, nil
				},
				provider: func() (tailnet.Provider, error) {
					called = true
					return stubProvider{}, nil
				},
			}
			var out, errb bytes.Buffer
			if code := cmdPeersTo(args, &out, &errb, deps); code != 2 {
				t.Fatalf("%v exit %d, want 2; stdout=%q stderr=%q", args, code, out.String(), errb.String())
			}
			if called {
				t.Fatalf("%v performed I/O before rejecting its arguments", args)
			}
			if !strings.Contains(errb.String(), "unexpected arguments") {
				t.Fatalf("%v error = %q", args, errb.String())
			}
		})
	}
}

func TestPeersListIsCompactAndRejectsHiddenSubcommands(t *testing.T) {
	runner := fakeRunner(t, false)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	deps := testDeps(t, cfgPath, stubProvider{})
	var out, errb bytes.Buffer
	if code := cmdPeersTo([]string{"add", "cabal", runner.URL}, &out, &errb, deps); code != 0 {
		t.Fatalf("add exit %d: %s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := cmdPeersTo(nil, &out, &errb, deps); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	text := out.String()
	for _, want := range []string{"NAME", "DEFAULT", "TARGET", "STATUS", "reachable"} {
		if !strings.Contains(text, want) {
			t.Fatalf("compact peers output missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"CPU", "KVM", "SLOTS", "linux/amd64"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("peers output overlaps with info via %q:\n%s", unwanted, text)
		}
	}
	if strings.Contains(text, "DETAIL") {
		t.Fatalf("healthy peers output includes an empty detail column:\n%s", text)
	}
	for _, hidden := range []string{"help", "list", "rm"} {
		out.Reset()
		errb.Reset()
		if code := cmdPeersTo([]string{hidden}, &out, &errb, deps); code != 2 {
			t.Fatalf("hidden subcommand %q exit %d, want 2", hidden, code)
		}
	}
}

func TestPeersListShowsDetailWhenAProbeFails(t *testing.T) {
	deps := peersDeps{
		load: func() (config.Client, error) {
			return config.Client{
				DefaultPeer: "cabal",
				Peers: map[string]config.Peer{
					"cabal": {URL: "http://cabal:7443"},
				},
			}, nil
		},
		probe: func(context.Context, string) (proto.Info, error) {
			return proto.Info{}, &client.ProbeError{Kind: client.ProbeUnreachable, Detail: "connection refused"}
		},
	}
	var out, errb bytes.Buffer
	if code := cmdPeersTo(nil, &out, &errb, deps); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"DETAIL", "unreachable", "connection refused"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("peers output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPeersHelpUsesEstablishedFlagAliases(t *testing.T) {
	deps := peersDeps{}
	var out, errb bytes.Buffer
	if code := cmdPeersTo([]string{"--help"}, &out, &errb, deps); code != 0 {
		t.Fatalf("peers --help exit %d", code)
	}
	if !strings.Contains(errb.String(), "errand peers discover [-a | --all]") {
		t.Fatalf("peers help omits discovery aliases:\n%s", errb.String())
	}

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"add", "--help"}, []string{"-f", "-force", "-n", "-dry-run", "-no-verify", "-remote-command", "-remote-socket", "-ssh"}},
		{[]string{"discover", "--help"}, []string{"-a", "-all", "-json"}},
	} {
		out.Reset()
		errb.Reset()
		if code := cmdPeersTo(tc.args, &out, &errb, deps); code != 0 {
			t.Fatalf("%v exit %d", tc.args, code)
		}
		for _, want := range tc.want {
			if !strings.Contains(errb.String(), want) {
				t.Errorf("%v help missing %q:\n%s", tc.args, want, errb.String())
			}
		}
	}
}

func TestPeersAddDryRunWritesNothing(t *testing.T) {
	runner := fakeRunner(t, false)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	var out, errb bytes.Buffer
	if code := cmdPeersTo([]string{"add", "-n", "cabal", runner.URL}, &out, &errb, testDeps(t, cfgPath, stubProvider{})); code != 0 {
		t.Fatalf("dry-run exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatal("dry run wrote config")
	}
	if !strings.Contains(out.String(), "[peers.cabal]") {
		t.Fatalf("dry run should show the block: %s", out.String())
	}
}

func TestPeersAddDryRunUsesRealDuplicateRulesBeforeProbing(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if _, err := config.AddPeer(cfgPath, "cabal", config.Peer{URL: "http://cabal:7443"}, false); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	deps := testDeps(t, cfgPath, stubProvider{})
	probes := 0
	deps.probe = func(context.Context, string) (proto.Info, error) {
		probes++
		return proto.Info{}, nil
	}
	var out, errb bytes.Buffer
	if code := cmdPeersTo([]string{"add", "--dry-run", "cabal", "cabal"}, &out, &errb, deps); code != 1 {
		t.Fatalf("duplicate dry run exit %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if probes != 0 {
		t.Fatalf("duplicate dry run made %d probes", probes)
	}
	if strings.Contains(out.String(), "would add") {
		t.Fatalf("duplicate dry run claimed it would add the peer: %s", out.String())
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("dry run changed the client config")
	}
}

func TestPeersHumanOutputEscapesControlCharacters(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	deps := testDeps(t, cfgPath, stubProvider{})
	deps.load = func() (config.Client, error) {
		return config.Client{DefaultPeer: "bad\x1b[2J", Peers: map[string]config.Peer{
			"bad\x1b[2J": {URL: "http://runner:7443"},
		}}, nil
	}
	deps.probe = func(context.Context, string) (proto.Info, error) {
		return proto.Info{}, &client.ProbeError{Kind: client.ProbeNotErrand, Detail: "bad\x1b[2J"}
	}
	var out, errb bytes.Buffer
	if code := cmdPeersTo(nil, &out, &errb, deps); code != 0 {
		t.Fatalf("peers exit %d: %s", code, errb.String())
	}
	if strings.ContainsRune(out.String(), '\x1b') || !strings.Contains(out.String(), `\x1b`) {
		t.Fatalf("peers emitted unsafe terminal text: %q", out.String())
	}

	deps.load = func() (config.Client, error) { return config.Client{}, nil }
	deps.provider = func() (tailnet.Provider, error) {
		return stubProvider{peers: []tailnet.Peer{{DNSName: "bad.example.ts.net", OS: "bad\x1b[2J", Online: true}}}, nil
	}
	deps.probe = func(context.Context, string) (proto.Info, error) {
		return proto.Info{Version: "bad\x1b[2J", Facts: proto.Facts{OS: "bad\x1b[2J"}}, nil
	}
	out.Reset()
	if code := cmdPeersTo([]string{"discover"}, &out, &errb, deps); code != 0 {
		t.Fatalf("discover exit %d: %s", code, errb.String())
	}
	if strings.ContainsRune(out.String(), '\x1b') || !strings.Contains(out.String(), `\x1b`) {
		t.Fatalf("discover emitted unsafe terminal text: %q", out.String())
	}
}

func TestPeersRemoveClearsDefault(t *testing.T) {
	runner := fakeRunner(t, false)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	deps := testDeps(t, cfgPath, stubProvider{})
	var out, errb bytes.Buffer
	cmdPeersTo([]string{"add", "cabal", runner.URL}, &out, &errb, deps)
	if code := cmdPeersTo([]string{"remove", "cabal"}, &out, &errb, deps); code != 0 {
		t.Fatalf("remove exit %d: %s", code, errb.String())
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "[peers.cabal]") || strings.Contains(string(raw), `default_peer = "cabal"`) {
		t.Fatalf("remove left state:\n%s", raw)
	}
	if code := cmdPeersTo([]string{"remove", "ghost"}, &out, &errb, deps); code == 0 {
		t.Fatal("removing an unknown peer must fail")
	}
}

func TestPeersDiscoverClassifiesTailnetNodes(t *testing.T) {
	runner := fakeRunner(t, false)
	forbidden := fakeRunner(t, true)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not errand</html>"))
	}))
	t.Cleanup(other.Close)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	provider := stubProvider{
		self: tailnet.Self{Login: "george@example.com"},
		peers: []tailnet.Peer{
			{DNSName: "cabal.example.ts.net", HostName: "cabal", OS: "linux", Online: true},
			{DNSName: "locked.example.ts.net", HostName: "locked", OS: "linux", Online: true},
			{DNSName: "web.example.ts.net", HostName: "web", OS: "linux", Online: true},
			{DNSName: "phone.example.ts.net", HostName: "phone", OS: "iOS", Online: false},
		},
	}
	targets := map[string]string{
		"http://cabal.example.ts.net:7443":  runner.URL,
		"http://locked.example.ts.net:7443": forbidden.URL,
		"http://web.example.ts.net:7443":    other.URL,
	}
	deps := testDeps(t, cfgPath, provider)
	deps.probe = func(ctx context.Context, peerURL string) (proto.Info, error) {
		if real, ok := targets[peerURL]; ok {
			return client.ProbeInfo(ctx, real, probeTimeout)
		}
		return client.ProbeInfo(ctx, "http://127.0.0.1:1", probeTimeout) // refused
	}
	var out, errb bytes.Buffer
	if code := cmdPeersTo([]string{"discover", "--json", "-a"}, &out, &errb, deps); code != 0 {
		t.Fatalf("discover exit %d: %s", code, errb.String())
	}
	var rows []discoveredRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, r := range rows {
		status[r.Name] = r.Status
	}
	want := map[string]string{"cabal": "runner", "locked": "forbidden", "web": "not-errand", "phone": "offline"}
	for name, s := range want {
		if status[name] != s {
			t.Fatalf("%s classified %q, want %q (rows %+v)", name, status[name], s, rows)
		}
	}
	out.Reset()
	if code := cmdPeersTo([]string{"discover"}, &out, &errb, deps); code != 0 {
		t.Fatalf("discover exit %d: %s", code, errb.String())
	}
	text := out.String()
	if !strings.Contains(text, "errand peers add cabal cabal.example.ts.net") ||
		!strings.Contains(text, `add "george@example.com" to allow_users`) ||
		!strings.Contains(text, "run `errand setup` to restart") ||
		strings.Contains(text, "web ") {
		t.Fatalf("discover output:\n%s", text)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatal("discover must be read-only")
	}
}

func TestPeersDiscoverRejectsMalformedClientConfig(t *testing.T) {
	deps := testDeps(t, filepath.Join(t.TempDir(), "config.toml"), stubProvider{})
	deps.load = func() (config.Client, error) { return config.Client{}, errors.New("bad client config") }
	var out, errb bytes.Buffer
	if code := cmdPeersTo([]string{"discover"}, &out, &errb, deps); code != 1 {
		t.Fatalf("discover exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "bad client config") {
		t.Fatalf("discover hid config error: %s", errb.String())
	}
}

func TestDiscoverRecognizesPeersConfiguredByIPOrShortName(t *testing.T) {
	cfg := config.Client{Peers: map[string]config.Peer{
		"cabal": {URL: "http://100.64.0.9:7443"},
		"mini":  {SSH: "george@mini"},
	}}
	hosts := configuredPeerHosts(cfg)
	cabal := tailnet.Peer{DNSName: "cabal.example.ts.net", HostName: "cabal", IPs: []string{"100.64.0.9", "fd7a::1"}}
	mini := tailnet.Peer{DNSName: "mini.example.ts.net", HostName: "mini", IPs: []string{"100.64.0.10"}}
	other := tailnet.Peer{DNSName: "web.example.ts.net", HostName: "web", IPs: []string{"100.64.0.11"}}
	if configuredAliasFor(hosts, cabal) != "cabal" {
		t.Fatal("IP-configured peer not recognized")
	}
	if configuredAliasFor(hosts, mini) != "mini" {
		t.Fatal("ssh user@host peer not recognized by short name")
	}
	if configuredAliasFor(hosts, other) != "" {
		t.Fatal("unconfigured node wrongly matched")
	}
}
