package main

import (
	"fmt"
	"os"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/proto"
)

func cmdAutomaticApply(args []string) int {
	if len(args) != 2 || args[0] == "" || !proto.ValidULID(args[1]) {
		fmt.Fprintln(os.Stderr, "errand: invalid automatic apply worker invocation")
		return client.ExitTransaction
	}
	if err := client.RunAutomaticApplyWorker(args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "errand: automatic workspace change application failed: %v\n", err)
		return client.ExitTransaction
	}
	return 0
}
