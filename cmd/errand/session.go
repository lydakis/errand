package main

import (
	"flag"
	"fmt"

	"github.com/lydakis/errand/internal/workspace"
)

type sessionFlags struct {
	forwards  stringList
	noForward bool
}

func (f *sessionFlags) bind(fs *flag.FlagSet) {
	fs.Var(&f.forwards, "forward", "forward local loopback [LOCAL:]REMOTE while attached (repeatable; replaces configured list)")
	fs.Var(&f.forwards, "L", "forward local loopback [LOCAL:]REMOTE while attached (repeatable)")
	fs.BoolVar(&f.noForward, "no-forward", false, "disable configured session forwards")
}

func (f sessionFlags) overrides(fs *flag.FlagSet) ([]string, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["no-forward"] && (set["forward"] || set["L"]) {
		return nil, fmt.Errorf("--forward and --no-forward are mutually exclusive")
	}
	if f.noForward {
		return []string{}, nil
	}
	if err := workspace.ValidatePortForwards(f.forwards); err != nil {
		return nil, err
	}
	return f.forwards, nil
}

func sessionForwards(values []string) (portForwardList, error) {
	var result portForwardList
	for _, value := range values {
		if err := result.Set(value); err != nil {
			return nil, err
		}
	}
	return result, nil
}
