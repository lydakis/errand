package config

import (
	"github.com/lydakis/errand/internal/workspace"
	"os"
	"strings"
	"testing"
)

func TestWhereSourcesPreservePercentInPaths(t *testing.T) {
	where := "*"
	personal := Client{Peers: map[string]Peer{"runner": {URL: "http://runner:7443"}}}
	for _, profile := range []bool{false, true} {
		out := EffectiveRun{Sources: map[string]string{}}
		selection := workspace.Selection{}
		p := workspace.Profile{}
		want := "/project%20name/.errand.toml (run.where)"
		if profile {
			p.Run.Where = &where
			want = "/project%20name/.errand.toml profile%go run.where"
		} else {
			selection.Where = &where
		}
		if err := resolvePlacement(&out, personal, selection, p, RunOverrides{}, "/personal%20name", "/project%20name/.errand.toml", "/project%20name/.errand.toml profile%go"); err != nil {
			t.Fatal(err)
		}
		if out.Sources["where"] != want {
			t.Fatalf("source=%q want=%q", out.Sources["where"], want)
		}
	}
}

func TestWherePrecedenceAndPersonalCandidates(t *testing.T) {
	root := runFixture(t, personalPeers, `[run]
where = "os=linux"
[profiles.mac.run]
where = "os=darwin"
[profiles.pin.run]
peer = "mac"
`)
	for _, tc := range []struct {
		name        string
		cli         RunOverrides
		where, peer string
	}{
		{"workspace", RunOverrides{}, "os=linux", ""},
		{"profile", RunOverrides{Profile: "mac"}, "os=darwin", ""},
		{"profile pin", RunOverrides{Profile: "pin"}, "", "mac"},
		{"CLI requirements", RunOverrides{Profile: "pin", Where: "*"}, "*", ""},
		{"CLI pin", RunOverrides{Peer: "linux"}, "", "linux"},
		{"CLI URL", RunOverrides{URL: "http://explicit:7443"}, "", "http://explicit:7443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := ResolveRun(root, tc.cli)
			if err != nil {
				t.Fatal(err)
			}
			if e.Where != tc.where || e.Peer != tc.peer {
				t.Fatalf("resolved %+v", e)
			}
			if tc.where != "" && (e.URL != "" || len(e.Candidates) != 2 || e.Candidates[1].RemoteCommand != "/opt/bin/errand") {
				t.Fatalf("lost candidates: %+v", e)
			}
		})
	}
}

func TestWhereLocalRequiresPersonalOptIn(t *testing.T) {
	for _, tc := range []struct {
		extra string
		count int
	}{
		{"", 1}, {"\n[peers.local]\n", 2},
	} {
		t.Run(tc.extra, func(t *testing.T) {
			root := runFixture(t, "default_where = '*'\n[peers.remote]\nurl='http://remote:7443'\n"+tc.extra, "")
			path, _ := DaemonPath()
			if err := os.WriteFile(path, []byte("transport = 'local'\nsocket = '/tmp/where-test.sock'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			e, err := ResolveRun(root, RunOverrides{})
			if err != nil {
				t.Fatal(err)
			}
			if len(e.Candidates) != tc.count {
				t.Fatalf("candidates=%+v", e.Candidates)
			}
		})
	}
	root := runFixture(t, "default_peer='local'", "[run]\nwhere='*'")
	e, err := ResolveRun(root, RunOverrides{})
	if err != nil || len(e.Candidates) != 1 || e.Candidates[0].Name != "local" {
		t.Fatalf("explicit local default: %+v %v", e, err)
	}
}

func TestWhereRejectsInvalidAndAmbiguousSettings(t *testing.T) {
	for _, project := range []string{"[run]\nwhere=''", "[run]\nwhere='gpu'", "[run]\nwhere='*'\npeer='linux'"} {
		root := runFixture(t, personalPeers, project)
		if _, err := ResolveRun(root, RunOverrides{}); err == nil {
			t.Fatalf("accepted %s", project)
		}
	}
	root := runFixture(t, strings.Replace(personalPeers, "default_peer", "default_where", 1), "")
	if _, err := ResolveRun(root, RunOverrides{}); err == nil {
		t.Fatal("accepted invalid personal selector")
	}
	for _, cli := range []RunOverrides{{Where: "*", Peer: "linux"}, {Where: "*", URL: "http://host"}} {
		if _, err := ResolveRun(root, cli); err == nil {
			t.Fatal("accepted contradictory CLI selectors")
		}
	}
}

func TestAddPeerPreservesAutomaticDefault(t *testing.T) {
	root := runFixture(t, "default_where='*'\n", "")
	path, err := ClientPath()
	if err != nil {
		t.Fatal(err)
	}
	made, err := AddPeer(path, "runner", Peer{URL: "http://runner:7443"}, false)
	if err != nil || made {
		t.Fatalf("add changed selection mode: made=%v err=%v", made, err)
	}
	e, err := ResolveRun(root, RunOverrides{})
	if err != nil || e.Where != "*" || len(e.Candidates) != 1 {
		t.Fatalf("resolved=%+v %v", e, err)
	}
}
