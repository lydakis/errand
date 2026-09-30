package setup

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/lydakis/errand/internal/serviceruntime"
)

// Windows runs the daemon as a Task Scheduler task for the signed-in user. It
// needs no administrator rights and, like a LaunchAgent, runs while the user
// is logged in.
const scheduledTaskName = DefaultServiceName

// The task queries go through PowerShell because its state names are not
// localized, unlike schtasks output.
const scheduledTaskQuery = `$t = Get-ScheduledTask -TaskPath '\' -TaskName '` + scheduledTaskName + `' -ErrorAction SilentlyContinue; if ($t) { "$($t.State)"; $t.Actions[0].Execute } else { 'Missing' }`

func windowsServiceDir(home string) string {
	return filepath.Join(home, "AppData", "Local", "errand")
}

func scheduledTaskPath(home string) string {
	return filepath.Join(windowsServiceDir(home), "errand-task.xml")
}

type scheduledTaskState struct {
	State   string
	Command string
}

func queryScheduledTask(ctx context.Context, sys serviceSystem) (scheduledTaskState, error) {
	out, err := sys.Run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", scheduledTaskQuery)
	if err != nil {
		return scheduledTaskState{}, err
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(out), "\r\n", "\n"), "\n")
	state := scheduledTaskState{State: strings.TrimSpace(lines[0])}
	if len(lines) > 1 {
		state.Command = strings.TrimSpace(lines[1])
	}
	if state.State == "" {
		return scheduledTaskState{}, fmt.Errorf("Task Scheduler returned no state for %s", scheduledTaskName)
	}
	return state, nil
}

// installScheduledTask registers the task to run the retained runtime copy, so
// replacing errand.exe never fights a running daemon for the file. Each upgrade
// moves the task to the new copy when setup runs again.
func installScheduledTask(ctx context.Context, opts Options, sys System, r *Report, home, exe, configPath, stateDir string) bool {
	r.Service = scheduledTaskName
	taskPath := scheduledTaskPath(home)
	r.ServicePath = taskPath
	logPath := filepath.Join(windowsServiceDir(home), "errand.log")
	runtimeExe, err := sys.RuntimePath(exe, stateDir, !opts.DryRun)
	if err != nil {
		r.fail("service", fmt.Errorf("preparing the runner's retained copy of %s: %w", exe, err))
		return false
	}
	user, err := sys.UserSID()
	if err != nil {
		r.fail("service", fmt.Errorf("reading the current user's SID: %w", err))
		return false
	}
	desired := renderScheduledTask(user, runtimeExe, configPath, logPath)
	changed, ok := writeTaskDefinition(sys, r, taskPath, desired, user, configPath, logPath, stateDir, opts)
	if !ok {
		return false
	}
	if opts.DryRun {
		r.step("service", "would run: schtasks /Create /TN "+scheduledTaskName+" /XML "+taskPath+" /F && schtasks /Run /TN "+scheduledTaskName, true)
		return false
	}
	current, err := queryScheduledTask(ctx, sys)
	if err != nil {
		r.fail("service", fmt.Errorf("cannot query Task Scheduler: %w", err))
		return false
	}
	if current.State == "Running" {
		if _, err := sys.Run(ctx, "schtasks", "/End", "/TN", scheduledTaskName); err != nil {
			r.fail("service", fmt.Errorf("cannot stop the running task: %w", err))
			return false
		}
	}
	if _, err := sys.Run(ctx, "schtasks", "/Create", "/TN", scheduledTaskName, "/XML", taskPath, "/F"); err != nil {
		r.fail("service", err)
		return false
	}
	if _, err := sys.Run(ctx, "schtasks", "/Run", "/TN", scheduledTaskName); err != nil {
		r.fail("service", err)
		return false
	}
	if changed || current.State == "Missing" {
		r.step("service", "registered and started scheduled task "+scheduledTaskName, true)
	} else {
		r.step("service", "restarted scheduled task "+scheduledTaskName+" so the preserved config is active", true)
	}
	r.step("logon", "the task starts when "+sys.Username()+" signs in; the runner stops while nobody is signed in", false)
	return true
}

