package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func processBootID() (string, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(boot))
	if id == "" {
		return "", fmt.Errorf("machine boot identity is unavailable")
	}
	return id, nil
}

func inspectProcessGroup(pgid int) (processGroupSnapshot, error) {
	return inspectProcessGroupWith(pgid, os.ReadFile)
}

func inspectProcessGroupWith(pgid int, readFile func(string) ([]byte, error)) (processGroupSnapshot, error) {
	var result processGroupSnapshot
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := readFile(filepath.Join("/proc", entry.Name(), "stat"))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return result, err
		}
		// comm can contain spaces and closing parentheses. Fields after its
		// final ')' begin with state (3), ppid (4), pgrp (5).
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			return result, fmt.Errorf("invalid process stat for %d", pid)
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 20 {
			return result, fmt.Errorf("short process stat for %d", pid)
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return result, err
		}
		if group != pgid {
			continue
		}
		if pid == pgid {
			result.leaderBirth = fields[19]
		}
		if fields[0] != "Z" && fields[0] != "X" {
			result.pids = append(result.pids, pid)
		}
	}
	return result, nil
}
