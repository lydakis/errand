//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	checkpoint "github.com/lydakis/errand/experiments/snapshotcheckpoint"
	"github.com/lydakis/errand/internal/snapshot"
)

func main() {
	root, cache, mode := flag.String("root", "", "fixture root"), flag.String("cache", "", "private cache directory"), flag.String("mode", "cold", "cold, checkpoint, current, derived, journal, or observation-journal")
	flag.Parse()
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
	case "observation-journal":
		result, err = checkpoint.PrepareObservationJournal(ctx, *root, *cache, snapshot.SelectOptions{})
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	wireStart := time.Now()
	hash, err := result.State.RootHash(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	result.State = nil
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Result      checkpoint.Result
		Hash        string
		Wire, Total time.Duration
	}{result, hash, time.Since(wireStart), time.Since(start)}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
