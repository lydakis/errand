package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

func TestTyposAndMissingSeparatorsGetOneLineFixes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"stauts", "mini/01M3BFTQ6QD4"}, []string{"unknown command 'stauts'", "did you mean errand status?", "errand -- stauts mini/01M3BFTQ6QD4"}},
		{[]string{"make", "test"}, []string{"unknown command 'make'", "errand -- make test"}},
		{[]string{"--on", "mini", "go", "test"}, []string{`missing "--" before the command`, "errand --on mini -- go test"}},
	} {
		var out bytes.Buffer
		e := termui.Plain(&out, &out).Err
		if code := unknownCommand(e, tc.args); code != 2 {
			t.Fatalf("%v exit = %d", tc.args, code)
		}
		for _, want := range tc.want {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%v output lacks %q:\n%s", tc.args, want, &out)
			}
		}
		if strings.Count(out.String(), "\n") > 2 {
			t.Fatalf("%v printed more than an error and a hint:\n%s", tc.args, &out)
		}
	}
}

func TestEveryCommandHelpUsesOneFormat(t *testing.T) {
	commands := map[string]func([]string, *bytes.Buffer, *bytes.Buffer) int{
		"ps":         func(a []string, o, e *bytes.Buffer) int { return cmdPsTo(a, o, e) },
		"status":     func(a []string, o, e *bytes.Buffer) int { return cmdStatusTo(a, o, e) },
		"attach":     func(a []string, o, e *bytes.Buffer) int { return cmdAttachTo(a, o, e) },
		"kill":       func(a []string, o, e *bytes.Buffer) int { return cmdKillTo(a, o, e) },
		"fetch":      func(a []string, o, e *bytes.Buffer) int { return cmdFetchTo(a, o, e) },
		"push":       func(a []string, o, e *bytes.Buffer) int { return cmdPushTo(a, o, e) },
		"workspaces": func(a []string, o, e *bytes.Buffer) int { return cmdWorkspacesTo(a, o, e) },
		"df":         func(a []string, o, e *bytes.Buffer) int { return cmdDfTo(a, o, e) },
		"gc":         func(a []string, o, e *bytes.Buffer) int { return cmdGCTo(a, o, e) },
		"config":     func(a []string, o, e *bytes.Buffer) int { return cmdConfigTo(a, o, e) },
		"access":     func(a []string, o, e *bytes.Buffer) int { return cmdAccessTo(a, o, e) },
		"version":    func(a []string, o, e *bytes.Buffer) int { return cmdVersionTo(a, o, e) },
		"serve":      func(a []string, o, e *bytes.Buffer) int { return cmdServeTo(a, o, e) },
		"setup":      func(a []string, o, e *bytes.Buffer) int { return cmdSetupTo(a, o, e, nil) },
		"peers":      func(a []string, o, e *bytes.Buffer) int { return cmdPeersTo(a, o, e, peersDeps{}) },
		"doctor": func(a []string, o, e *bytes.Buffer) int {
			return cmdDoctorTo(a, o, e, nil)
		},
	}
	for name, run := range commands {
		var out, errOut bytes.Buffer
		if code := run([]string{"--help"}, &out, &errOut); code != 0 {
			t.Fatalf("%s --help exit = %d: %s", name, code, &errOut)
		}
		help := out.String()
		if !strings.Contains(help, "Usage\n") || !strings.Contains(help, "errand "+name) || errOut.Len() != 0 {
			t.Fatalf("%s --help isn't the shared format on stdout:\n%s\nstderr: %s", name, help, &errOut)
		}
		for _, line := range strings.Split(help, "\n") {
			fields := strings.Fields(line)
			// Long options never use Go's single-dash spelling.
			if len(fields) > 0 && strings.HasPrefix(fields[0], "-") && !strings.HasPrefix(fields[0], "--") && len(strings.TrimRight(fields[0], ",")) > 2 {
				t.Fatalf("%s --help has a single-dash long option: %q", name, line)
			}
		}
		if name == "doctor" || name == "config" {
			if strings.Contains(help, "--env-file") {
				t.Fatalf("%s --help lists every run option instead of pointing at errand --help:\n%s", name, help)
			}
		}
	}
}

