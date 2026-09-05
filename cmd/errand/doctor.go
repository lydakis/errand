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
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
)

type doctorCheck = setup.DiagnosticCheck

type doctorReport struct {
	OK         bool                 `json:"ok"`
	Effective  *config.EffectiveRun `json:"effective,omitempty"`
	Checks     []doctorCheck        `json:"checks"`
	Info       *proto.Info          `json:"info,omitempty"`
	LocalInfo  *proto.Info          `json:"local_info,omitempty"`
	Scope      string               `json:"scope"`
	SocketPath string               `json:"socket_path,omitempty"`
}

const doctorScope = "Checks this installation, any configured local runner, and access to the selected peer's info. Custom service definitions and serve CLI overrides require separate inspection. No job is submitted or configuration changed. Success does not guarantee snapshot validity, command availability, submission permission, or capacity."

type doctorProbe func(context.Context, string) (proto.Info, error)

type doctorServices struct {
	probe doctorProbe
	local func(context.Context, string) setup.Diagnosis
	ssh   func(context.Context, string) error
}

func cmdDoctor(args []string) int {
	return cmdDoctorWith(args, os.Stdout, os.Stderr, doctorServices{probe: func(ctx context.Context, target string) (proto.Info, error) {
		return client.ProbeInfo(ctx, target, probeTimeout)
	}, local: localDoctor, ssh: client.InspectSSH})
}

func cmdDoctorTo(args []string, stdout, stderr io.Writer, probe doctorProbe) int {
	return cmdDoctorWith(args, stdout, stderr, doctorServices{probe: probe})
}

func localDoctor(ctx context.Context, path string) setup.Diagnosis {
	return setup.Diagnose(ctx, path, setup.RealSystem{})
}

func cmdDoctorWith(args []string, stdout, stderr io.Writer, services doctorServices) int {
	fs := flag.NewFlagSet("errand doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var flags runConfigFlags
	flags.bind(fs)
	asJSON := fs.Bool("json", false, "emit diagnostic checks, next steps, and effective configuration as JSON")
	configPath := fs.String("config", "", "explicitly check this local runner configuration")
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
	var invalid string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" && *configPath == "" {
			invalid = "--config requires a non-empty path"
		}
	})
	if invalid != "" {
		fmt.Fprintln(stderr, "errand doctor: "+invalid)
		return 2
	}
	overrides, err := flags.overrides(fs)
	if err != nil {
		fmt.Fprintf(stderr, "errand doctor: %v\n", err)
		return 2
	}
	report := doctorReport{Scope: doctorScope}
	if services.local != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		localReport := services.local(ctx, *configPath)
		cancel()
		report.LocalInfo, report.SocketPath = localReport.Info, localReport.SocketPath
		for _, check := range localReport.Checks {
			check.Name = "local." + check.Name
			report.Checks = append(report.Checks, check)
		}
	}
	cwd, err := os.Getwd()
	var effective config.EffectiveRun
	if err == nil {
		effective, err = config.ResolveRun(cwd, overrides)
	}
	noPeer := errors.Is(err, config.ErrNoPeerSelected)
	if err != nil && !noPeer {
		report.Checks = append(report.Checks,
			doctorCheck{Name: "configuration", Status: "error", Detail: err.Error(), Hint: "Correct the reported configuration or select a configured peer with --on NAME."},
			doctorCheck{Name: "runner", Status: "skipped", Detail: "No probe was made because configuration did not resolve."},
		)
	} else {
		report.Effective = &effective
		detail := fmt.Sprintf("Selected %s at %s (from %s)", effective.Peer, effective.URL, effective.Sources["peer"])
		if noPeer {
			detail = "Run settings resolved; no outbound peer is selected."
		}
		report.Checks = append(report.Checks, doctorCheck{Name: "configuration", Status: "ok", Detail: detail})
		if missing := effective.MissingEnvironment(); len(missing) != 0 {
			report.Checks = append(report.Checks,
				doctorCheck{Name: "environment", Status: "error", Detail: fmt.Sprintf("Required local variables are unset: %q", missing), Hint: "Set the required variables in the initiating shell, or change the selected environment settings."},
				doctorCheck{Name: "runner", Status: "skipped", Detail: "No probe was made because required environment variables are missing."},
			)
		} else if noPeer {
			report.Checks = append(report.Checks, doctorCheck{Name: "runner", Status: "skipped", Detail: "No outbound peer is selected; use --on NAME to check a configured peer."})
		} else {
			if len(effective.Environment) != 0 {
				report.Checks = append(report.Checks, doctorCheck{Name: "environment", Status: "ok", Detail: fmt.Sprintf("%d environment variables resolved; values hidden.", len(effective.Environment))})
			}
			target := effective.URL
			if overrides.URL == "" {
				target = client.ConfigureSSHPeer(target, effective.Peer, effective.RemoteCommand, effective.RemoteSocket)
			}
			if client.IsSSHPeer(target) && services.ssh != nil {
				ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
				sshErr := services.ssh(ctx, target)
				cancel()
				if sshErr != nil {
					hint := "Check SSH connectivity, known host keys and non-interactive authentication for this peer."
					var diagnostic *client.SSHDiagnosticError
					if errors.As(sshErr, &diagnostic) && diagnostic.CommandUnavailable {
						hint = "Check the remote shell's PATH or set this peer's remote_command to the absolute Errand executable path."
					}
					report.Checks = append(report.Checks, doctorCheck{Name: "ssh", Status: "error", Detail: sshErr.Error(), Hint: hint}, doctorCheck{Name: "runner", Status: "skipped", Detail: "No info probe was made because SSH readiness failed."})
					return finishDoctorReport(stdout, stderr, report, *asJSON)
				}
				report.Checks = append(report.Checks, doctorCheck{Name: "ssh", Status: "ok", Detail: "Non-interactive SSH connected and resolved the configured bridge executable."})
			}
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			info, probeErr := services.probe(ctx, target)
			cancel()
			if probeErr != nil {
				report.Checks = append(report.Checks, doctorProbeFailure(effective.URL, probeErr))
			} else {
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
	return finishDoctorReport(stdout, stderr, report, *asJSON)
}

func finishDoctorReport(stdout, stderr io.Writer, report doctorReport, asJSON bool) int {
	report.OK = true
	for _, check := range report.Checks {
		if check.Status == "error" {
			report.OK = false
		}
	}
	if err := writeDoctorReport(stdout, report, asJSON); err != nil {
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
		check.Hint = "On the runner, inspect errand access list using its service's --config path. Check deny_users first, then the intended allowlist or capability grant, and restart after saved policy edits. SSH access is managed separately."
	case client.ProbeNotErrand:
		check.Hint = "Verify the selected endpoint serves Errand and that the client and runner use a compatible protocol version."
	default:
		if client.IsSSHPeer(target) {
			check.Hint = "Check the peer's remote_command and remote_socket settings, then run errand doctor on the runner as its service user (with the service's --config path if customized)."
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
