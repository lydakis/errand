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
func detachServiceConsole() {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n == 1 {
		procFreeConsole.Call()
	}
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
