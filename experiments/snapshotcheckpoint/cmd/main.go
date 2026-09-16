//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	checkpoint "github.com/lydakis/errand/experiments/snapshotcheckpoint"
	"github.com/lydakis/errand/internal/snapshot"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root, cache, mode := flag.String("root", "", "fixture root"), flag.String("cache", "", "private cache directory"), flag.String("mode", "cold", "cold, checkpoint, current, derived, journal, observation-journal, or observation-replacement")
	cpuPath := flag.String("cpu-profile", "", "write diagnostic CPU profile (not for timing comparisons)")
	heapPath := flag.String("heap-profile", "", "write diagnostic allocation profile (not for timing comparisons)")
	flag.Parse()
	if *heapPath != "" {
		runtime.MemProfileRate = 64 << 10
	}
	if *cpuPath != "" {
		file, err := os.Create(*cpuPath)
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(file); err != nil {
			file.Close()
			return err
		}
		defer file.Close()
		defer pprof.StopCPUProfile()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	start := time.Now()
	var result checkpoint.Result
	var err error
	switch *mode {
	case "cold":
		result, err = checkpoint.Cold(ctx, *root, snapshot.SelectOptions{})
	case "current":
		result, err = checkpoint.PrepareCurrent(ctx, *root, *cache, snapshot.SelectOptions{})
	case "checkpoint":
		result, err = checkpoint.Prepare(ctx, *root, *cache, snapshot.SelectOptions{})
	case "derived", "journal":
		result, err = checkpoint.PrepareDerived(ctx, *root, *cache, snapshot.SelectOptions{}, *mode == "journal")
	case "observation-replacement":
		result, err = checkpoint.PrepareObservationReplacement(ctx, *root, *cache, snapshot.SelectOptions{})
	case "observation-journal":
		result, err = checkpoint.PrepareObservationJournal(ctx, *root, *cache, snapshot.SelectOptions{})
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		return err
	}
	wireStart := time.Now()
	hash, err := result.State.RootHash(ctx)
	if err != nil {
		return err
	}
	wire, total := time.Since(wireStart), time.Since(start)
	// Do not attribute the diagnostic forced GC or profile serialization to
	// preparation. Profiled samples are excluded from comparison summaries.
	if *cpuPath != "" {
		pprof.StopCPUProfile()
	}
	result.State = nil
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Result      checkpoint.Result
		Hash        string
		Wire, Total time.Duration
	}{result, hash, wire, total}); err != nil {
		return err
	}
	if *heapPath != "" {
		runtime.GC()
		file, err := os.Create(*heapPath)
		if err != nil {
			return err
		}
		err = pprof.WriteHeapProfile(file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	return nil
}
