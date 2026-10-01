//go:build !darwin && !linux

package termui

func isTerminal(uintptr) bool { return false }

func terminalColumns(uintptr) int { return 0 }