func TestHelpPairsShortAndLongAliasesOnOneRow(t *testing.T) {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	var all bool
	fs.BoolVar(&all, "all", false, "")
	fs.BoolVar(&all, "a", false, "")
	fs.String("on", "", "")
	rows := helpRows(fs, helpPages["ps"])
	if len(rows) != 2 || rows[0].long != "all" || rows[0].short != "a" || rows[1].long != "on" || rows[1].arg != "PEER" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestBadFlagsNameTheOptionAndPointAtHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cmdPsTo([]string{"--bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	if got := errOut.String(); got != "errand: error: unknown option --bogus\nerrand: hint: errand ps --help\n" {
		t.Fatalf("bad flag = %q", got)
	}
	errOut.Reset()
	if code := cmdPsTo([]string{"-n", "many"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), `invalid value "many" for -n`) {
		t.Fatalf("bad value = %d %q", code, errOut.String())
	}
}

func TestShortJobIDsResolveToTheOneMatchingJob(t *testing.T) {
	jobs := []proto.JobListEntry{{ID: "01M3BFTQ6QD4GRXZKC3F4PTG15"}, {ID: "01M3BFTQX1WFQKGP1SP1RCDYW2"}}
	var prefixes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefixes = append(prefixes, r.URL.Query().Get("prefix"))
		var matches []proto.JobListEntry
		for _, j := range jobs {
			if strings.HasPrefix(j.ID, r.URL.Query().Get("prefix")) {
				matches = append(matches, j)
			}
		}
		json.NewEncoder(w).Encode(matches)
	}))
	defer server.Close()

	_, _, id, err := resolveHandle("01m3bftq6qd4", server.URL, "")
	if err != nil || id != jobs[0].ID || prefixes[0] != "01M3BFTQ6QD4" {
		t.Fatalf("short id resolved to %q, %v (asked %v)", id, err, prefixes)
	}
	_, _, _, err = resolveHandle("01M3BFTQ", server.URL, "")
	var prefix *client.JobPrefixError
	if !errors.As(err, &prefix) || len(prefix.Matches) != 2 {
		t.Fatalf("ambiguous prefix = %v", err)
	}
	msg, hint := describeError(err, errorScope{peer: "mini"})
	if msg != "01M3BFTQ matches 2 jobs on mini" || hint == "" {
		t.Fatalf("ambiguous message = %q / %q", msg, hint)
	}
	_, hint = describeError(&client.JobPrefixError{Prefix: "01M3ZZZZ"}, errorScope{peer: server.URL})
	if !strings.Contains(hint, "--url "+server.URL) || strings.Contains(hint, "--on") {
		t.Fatalf("a job found by --url got a --on hint: %q", hint)
	}
	if _, _, _, err := resolveHandle("mini/ab", "", ""); err == nil {
		t.Fatal("a two-character id must not be accepted")
	} else if msg, _ := describeError(err, errorScope{}); msg != `"mini/ab" isn't a job handle` {
		t.Fatalf("bad handle message = %q", msg)
	}
	full := proto.NewULID()
	before := len(prefixes)
	if _, _, id, err := resolveHandle(full, server.URL, ""); err != nil || id != full || len(prefixes) != before {
		t.Fatalf("a full id must not need a lookup: %q %v", id, err)
	}
}

func TestStatusQuotesRunnerTextAndKeepsAmbiguousStartErrors(t *testing.T) {
	s := termui.Plain(io.Discard, io.Discard).Out
	d := proto.JobDetails{JobStatus: proto.JobStatus{State: proto.StateAmbiguous, Result: &proto.Result{StartError: "exec format error"}}}
	if line := statusLine(s, "mini", d, time.Now()); !strings.Contains(line, "state unknown") || !strings.Contains(line, "couldn't start: exec format error") {
		t.Fatalf("ambiguous verdict lost the start error: %q", line)
	}
	issues := statusProblems(&proto.Result{TransactionError: "bad\x1b[2Jpath"})
	if len(issues) == 0 || strings.ContainsRune(strings.Join(issues, ""), '\x1b') {
		t.Fatalf("transaction error reached the terminal unquoted: %q", issues)
	}
}

func TestRunnerErrorsReadAsSentencesAboutTheThingAskedFor(t *testing.T) {
	writeClientConfig(t, "default_peer='cabal'\n[peers.cabal]\nurl='http://cabal.invalid'\n[peers.mini]\nurl='http://mini.invalid'\n")
	msg, hint := describeError(&config.UnknownPeerError{Name: "nope"}, errorScope{})
	if msg != "no runner named nope" || !strings.Contains(hint, "you have cabal and mini") {
		t.Fatalf("unknown peer = %q / %q", msg, hint)
	}
	_, hint = describeError(&config.UnknownPeerError{Name: "mnii"}, errorScope{})
	if !strings.Contains(hint, "did you mean mini?") {
		t.Fatalf("typo'd peer hint = %q", hint)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no such job"}`, http.StatusNotFound)
	}))
	defer server.Close()
	_, err := client.GetJobDetails(server.URL, "01M3BFYR7PQXA36M5Z5C4W47XZ")
	msg, hint = describeError(err, errorScope{peer: "mini", job: "01M3BFYR7PQXA36M5Z5C4W47XZ"})
	if msg != "mini has no job 01M3BFYR7PQXA36M5Z5C4W47XZ" || strings.Contains(msg, "404") || !strings.Contains(hint, "errand ps -a --on mini") {
		t.Fatalf("missing job = %q / %q", msg, hint)
	}
	_, err = client.RemoveWorkspace(server.URL, "gone")
	msg, _ = describeError(err, errorScope{peer: "mini", workspace: "gone"})
	if msg != "mini has no workspace named gone" {
		t.Fatalf("missing workspace = %q", msg)
	}
	var stderr bytes.Buffer
	if code := failWith(termui.Plain(&stderr, &stderr).Err, 2, fmt.Errorf("wrapped: %w", &config.UnknownPeerError{Name: "nope"}), errorScope{}); code != 2 ||
		!strings.HasPrefix(stderr.String(), "errand: error: no runner named nope\nerrand: hint: ") {
		t.Fatalf("failWith = %d %q", code, stderr.String())
	}
}
