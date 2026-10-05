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
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

func TestServeLogReportsDroppedLifecycleEvents(t *testing.T) {
	for _, tty := range []bool{false, true} {
		var out bytes.Buffer
		con := termui.New(io.Discard, &out, termui.Options{ErrTTY: tty})
		logger := serveLog{e: con.Err}
		logger.job(daemon.JobLogEvent{Kind: daemon.JobLogDropped, Dropped: 42})
		text := termui.StripANSI(out.String())
		if tty {
			if !strings.Contains(text, "dropped 42 job lifecycle log events") {
				t.Fatalf("terminal overflow warning: %q", text)
			}
		} else if !strings.Contains(text, "level=warning") || !strings.Contains(text, "dropped=42") {
			t.Fatalf("service overflow warning: %q", text)
		}
	}
}

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
		if code := unknownCommand(e, recoveryFlagSet(), tc.args); code != 2 {
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

func TestUnknownSubcommandsEscapeControlCharacters(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
	}{
		{"peers", func(args []string, stdout, stderr io.Writer) int {
			return cmdPeersTo(args, stdout, stderr, peersDeps{})
		}},
		{"workspaces", cmdWorkspacesTo},
		{"access", cmdAccessTo},
		{"gc", cmdGCTo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, input := range []string{"bogus\nforged", "bogus\x1b[2J", "bogus\r\t\u0085forged"} {
				var stdout, stderr bytes.Buffer
				if code := tc.run([]string{input}, &stdout, &stderr); code != 2 {
					t.Fatalf("code=%d, want 2", code)
				}
				got := stderr.String()
				if stdout.Len() != 0 || strings.Count(got, "\n") != 2 || strings.ContainsAny(got, "\x1b\r\t\u0085") || !strings.Contains(got, "bogus") || !strings.Contains(got, "hint:") {
					t.Fatalf("input=%q stdout=%q stderr=%q", input, stdout.String(), got)
				}
			}
		})
	}
}

func recoveryFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("errand", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var settings runConfigFlags
	settings.bind(fs)
	var output outputFlags
	output.bind(fs, "")
	fs.Bool("include-all", false, "")
	detach := fs.Bool("detach", false, "")
	fs.BoolVar(detach, "d", false, "")
	return fs
}

func TestRunRecoveryRedactsEnvironmentAndUsesParserOptionBoundaries(t *testing.T) {
	const secret = "review-placeholder-secret"
	for _, option := range []string{"--env", "-env", "-e", "--e"} {
		for _, inline := range []bool{false, true} {
			for _, assignment := range []string{"API_TOKEN=" + secret, secret} {
				args := []string{option, assignment, "echo", "ok"}
				redacted := "<redacted>"
				if strings.HasPrefix(assignment, "API_TOKEN=") {
					redacted = "API_TOKEN=" + redacted
				}
				want := []string{option, redacted, "--", "echo", "ok"}
				if inline {
					args = []string{option + "=" + assignment, "echo", "ok"}
					want = []string{option + "=" + redacted, "--", "echo", "ok"}
				}
				t.Run(option+"/"+fmt.Sprint(inline)+"/"+assignment, func(t *testing.T) {
					original := slices.Clone(args)
					fs := recoveryFlagSet()
					if got := runExample(fs, args); !slices.Equal(got, want) {
						t.Fatalf("example = %q, want %q", got, want)
					}
					var out bytes.Buffer
					unknownCommand(termui.Plain(&out, &out).Err, fs, args)
					if strings.Contains(out.String(), secret) {
						t.Fatalf("environment value leaked in hint: %s", &out)
					}
					if !slices.Equal(args, original) {
						t.Fatalf("arguments changed: %q", args)
					}
					if err := fs.Parse(want); err != nil || !slices.Equal(fs.Args(), []string{"echo", "ok"}) {
						t.Fatalf("hint doesn't parse: %v, command = %q", err, fs.Args())
					}
				})
			}
		}
	}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"-on", "mini", "echo", "ok"}, []string{"-on", "mini", "--", "echo", "ok"}},
		{[]string{"-on=mini", "-quiet=false", "-include-all", "-d", "echo"}, []string{"-on=mini", "-quiet=false", "-include-all", "-d", "--", "echo"}},
		{[]string{"--profile", "-e", "--env", "KEY=value", "echo", "-e", "command-value"}, []string{"--profile", "-e", "--env", "KEY=<redacted>", "--", "echo", "-e", "command-value"}},
		{[]string{"--on", "mini", "-", "--env", "command-value"}, []string{"--on", "mini", "--", "-", "--env", "command-value"}},
		{[]string{"--on", "mini"}, []string{"--on", "mini", "--", "COMMAND"}},
	} {
		fs := recoveryFlagSet()
		got := runExample(fs, tc.args)
		if !slices.Equal(got, tc.want) {
			t.Fatalf("%q example = %q, want %q", tc.args, got, tc.want)
		}
		if err := fs.Parse(got); err != nil {
			t.Fatalf("%q hint doesn't parse: %v", tc.args, err)
		}
		command := got[slices.Index(got, "--")+1:]
		if !slices.Equal(fs.Args(), command) {
			t.Fatalf("%q command = %q, want %q", tc.args, fs.Args(), command)
		}
	}
	for _, args := range [][]string{
		{"--on", "mini", "--env"},
		{"--bogus", "--env=" + secret},
		{"---env", secret},
	} {
		var out bytes.Buffer
		unknownCommand(termui.Plain(&out, &out).Err, recoveryFlagSet(), args)
		if strings.Contains(out.String(), secret) || !strings.Contains(out.String(), "errand --help") {
			t.Fatalf("malformed options didn't get a safe help hint: %s", &out)
		}
	}
}

func TestRecoveryHintsRoundTripThroughShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	command := []string{"printf", "%s", "two words", "", "$(printf expanded)", "'quoted'", "line\nbreak", "\x1b[2J"}
	for _, args := range [][]string{
		append([]string{"-on", "mini"}, command...),
		command,
		append([]string{"stauts"}, command[1:]...),
		append([]string{"bad\ncommand"}, command[1:]...),
		{"--env", "KEY=private-value", "printf", "%s", "two words"},
	} {
		var out bytes.Buffer
		fs := recoveryFlagSet()
		unknownCommand(termui.Plain(&out, &out).Err, fs, args)
		if strings.Count(out.String(), "\n") != 2 || strings.Contains(out.String(), "\x1b") {
			t.Fatalf("hint isn't safe, single-line text: %q", out.String())
		}
		marker := "after --: errand "
		want := append([]string{"--"}, args...)
		if strings.HasPrefix(args[0], "-") {
			marker = "e.g. errand "
			want = runExample(fs, args)
		}
		_, example, found := strings.Cut(out.String(), marker)
		if !found {
			t.Fatalf("hint lacks command example: %s", &out)
		}
		// Print the suggested argv rather than executing errand. A quoting
		// regression would expand the harmless $(printf expanded) fixture.
		got, err := exec.Command(bash, "-c", "printf '%s\\0' errand "+strings.TrimSuffix(example, "\n")).Output()
		want = append([]string{"errand"}, want...)
		if err != nil || string(got) != strings.Join(want, "\x00")+"\x00" {
			t.Fatalf("shell argv = %q, want %q; error = %v", got, want, err)
		}
	}
}

func TestRunnerURLHintsRoundTripThroughShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	check := func(command string, want []string) {
		t.Helper()
		got, err := exec.Command(bash, "-c", "printf '%s\\0' "+command).Output()
		if err != nil || string(got) != strings.Join(want, "\x00")+"\x00" {
			t.Fatalf("hint %q => %q, want %q; error=%v", command, got, want, err)
		}
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	writeClientConfig(t, "")
	d, err := daemon.New(daemon.Config{StateDir: t.TempDir(), InsecureNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/runner&test")
		d.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	peer := server.URL + "/runner&test"
	var stdout, stderr bytes.Buffer
	if code := cmdWorkspacesTo([]string{"create", "--url", peer, "--no-snapshot", "dev"}, &stdout, &stderr); code != 0 {
		t.Fatalf("workspace creation: %d, %s", code, &stderr)
	}
	var hints []string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if command, ok := strings.CutPrefix(line, "errand: next: "); ok {
			command, _, _ = strings.Cut(command, " (")
			hints = append(hints, command)
		}
	}
	if len(hints) != 2 {
		t.Fatalf("workspace hints = %q", hints)
	}
	check(hints[0], []string{"errand", "--url", peer, "--workspace", "dev", "--", "make", "test"})
	check(hints[1], []string{"errand", "push", "--watch", "--apply", "--url", peer, "--workspace", "dev"})
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, suffix := range []string{"/runner&test", "/$(printf expanded)", "/two words"} {
		peer = server.URL + suffix
		handle := peer + "/" + id
		stdout.Reset()
		started := time.Now()
		writeStatus(termui.Plain(&stdout, io.Discard).Out, peer, handle, proto.JobDetails{
			JobStatus: proto.JobStatus{ID: id, State: proto.StateRunning}, StartedAt: &started,
		}, nil, false, started)
		nextCount := 0
		for _, line := range strings.Split(stdout.String(), "\n") {
			if command, ok := strings.CutPrefix(line, "errand: next: "); ok {
				command, _, _ = strings.Cut(command, " (")
				verb := "attach"
				if strings.HasPrefix(command, "errand kill ") {
					verb = "kill"
				}
				check(strings.TrimSpace(command), []string{"errand", verb, handle})
				nextCount++
			}
		}
		if nextCount != 2 {
			t.Fatalf("status next commands = %d, want 2", nextCount)
		}
		hint := applyRecoveryHint(handle, &client.AutomaticApplyStatus{State: "failed"})
		_, command, found := strings.Cut(hint, "workspace: ")
		if !found {
			t.Fatalf("recovery command missing: %s", hint)
		}
		check(command, []string{"errand", "fetch", "--apply", handle})
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
	zero := 0
	d.Result = &proto.Result{ExitCode: &zero, ChangesOK: true, CleanupOK: true, LogsComplete: true}
	if line := statusLine(s, "mini", d, time.Now()); !strings.Contains(line, "state unknown") || !strings.Contains(line, "last seen exiting 0") {
		t.Fatalf("an unconfirmed exit 0 read as a success: %q", line)
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
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"gone\u001b[2J away"}`, http.StatusConflict)
	}))
	defer evil.Close()
	_, err = client.GetJobDetails(evil.URL, "01M3BFYR7PQXA36M5Z5C4W47XZ")
	if msg, _ := describeError(err, errorScope{peer: "mini"}); strings.ContainsRune(msg, '\x1b') || !strings.Contains(msg, "gone") {
		t.Fatalf("runner error text reached the terminal unquoted: %q", msg)
	}
	var stderr bytes.Buffer
	if code := failWith(termui.Plain(&stderr, &stderr).Err, 2, fmt.Errorf("wrapped: %w", &config.UnknownPeerError{Name: "nope"}), errorScope{}); code != 2 ||
		!strings.HasPrefix(stderr.String(), "errand: error: no runner named nope\nerrand: hint: ") {
		t.Fatalf("failWith = %d %q", code, stderr.String())
	}
}

func TestServeLogSaysWhenAJobWasKilledBeforeItStarted(t *testing.T) {
	_, queued := serveOutcome(daemon.JobLogEvent{Result: &proto.Result{Signal: "terminated", SignalNum: 15}})
	_, ran := serveOutcome(daemon.JobLogEvent{Result: &proto.Result{Signal: "terminated", SignalNum: 15, Started: true, DurationMS: 1500, LogsComplete: true, ChangesOK: true, CleanupOK: true}})
	if queued != "killed by SIGTERM before the command started" || ran != "killed by SIGTERM after 1.5s" {
		t.Fatalf("serve outcomes = %q / %q", queued, ran)
	}
	zero := 0
	glyph, unconfirmed := serveOutcome(daemon.JobLogEvent{Result: &proto.Result{State: proto.StateAmbiguous, ExitCode: &zero, Started: true}})
	if glyph == termui.OK || !strings.Contains(unconfirmed, "state unknown") {
		t.Fatalf("an unconfirmed exit 0 was logged as success: %q", unconfirmed)
	}
	glyph, failed := serveOutcome(daemon.JobLogEvent{Result: &proto.Result{ExitCode: &zero, Started: true, TransactionError: "persisting result: disk full"}})
	if glyph != termui.Fail || !strings.Contains(failed, "disk full") {
		t.Fatalf("a failed transaction after exit 0 = %q", failed)
	}
}

func TestServeLogReportsTransactionOutcomes(t *testing.T) {
	zero, nonzero := 0, 7
	for _, tc := range []struct {
		name   string
		change func(*proto.Result)
		want   []string
		fields []string
		ok     bool
	}{
		{"success", func(*proto.Result) {}, nil, []string{"logs_complete=true", "changes_ok=true", "cleanup_ok=true"}, true},
		{"log cap after exit zero", func(r *proto.Result) { r.LimitExceeded = "log_bytes"; r.LogsComplete = false }, []string{"exited 0", "hit the log_bytes limit", "logs are incomplete"}, []string{"limit_exceeded=log_bytes", "logs_complete=false"}, false},
		{"log cap after nonzero", func(r *proto.Result) { r.ExitCode = &nonzero; r.LimitExceeded = "log_bytes"; r.LogsComplete = false }, []string{"exited 7", "hit the log_bytes limit", "logs are incomplete"}, []string{"exit=7", "limit_exceeded=log_bytes", "logs_complete=false"}, false},
		{"log cap after signal", func(r *proto.Result) {
			r.ExitCode = nil
			r.Signal = "killed"
			r.SignalNum = 9
			r.LimitExceeded = "log_bytes"
			r.LogsComplete = false
		}, []string{"killed by SIGKILL", "hit the log_bytes limit"}, []string{"signal=SIGKILL", "limit_exceeded=log_bytes"}, false},
		{"incomplete logs", func(r *proto.Result) { r.LogsComplete = false }, []string{"logs are incomplete"}, []string{"logs_complete=false"}, false},
		{"failed changes", func(r *proto.Result) { r.ChangesOK = false }, []string{"changed files weren't kept"}, []string{"changes_ok=false"}, false},
		{"failed cleanup", func(r *proto.Result) { r.CleanupOK = false }, []string{"cleanup on the runner didn't finish"}, []string{"cleanup_ok=false"}, false},
		{"unsafe limit", func(r *proto.Result) { r.LimitExceeded = "log_bytes\nforged" }, []string{`log_bytes\nforged`}, []string{`limit_exceeded="log_bytes\nforged"`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := proto.Result{State: proto.StateExited, Started: true, ExitCode: &zero, LogsComplete: true, ChangesOK: true, CleanupOK: true}
			tc.change(&res)
			event := daemon.JobLogEvent{Kind: daemon.JobLogFinished, ID: proto.NewULID(), Result: &res}
			glyph, text := serveOutcome(event)
			if (glyph == termui.OK) != tc.ok {
				t.Fatalf("outcome glyph = %v: %q", glyph, text)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("outcome %q missing %q", text, want)
				}
			}
			var buf bytes.Buffer
			serveLog{e: termui.Plain(io.Discard, &buf).Err}.job(event)
			for _, want := range tc.fields {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("service log %q missing %q", buf.String(), want)
				}
			}
			if strings.Count(buf.String(), "\n") != 1 || strings.Contains(text, "\n") {
				t.Fatalf("outcome contains unescaped controls: %q / %q", text, buf.String())
			}
		})
	}
}

func TestPlacementNoteQuotesRunnerSuppliedReasons(t *testing.T) {
	note := placementNote("os=darwin", []placementExclusion{
		{Peer: "cabal", Reason: "unreachable: \x1b[2Jspoof"},
		{Peer: "mini", Reason: "os=darwin", info: &proto.Info{Facts: proto.Facts{OS: "linux\x1b]0;x\x07"}}},
	})
	if strings.ContainsRune(note, '\x1b') || !strings.HasPrefix(note, "matched os=darwin (cabal skipped: ") {
		t.Fatalf("placement note = %q", note)
	}
}

func TestServeLogFieldsQuoteControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	serveLog{e: termui.Plain(&buf, &buf).Err}.kv("info", "job started", "project", "evil\nforged")
	if strings.Count(buf.String(), "\n") != 1 || !strings.Contains(buf.String(), `project="evil\nforged"`) {
		t.Fatalf("a project name split the log line: %q", buf.String())
	}
}

func TestServeLogKeepsAmbiguityForEveryObservedOutcome(t *testing.T) {
	zero, failed := 0, 7
	for _, tc := range []struct {
		name string
		res  proto.Result
		want string
	}{
		{"exit zero", proto.Result{ExitCode: &zero}, "last seen exiting 0"},
		{"exit nonzero", proto.Result{ExitCode: &failed}, "last seen exiting 7"},
		{"signal", proto.Result{Signal: "terminated", SignalNum: 15, Started: true}, "last seen killed by SIGTERM"},
		{"queued cancellation", proto.Result{Signal: "terminated", SignalNum: 15}, "last seen killed by SIGTERM"},
		{"start error", proto.Result{StartError: "exec format error"}, "couldn't start: exec format error"},
		{"no outcome", proto.Result{}, "state unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.res.State = proto.StateAmbiguous
			event := daemon.JobLogEvent{Kind: daemon.JobLogFinished, ID: proto.NewULID(), Result: &tc.res}
			glyph, text := serveOutcome(event)
			if glyph != termui.Warn || !strings.Contains(text, "state unknown") || !strings.Contains(text, tc.want) {
				t.Fatalf("ambiguous verdict = %+v %q", glyph, text)
			}
			var buf bytes.Buffer
			serveLog{e: termui.Plain(io.Discard, &buf).Err}.job(event)
			if !strings.Contains(buf.String(), "state=ambiguous") {
				t.Fatalf("service log lost ambiguity: %q", buf.String())
			}
		})
	}
}

func TestVersionQuotesRunnerSuppliedVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(proto.Info{Proto: proto.ProtoVersion, Version: "9.9\x1b[2J"})
	}))
	defer server.Close()
	offline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer offline.Close()
	writeClientConfig(t, fmt.Sprintf("default_peer='mini'\n[peers.mini]\nurl=%q\n[peers.\"offline\\u001b[2J\"]\nurl=%q\n", server.URL, offline.URL))
	var out, errOut bytes.Buffer
	if code := cmdVersionTo([]string{"-v"}, &out, &errOut); code != 0 || strings.ContainsRune(out.String(), '\x1b') || !strings.Contains(out.String(), "mini") || !strings.Contains(out.String(), "unreachable") {
		t.Fatalf("version -v = %d %q", code, &out)
	}
}
