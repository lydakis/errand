//go:build !windows

package main

func detachServiceConsole() {}

// Unix service managers stop the runner with SIGTERM and record it.
func logServiceStop() {}
