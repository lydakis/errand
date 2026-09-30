// The fake system models Unix paths; Windows setup decisions run here too.
//go:build unix

package setup

import (
	"context"
	"os"
	"strings"
	"testing"
)

const testTaskPath = "/home/george/AppData/Local/errand/errand-task.xml"

func newWindowsFake(t *testing.T) *fakeSystem {
	f := newFake(t, "windows")
	f.provider = fakeProvider{name: "cli:tailscale.exe", self: f.provider.(fakeProvider).self}
	f.quiesceErr = os.ErrNotExist // no runner yet
	return f
}

func TestWindowsSetupRegistersALogonTaskForTheRuntimeCopy(t *testing.T) {
	f := newWindowsFake(t)
	r, err := Run(context.Background(), Options{Transport: "tailscale"}, f)
	if err != nil || r.Failed() {
		t.Fatalf("windows setup failed: %v / %+v", err, r.Steps)
	}
	text, err := decodeUTF16(f.files[testTaskPath])
	if err != nil {
		t.Fatalf("task definition is not UTF-16: %v", err)
	}
	for _, want := range []string{
		"<UserId>S-1-5-21-1-2-3-1001</UserId>",
		"<LogonType>InteractiveToken</LogonType>",
		"<Command>/home/george/.errand/runtime/0123abcd/errand.exe</Command>",
		"<Arguments>serve --config /home/george/.config/errand/errandd.toml --log-file /home/george/AppData/Local/errand/errand.log</Arguments>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<Priority>4</Priority>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("task definition missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "\r\n") {
		t.Fatal("task definition should use CRLF line endings")
	}
	if ran(f, "schtasks /End") {
		t.Fatalf("stopped a task that did not exist: %v", f.commands)
	}
	for _, cmd := range []string{"schtasks /Create /TN errand /XML " + testTaskPath + " /F", "schtasks /Run /TN errand"} {
		if !ran(f, cmd) {
			t.Fatalf("expected %q; ran %v", cmd, f.commands)
		}
	}
	if r.Info == nil || !strings.Contains(stepDetail(r, "logon"), "signs in") {
		t.Fatalf("probe/logon = %+v %+v", r.Info, r.Steps)
	}
}

func TestWindowsSetupMovesItsTaskToTheUpgradedRuntime(t *testing.T) {
	f := newWindowsFake(t)
	old := "/home/george/.errand/runtime/ffff0000/errand.exe"
	f.files[testTaskPath] = renderScheduledTask("S-1-5-21-1-2-3-1001", old,
		"/home/george/.config/errand/errandd.toml", "/home/george/AppData/Local/errand/errand.log")
	f.files["/home/george/.config/errand/errandd.toml"] = "transport = \"tailscale\"\nlisten = \"tailnet:7443\"\nmax_jobs = 1\n"
	f.taskState, f.taskCommand, f.processImage = "Running", old, old
	f.quiesceErr = nil

	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() {
		t.Fatalf("upgrade failed: %v / %+v", err, r.Steps)
	}
	if f.taskCommand != "/home/george/.errand/runtime/0123abcd/errand.exe" {
		t.Fatalf("task still runs %s", f.taskCommand)
	}
	endAt, createAt := -1, -1
	for i, cmd := range f.commands {
		if strings.HasPrefix(cmd, "schtasks /End ") {
			endAt = i
		}
		if strings.HasPrefix(cmd, "schtasks /Create ") {
			createAt = i
		}
	}
	if endAt < 0 || createAt < endAt {
		t.Fatalf("expected the running task to stop before re-registering: %v", f.commands)
	}
}

