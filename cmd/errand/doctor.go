package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warning, error, or skipped
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type doctorReport struct {
	OK        bool                 `json:"ok"`
	Effective *config.EffectiveRun `json:"effective,omitempty"`
	Checks    []doctorCheck        `json:"checks"`
	Info      *proto.Info          `json:"info,omitempty"`
	Scope     string               `json:"scope"`
}

const doctorScope = "Checks configuration and access to runner info. A successful check does not guarantee snapshot validity, command availability, permission to submit, or available capacity."

type doctorProbe func(context.Context, string) (proto.Info, error)

func cmdDoctor(args []string) int {
	return cmdDoctorTo(args, os.Stdout, os.Stderr, func(ctx context.Context, target string) (proto.Info, error) {
		return client.ProbeInfo(ctx, target, probeTimeout)
	})
}

func cmdDoctorTo(args []string, stdout, stderr io.Writer, probe doctorProbe) int {
	fs := flag.NewFlagSet("errand doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var flags runConfigFlags
	flags.bind(fs)
	asJSON := fs.Bool("json", false, "emit diagnostic checks, next steps, and effective configuration as JSON")
	setFlagUsage(fs, "errand doctor [options]")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "errand doctor: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	overrides, err := flags.overrides(fs)
	if err != nil {
		fmt.Fprintf(stderr, "errand doctor: %v\n", err)
		return 2
	}
	report := doctorReport{Scope: doctorScope}
	cwd, err := os.Getwd()
	var effective config.EffectiveRun
	if err == nil {
		effective, err = config.ResolveRun(cwd, overrides)
	}
	if err != nil {
		report.Checks = []doctorCheck{
			{Name: "configuration", Status: "error", Detail: err.Error(), Hint: "Correct the reported configuration or select a configured peer with --on NAME."},
			{Name: "runner", Status: "skipped", Detail: "No probe was made because configuration did not resolve."},
		}
	} else {
		report.Effective = &effective
		report.Checks = []doctorCheck{{Name: "configuration", Status: "ok", Detail: fmt.Sprintf("Selected %s at %s (from %s)", effective.Peer, effective.URL, effective.Sources["peer"])}}
		if missing := effective.MissingEnvironment(); len(missing) != 0 {
			report.Checks = append(report.Checks,
				doctorCheck{Name: "environment", Status: "error", Detail: fmt.Sprintf("Required local variables are unset: %q", missing), Hint: "Set the required variables in the initiating shell, or change the selected environment settings."},
				doctorCheck{Name: "runner", Status: "skipped", Detail: "No probe was made because required environment variables are missing."},
			)
		} else {
			if len(effective.Environment) != 0 {
				report.Checks = append(report.Checks, doctorCheck{Name: "environment", Status: "ok", Detail: fmt.Sprintf("%d environment variables resolved; values hidden.", len(effective.Environment))})
			}
			target := effective.URL
			if overrides.URL == "" {
				target = client.ConfigureSSHPeer(target, effective.Peer, effective.RemoteCommand, effective.RemoteSocket)
			}
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			info, probeErr := probe(ctx, target)
			cancel()
			if probeErr != nil {
				report.Checks = append(report.Checks, doctorProbeFailure(effective.URL, probeErr))
			} else {
				report.OK = true
				report.Info = &info
				check := doctorCheck{Name: "runner", Status: "ok", Detail: fmt.Sprintf("Runner %s answered with compatible protocol %d; this caller can read runner info.", info.Version, info.Proto)}
				if info.Busy {
					check.Status = "warning"
					check.Detail += " Runner is currently busy."
					check.Hint = "A later submission may queue or be refused; check capacity with errand peers."
				}
				report.Checks = append(report.Checks, check)
			}
		}
	}
	if err := writeDoctorReport(stdout, report, *asJSON); err != nil {
		fmt.Fprintf(stderr, "errand doctor: writing report: %v\n", err)
		return 1
	}
	if !report.OK {
		return 1
	}
	return 0
}

func doctorProbeFailure(target string, err error) doctorCheck {
	check := doctorCheck{Name: "runner", Status: "error", Detail: err.Error()}
	kind, _ := client.ProbeKindOf(err)
	switch kind {
	case client.ProbeForbidden:
		check.Hint = "On the runner, inspect errand access list using its service's --config path. Verify the intended allowlist or capability grant, and restart after saved allowlist edits. SSH access is managed separately."
	case client.ProbeNotErrand:
		check.Hint = "Verify the selected endpoint serves Errand and that the client and runner use a compatible protocol version."
	default:
		if client.IsSSHPeer(target) {
			check.Hint = "Check SSH connectivity, the runner service, and the peer's remote_command and remote_socket settings."
		} else {
			check.Hint = "Check tailnet connectivity, network policy, the configured address and port, and the runner service."
		}
	}
	return check
}

func writeDoctorReport(w io.Writer, report doctorReport, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(w, "%s %s: %s\n", strings.ToUpper(check.Status), check.Name, terminalSafeField(check.Detail)); err != nil {
			return err
		}
		if check.Hint != "" {
			if _, err := fmt.Fprintf(w, "  Next: %s\n", check.Hint); err != nil {
				return err
			}
		}
	}
	if report.Effective != nil {
		workdir := report.Effective.Workdir
		if workdir == "" {
			workdir = "."
		}
		if _, err := fmt.Fprintf(w, "Workspace: %s\nWorkdir: %s\n", terminalSafeField(report.Effective.Root), terminalSafeField(workdir)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, report.Scope)
	return err
}
