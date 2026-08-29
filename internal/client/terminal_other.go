//go:build !darwin && !linux

package client

import (
	"os"
	"time"
)

func isTerminalFile(_ *os.File) bool { return false }

func waitTerminalReadable(_ *os.File, _ time.Duration) (bool, error) { return false, nil }
