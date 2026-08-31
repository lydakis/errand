//go:build !darwin && !linux

package main

func fileTerminalColumns(uintptr) int { return 0 }
