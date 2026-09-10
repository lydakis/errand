package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

type dfRow struct {
	Details     *proto.StorageDetails  `json:"details,omitempty"`
	Workspaces  *proto.StorageCategory `json:"workspaces,omitempty"`
	hasRunner   bool
	NamedCaches *proto.NamedCacheStats `json:"named_caches,omitempty"`
	Location    string                 `json:"location"`
	Cache       *proto.CacheStats      `json:"cache,omitempty"`
	Jobs        proto.StorageCategory  `json:"jobs"`
	Changes     *proto.StorageCategory `json:"changes,omitempty"`
	TotalBytes  int64                  `json:"total_bytes"`
}

func cmdDf(args []string) int {
	return cmdDfTo(args, os.Stdout, os.Stderr)
}

func cmdDfTo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("errand df", flag.ContinueOnError)
	fs.SetOutput(stderr)
	on := fs.String("on", "", "restrict to one peer name")
	rawURL := fs.String("url", "", "restrict to one peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	verbose := false
	fs.BoolVar(&verbose, "verbose", false, "show individual workspace, named-cache, and job storage")
	fs.BoolVar(&verbose, "v", false, "show individual workspace, named-cache, and job storage")
	setFlagUsage(fs, "errand df [options]")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand: unexpected df arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	query := client.StorageStats
	if verbose {
		query = client.StorageStatsDetailed
	}
	read, err := readFleet(*rawURL, *on, stderr, query)
	if err != nil && !errors.Is(err, errNoUsablePeers) {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	changes, err := client.ChangeStorageStats(ctx)
	var localChanges *proto.ChangeStorageStats
	if err != nil {
		fmt.Fprintf(stderr, "errand: local changes: %v\n", err)
		read.failed = true
	} else {
		localChanges = &changes
	}
	rows := storageRows(read.results, localChanges)

	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "errand: encoding storage usage: %v\n", err)
			return 1
		}
	} else {
		writeDf(stdout, rows)
		if verbose {
			writeDfDetails(stdout, rows)
		}
	}
	if read.failed {
		return 1
	}
	return 0
}

func writeDf(w io.Writer, rows []dfRow) {
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "LOCATION\tCACHE\tNAMED CACHES\tWORKSPACES\tJOBS\tCHANGES\tTOTAL")
	for _, row := range rows {
		cache := "-"
		if row.Cache != nil {
			cache = formatByteSize(row.Cache.Bytes)
			if row.Cache.MaxBytes > 0 {
				cache += " / " + formatByteSize(row.Cache.MaxBytes)
			}
		}
		named := "-"
		if row.NamedCaches != nil {
			named = formatByteSize(row.NamedCaches.Bytes)
			if row.NamedCaches.Protected > 0 {
				named += fmt.Sprintf(" (%d protected)", row.NamedCaches.Protected)
			}
		}
		jobs := "-"
		if row.hasRunner {
			jobs = formatByteSize(row.Jobs.Bytes)
		}
		changes := "-"
		if row.Changes != nil {
			changes = formatByteSize(row.Changes.Bytes)
		}
		workspaces := "-"
		if row.Workspaces != nil {
			workspaces = formatByteSize(row.Workspaces.Bytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			terminalSafeField(row.Location), cache, named, workspaces, jobs, changes, formatByteSize(row.TotalBytes))
	}
	_ = tw.Flush()
}

func formatByteSize(bytes int64) string {
	if bytes < 0 {
		return "-"
	}
	const unit = int64(1024)
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for _, label := range units {
		value /= 1024
		if value < 1024 || label == units[len(units)-1] {
			if value < 10 {
				return fmt.Sprintf("%.1f %s", value, label)
			}
			return fmt.Sprintf("%.0f %s", value, label)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}
