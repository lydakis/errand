//go:build windows

package nowindow

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

const childEnv = "ERRAND_NOWINDOW_CHILD"

var procGetConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		// Report whether this process got a console window.
		if window, _, _ := procGetConsoleWindow.Call(); window == 0 {
			os.Stdout.WriteString("no window")
		} else {
			os.Stdout.WriteString("window")
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestHideStartsConsoleProgramWithoutWindowOnWindows(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_UNICODE_ENVIRONMENT}
	Hide(cmd)
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_UNICODE_ENVIRONMENT == 0 {
		t.Fatal("Hide dropped existing creation flags")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "no window" {
		t.Fatalf("child reported %q", got)
	}
}
