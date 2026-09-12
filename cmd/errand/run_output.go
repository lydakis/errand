package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/lydakis/errand/internal/config"
)

func printRunBindings(w io.Writer, effective config.EffectiveRun, verbose bool) {
	if verbose {
		for _, cache := range effective.Caches {
			fmt.Fprintf(w, "errand: using cache %q at %q\n", cache.Name, cache.Path)
		}
		for _, artifact := range effective.Artifacts {
			fmt.Fprintf(w, "errand: retaining artifact %q\n", artifact)
		}
		return
	}
	var parts []string
	if n := len(effective.Caches); n != 0 {
		parts = append(parts, fmt.Sprintf("using %d %s", n, plural(n, "cache")))
	}
	if n := len(effective.Artifacts); n != 0 {
		parts = append(parts, fmt.Sprintf("retaining %d %s", n, plural(n, "artifact")))
	}
	if len(parts) != 0 {
		fmt.Fprintf(w, "errand: %s (use --verbose for bindings)\n", strings.Join(parts, "; "))
	}
}
