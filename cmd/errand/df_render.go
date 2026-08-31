package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

type dfRow struct {
	Location   string                 `json:"location"`
	Cache      *proto.CacheStats      `json:"cache,omitempty"`
	Jobs       proto.StorageCategory  `json:"jobs"`
	Outputs    *proto.StorageCategory `json:"outputs,omitempty"`
	TotalBytes int64                  `json:"total_bytes"`
}

func cmdDf(args []string) int {
	return cmdDfTo(args, os.Stdout, os.Stderr)
}

func cmdDfTo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("df", flag.ExitOnError)
	on := fs.String("on", "", "restrict to one peer name")
	rawURL := fs.String("url", "", "restrict to one peer base URL")
	jsonOutput := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand: unexpected df arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	read, err := readFleet(*rawURL, *on, stderr, client.StorageStats)
	if err != nil {
		fmt.Fprintf(stderr, "errand: %v\n", err)
		return 1
	}
	rows := make([]dfRow, 0, len(read.results)+1)
	for _, result := range read.results {
		row := dfRow{
			Location: result.target.name,
			Cache:    result.value.Cache,
			Jobs:     result.value.Jobs,
		}
		if row.Cache != nil {
			row.TotalBytes += row.Cache.Bytes
		}
		row.TotalBytes += row.Jobs.Bytes
		rows = append(rows, row)
	}
	outputs, err := client.OutputStats()
	if err != nil {
		fmt.Fprintf(stderr, "errand: local outputs: %v\n", err)
		read.failed = true
	} else {
		rows = append(rows, dfRow{
			Location: "local", Outputs: &outputs, TotalBytes: outputs.Bytes,
		})
	}

	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "errand: encoding storage usage: %v\n", err)
			return 1
		}
	} else {
		writeDf(stdout, rows)
	}
	return read.exitCode()
}

func writeDf(w io.Writer, rows []dfRow) {
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "LOCATION\tCACHE\tJOBS\tOUTPUTS\tTOTAL")
	for _, row := range rows {
		cache := "-"
		if row.Cache != nil {
			cache = formatByteSize(row.Cache.Bytes)
			if row.Cache.MaxBytes > 0 {
				cache += " / " + formatByteSize(row.Cache.MaxBytes)
			}
		}
		jobs := "-"
		if row.Location != "local" {
			jobs = formatByteSize(row.Jobs.Bytes)
		}
		outputs := "-"
		if row.Outputs != nil {
			outputs = formatByteSize(row.Outputs.Bytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			terminalSafeField(row.Location), cache, jobs, outputs, formatByteSize(row.TotalBytes))
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
