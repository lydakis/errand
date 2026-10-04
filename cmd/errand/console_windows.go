//go:build windows

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
)

// Task Scheduler gives a console program its own visible console window. When
// the runner is that console's only process, nobody is reading it, so close
// it. A runner started from a terminal shares the console and keeps it.
//
// Detaching closes the console's handles, but the process's standard handles
// still name them. Those values are soon reused by other objects, so point
// the standard handles at the log and NUL before anything can use them.
func detachServiceConsole(logFile *os.File) {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n != 1 {
		return
	}
	nul, err := os.Open(os.DevNull)
	if err != nil {
		return // keep the console rather than leave stale handles
	}
	procFreeConsole.Call()
	retiredStdio = append(retiredStdio, os.Stdin)
	os.Stdin = nul
	_ = windows.SetStdHandle(windows.STD_INPUT_HANDLE, windows.Handle(nul.Fd()))
	_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(logFile.Fd()))
	_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(logFile.Fd()))
}

// logServiceStop records why the runner stops on a console event (closing
// its window, signing out, shutting down). Unhandled, Go exits with status 2
// and leaves nothing in the log.
func logServiceStop() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		log.Printf("errand serve: stopping on %v", <-stop)
		os.Exit(1)
	}()
}