// writeTaskDefinition is writeDefinition for a task whose command moves to a
// new runtime copy on every upgrade. A definition setup rendered for any
// retained copy is setup's own and is replaced. Anything else is an
// operator's: registering it would restart an unknown command, so setup
// stops unless --force replaces it.
func writeTaskDefinition(sys System, r *Report, path, desired, user, configPath, logPath, stateDir string, opts Options) (changed, ok bool) {
	if sys.Exists(path) {
		current, err := sys.ReadFile(path)
		if err != nil {
			r.fail("service", err)
			return false, false
		}
		if string(current) == desired {
			r.step("service", "definition unchanged at "+path, false)
			return false, true
		}
		if !opts.Force && !setupRenderedTask(string(current), user, configPath, logPath, stateDir) {
			r.fail("service", fmt.Errorf("%s differs from what setup would write; inspect it and rerun setup --force to replace it", path))
			return false, false
		}
	}
	if opts.DryRun {
		r.step("service", "would write "+path, true)
		return true, true
	}
	if err := sys.WriteFile(path, []byte(desired), 0o600); err != nil {
		r.fail("service", err)
		return false, false
	}
	r.step("service", "wrote "+path, true)
	return true, true
}

func setupRenderedTask(existing, user, configPath, logPath, stateDir string) bool {
	command, err := taskCommand(existing)
	if err != nil {
		return false
	}
	runtimeDir, err := serviceruntime.Directory(stateDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(runtimeDir, command)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return false
	}
	return existing == renderScheduledTask(user, command, configPath, logPath)
}

func taskCommand(definition string) (string, error) {
	text, err := decodeUTF16(definition)
	if err != nil {
		return "", err
	}
	var task struct {
		Command string `xml:"Actions>Exec>Command"`
	}
	// The declaration names UTF-16, which the decoder has already undone.
	text = strings.Replace(text, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
	if err := xml.Unmarshal([]byte(text), &task); err != nil {
		return "", err
	}
	if task.Command == "" {
		return "", errors.New("task has no command")
	}
	return task.Command, nil
}

// renderScheduledTask is the task definition in the UTF-16 encoding schtasks
// requires for /XML.
func renderScheduledTask(user, executable, configPath, logPath string) string {
	arguments := strings.Join([]string{"serve", "--config", windowsArg(configPath), "--log-file", windowsArg(logPath)}, " ")
	// Priority 7 is Task Scheduler's default and runs the daemon, and every
	// job it starts, below normal priority. 4 is normal.
	text := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>errand runner</Description>
    <URI>\%s</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>4</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
    </Exec>
  </Actions>
</Task>
`, xmlText(scheduledTaskName), xmlText(user), xmlText(user), xmlText(executable), xmlText(arguments))
	return encodeUTF16(strings.ReplaceAll(text, "\n", "\r\n"))
}

func encodeUTF16(text string) string {
	var out bytes.Buffer
	out.Write([]byte{0xff, 0xfe})
	for _, unit := range utf16.Encode([]rune(text)) {
		_ = binary.Write(&out, binary.LittleEndian, unit)
	}
	return out.String()
}

func decodeUTF16(data string) (string, error) {
	raw := []byte(data)
	if len(raw) < 2 || raw[0] != 0xff || raw[1] != 0xfe || len(raw)%2 != 0 {
		return "", errors.New("not UTF-16LE text")
	}
	units := make([]uint16, 0, len(raw)/2-1)
	for i := 2; i < len(raw); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(raw[i:]))
	}
	return string(utf16.Decode(units)), nil
}

// windowsArg quotes one argument the way CommandLineToArgvW reads it back.
func windowsArg(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\"") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, c := range arg {
		switch c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			slashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
		}
		if c != '\\' {
			b.WriteRune(c)
		}
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}
