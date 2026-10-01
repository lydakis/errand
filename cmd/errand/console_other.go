//go:build !windows

package main

import "os"

func detachServiceConsole(*os.File) {}

// Unix service managers stop the runner with SIGTERM and record it.
func logServiceStop() {}