func TestWindowsSetupLeavesAnOperatorTaskAlone(t *testing.T) {
	f := newWindowsFake(t)
	f.files[testTaskPath] = renderScheduledTask("S-1-5-21-1-2-3-1001", `C:\tools\wrapper.exe`,
		"/home/george/.config/errand/errandd.toml", "/home/george/AppData/Local/errand/errand.log")
	r, err := Run(context.Background(), Options{Transport: "tailscale"}, f)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Failed() || !strings.Contains(stepErrorDetail(r, "service"), "--force") {
		t.Fatalf("operator task was not protected: %+v", r.Steps)
	}
	if ran(f, "schtasks") {
		t.Fatalf("changed an operator task: %v", f.commands)
	}

	f = newWindowsFake(t)
	f.files[testTaskPath] = "operator edit"
	r, err = Run(context.Background(), Options{Transport: "tailscale", Force: true}, f)
	if err != nil || r.Failed() {
		t.Fatalf("forced setup failed: %v / %+v", err, r.Steps)
	}
	if !ran(f, "schtasks /Create") {
		t.Fatalf("--force did not replace the task: %v", f.commands)
	}
}

func TestWindowsSetupRefusesARunnerItDidNotStart(t *testing.T) {
	f := newWindowsFake(t)
	f.taskState, f.taskCommand = "Running", "/home/george/.errand/runtime/ffff0000/errand.exe"
	f.processImage = `C:\Users\george\Downloads\errand.exe`
	f.quiesceErr = nil
	r, err := Run(context.Background(), Options{Transport: "tailscale"}, f)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Failed() || !strings.Contains(stepErrorDetail(r, "service"), "Downloads") {
		t.Fatalf("foreign runner was not refused: %+v", r.Steps)
	}
	if ran(f, "schtasks") || len(f.writes) != 0 {
		t.Fatalf("setup changed things before refusing: %v %v", f.commands, f.writes)
	}
}

func TestWindowsDryRunPublishesNothing(t *testing.T) {
	f := newWindowsFake(t)
	r, err := Run(context.Background(), Options{Transport: "tailscale", DryRun: true}, f)
	if err != nil || r.Failed() {
		t.Fatalf("dry run failed: %v / %+v", err, r.Steps)
	}
	if len(f.writes) != 0 || len(f.files) != 0 || ran(f, "schtasks") {
		t.Fatalf("dry run changed things: writes=%v files=%v commands=%v", f.writes, f.files, f.commands)
	}
}

func TestNewWindowsRunnersUseOnlyTheTailnet(t *testing.T) {
	f := newWindowsFake(t)
	r, err := Run(context.Background(), Options{}, f)
	if err != nil || r.Failed() {
		t.Fatalf("windows setup failed: %v / %+v", err, r.Steps)
	}
	if cfg := f.files["/home/george/.config/errand/errandd.toml"]; !strings.Contains(cfg, `transport = "tailscale"`) {
		t.Fatalf("config = %s", cfg)
	}

	f = newWindowsFake(t)
	r, err = Run(context.Background(), Options{Transport: "both"}, f)
	if err != nil || r.Failed() {
		t.Fatalf("windows setup failed: %v / %+v", err, r.Steps)
	}
	if !strings.Contains(stepDetail(r, "path"), "not supported on Windows") || len(f.symlinks) != 0 {
		t.Fatalf("path step = %q, symlinks %v", stepDetail(r, "path"), f.symlinks)
	}
}

func TestWindowsArgRoundTripsThroughTheCommandLineParser(t *testing.T) {
	for arg, want := range map[string]string{
		`C:\Users\george\errandd.toml`:   `C:\Users\george\errandd.toml`,
		`C:\Users\George Lydakis\e.toml`: `"C:\Users\George Lydakis\e.toml"`,
		`C:\dir with space\`:             `"C:\dir with space\\"`,
		`say "hi"`:                       `"say \"hi\""`,
		``:                               `""`,
		`a\\"b`:                          `"a\\\\\"b"`,
	} {
		if got := windowsArg(arg); got != want {
			t.Errorf("windowsArg(%q) = %s, want %s", arg, got, want)
		}
	}
}
