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
	"sync"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/setup"
	"github.com/lydakis/errand/internal/termui"
)

type doctorCheck = setup.DiagnosticCheck

type doctorReport struct {
	PlacementSkipped []placementExclusion `json:"placement_skipped,omitempty"`
	OK               bool                 `json:"ok"`
	Effective        *config.EffectiveRun `json:"effective,omitempty"`
	Checks           []doctorCheck        `json:"checks"`
	Info             *proto.Info          `json:"info,omitempty"`
	LocalInfo        *proto.Info          `json:"local_info,omitempty"`
	Scope            string               `json:"scope"`
	SocketPath       string               `json:"socket_path,omitempty"`
}

const doctorScope = "Checks this installation, local automatic-apply state, any configured local runner, and access to the selected peer's info. Custom service definitions and serve CLI overrides require separate inspection. No job is submitted or configuration changed. Success does not guarantee snapshot validity, command availability, submission permission, or capacity."

type doctorProbe func(context.Context, string) (proto.Info, error)

type doctorServices struct {
	where placementProbe
	probe doctorProbe
	local func(context.Context, string) setup.Diagnosis
	ssh   func(context.Context, string) error
}

func cmdDoctor(args []string) int {
	return cmdDoctorWith(args, os.Stdout, os.Stderr, doctorServices{probe: func(ctx context.Context, target string) (proto.Info, error) {
		return client.ProbeInfo(ctx, target, probeTimeout)
	}, where: client.ProbeWhereInfo, local: localDoctor, ssh: client.InspectSSH})
}

func cmdDoctorTo(args []string, stdout, stderr io.Writer, probe doctorProbe) int {
	return cmdDoctorWith(args, stdout, stderr, doctorServices{probe: probe, where: func(ctx context.Context, target, _ string, _ time.Duration) (proto.Info, error) {
		return probe(ctx, target)
	}})
}

func localDoctor(ctx context.Context, path string) setup.Diagnosis {
	report := setup.Diagnose(ctx, path, setup.RealSystem{})
	if report.Info != nil && report.Info.Version != version {
		report.Checks = append(report.Checks, setup.DiagnosticCheck{Name: "version", Status: "warning", Detail: fmt.Sprintf("Local daemon %s; CLI %s.", report.Info.Version, version), Hint: "If behavior differs, update the installation and run errand setup to restart its daemon."})
	}
	return report
}

func cmdDoctorWith(args []string, stdout, stderr io.Writer, services doctorServices) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand doctor", flag.ContinueOnError)
	var flags runConfigFlags
	flags.bind(fs)
	asJSON := fs.Bool("json", false, "emit diagnostic checks, next steps, and effective configuration as JSON")
	configPath := fs.String("config", "", "explicitly check this local runner configuration")
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "doctor", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	var spin *termui.Spinner
	finish := func(report doctorReport) int {
		if spin != nil {
			spin.Stop()
		}
		return finishDoctorReport(con, report, *asJSON, output)
	}
	var invalid string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" && *configPath == "" {
			invalid = "--config requires a non-empty path"
		}
	})
	if invalid != "" {
		return usageError(e, "%s", invalid)
	}
	overrides, err := flags.overrides(fs)
	if err != nil {
		return usageError(e, "%v", err)
	}
	// Started only after the arguments check out, so a usage error never
	// leaves a spinner behind.
	if !*asJSON && !output.quiet {
		spin = e.Spin("Checking this machine and your runners…")
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
	if err == nil || noPeer {
		if executionErr := effective.PrepareExecution(false); executionErr != nil {
			err = executionErr
			noPeer = false
		}
	}
	if err != nil && !noPeer {
		report.Checks = append(report.Checks,
			doctorCheck{Name: "configuration", Status: "error", Detail: err.Error(), Hint: "Correct the reported configuration or select a configured peer with --on NAME."},
			doctorCheck{Name: "runner", Status: "skipped", Detail: "No probe was made because configuration did not resolve."},
		)
	} else {
		report.Effective = &effective
		detail := fmt.Sprintf("Selected %s at %s (from %s)", effective.Peer, effective.URL, effective.Sources["peer"])
		if effective.Where != "" {
			detail = fmt.Sprintf("Select a configured runner matching %s (from %s)", effective.Where, effective.Sources["where"])
		}
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
			var selectedInfo *proto.Info
			var selectionTarget string
			if effective.Where != "" {
				probe := services.where
				selection, selectionErr := chooseRunners(context.Background(), effective, probe)
				report.PlacementSkipped = selection.Excluded
				if !*asJSON && selectionErr == nil && output.verbose {
					selection.printExcluded(e)
				}
				if selectionErr != nil {
					report.Checks = append(report.Checks, doctorCheck{Name: "runner", Status: "error", Detail: selectionErr.Error(), Hint: "Check peer connectivity and requirements with errand peers."})
					return finish(report)
				}
				chosen := selection.Choices[0]
				effective.Peer, effective.URL = chosen.Name, chosen.URL
				selectionTarget = chosen.Target
				effective.RemoteCommand, effective.RemoteSocket = chosen.RemoteCommand, chosen.RemoteSocket
				selectedInfo = &chosen.Info
				report.Checks = append(report.Checks, doctorCheck{Name: "placement", Status: "ok", Detail: fmt.Sprintf("Selected %s for %s; capacity is a point-in-time observation.", chosen.Name, effective.Where)})
			}
			target := effective.URL
			if selectionTarget != "" {
				target = selectionTarget
			} else if overrides.URL == "" {
				target = client.ConfigureSSHPeer(target, effective.Peer, effective.RemoteCommand, effective.RemoteSocket)
			}
			if client.IsSSHPeer(target) && services.ssh != nil && selectedInfo == nil {
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
					return finish(report)
				}
				report.Checks = append(report.Checks, doctorCheck{Name: "ssh", Status: "ok", Detail: "Non-interactive SSH connected and resolved the configured bridge executable."})
			}
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			var info proto.Info
			var probeErr error
			if selectedInfo != nil {
				info = *selectedInfo
			} else {
				info, probeErr = services.probe(ctx, target)
			}
			cancel()
			if probeErr != nil {
				report.Checks = append(report.Checks, doctorProbeFailure(effective.URL, probeErr))
			} else {
				report.Info = &info
				check := doctorCheck{Name: "runner", Status: "ok", Detail: fmt.Sprintf("Runner %s answered with protocol %d; this caller can read runner info.", info.Version, info.Proto)}
				if info.Busy {
					check.Status = "warning"
					check.Detail += " Runner is currently busy."
					check.Hint = "A later submission may queue or be refused; check capacity with errand peers."
				}
				if info.Version != version {
					check.Status = "warning"
					check.Detail = fmt.Sprintf("Runner %s; CLI %s.", info.Version, version)
					check.Hint = "If behavior differs, update the installations and run errand setup on the runner."
				}
				report.Checks = append(report.Checks, check)
			}
		}
	}
	if report.Effective != nil && report.Effective.Where == "" && overrides.URL == "" {
		report.Checks = append(report.Checks, otherRunnerChecks(report.Effective.Peer, services.probe)...)
	}
	report.Checks = append(report.Checks, doctorApplyChecks()...)
	return finish(report)
}

// otherRunnerChecks probes every configured runner besides the selected one,
// in parallel, so doctor answers "can I use my runners?" in one run.
func otherRunnerChecks(selected string, probe doctorProbe) []doctorCheck {
	if probe == nil {
		return nil
	}
	targets, warnings, err := peerTargets("", "")
	if err != nil {
		return nil
	}
	// A runner that can't even be resolved from the config is still a
	// runner doctor should mention.
	var configured []doctorCheck
	for _, warning := range warnings {
		configured = append(configured, doctorCheck{Name: "runners", Status: "warning", Detail: fmt.Sprint(warning), Hint: "Fix or remove that runner's entry in your errand config."})
	}
	var others []peerTarget
	for _, t := range targets {
		if t.name != selected && t.name != "local" {
			others = append(others, t)
		}
	}
	checks := make([]doctorCheck, len(others))
	var wg sync.WaitGroup
	for i, t := range others {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			defer cancel()
			info, err := probe(ctx, t.url)
			if err != nil {
				// Only the runner this setup selects decides doctor's verdict.
				check := doctorProbeFailure(t.url, err)
				check.Name = "runner." + t.name
				check.Status = "warning"
				checks[i] = check
				return
			}
			check := doctorCheck{Name: "runner." + t.name, Status: "ok", Detail: fmt.Sprintf("Runner %s answered with protocol %d; this caller can read runner info.", info.Version, info.Proto)}
			if info.Version != version {
				check.Status = "warning"
				check.Detail = fmt.Sprintf("Runner %s; CLI %s.", info.Version, version)
				check.Hint = "If behavior differs, update the installations and run errand setup on the runner."
			}
			checks[i] = check
		}()
	}
	wg.Wait()
	return append(configured, checks...)
}

func finishDoctorReport(con *termui.Console, report doctorReport, asJSON bool, output outputFlags) int {
	report.OK = true
	for _, check := range report.Checks {
		if check.Status == "error" {
			report.OK = false
		}
	}
	if asJSON {
		encoder := json.NewEncoder(con.Out.Writer())
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			con.Err.Errorf("writing report: %v", err)
			return 1
		}
	} else if !output.quiet {
		writeDoctorReport(con.Out, report, output.verbose)
	} else {
		// Quiet keeps the errors; a failing doctor never exits silently.
		for _, check := range safeDoctorChecks(report.Checks) {
			if check.Status == "error" {
				con.Err.Errorf("%s: %s", check.Name, check.Detail)
				if check.Hint != "" {
					con.Err.Hintf("%s", check.Hint)
				}
			}
		}
	}
	if !report.OK {
		return 1
	}
	return 0
}

// safeDoctorChecks quotes check text that came from runners or the system
// (probe errors, response bodies) before it reaches a terminal.
func safeDoctorChecks(checks []doctorCheck) []doctorCheck {
	safe := make([]doctorCheck, len(checks))
	for i, check := range checks {
		check.Detail, check.Hint = termui.SafeText(check.Detail), termui.SafeText(check.Hint)
		safe[i] = check
	}
	return safe
}

func doctorProbeFailure(target string, err error) doctorCheck {
	check := doctorCheck{Name: "runner", Status: "error", Detail: err.Error()}
	kind, _ := client.ProbeKindOf(err)
	switch kind {
	case client.ProbeForbidden:
		check.Hint = "On the runner, inspect errand access list using its service's --config path. Check deny_users first, then the intended allowlist or capability grant, and restart after saved policy edits. SSH access is managed separately."
	case client.ProbeNotErrand:
		check.Hint = "Verify the selected endpoint serves Errand and that the client and runner run the same Errand version."
	default:
		if client.IsSSHPeer(target) {
			check.Hint = "Check the peer's remote_command and remote_socket settings, then run errand doctor on the runner as its service user (with the service's --config path if customized)."
		} else {
			check.Hint = "Check tailnet connectivity, network policy, the configured address and port, and the runner service."
		}
	}
	return check
}

// writeDoctorReport prints one line per check in plain words, the same way
// whether things pass or fail, then a verdict.
func writeDoctorReport(s *termui.Stream, report doctorReport, verbose bool) {
	report.Checks = safeDoctorChecks(report.Checks)
	var binary, path *doctorCheck
	for i := range report.Checks {
		switch report.Checks[i].Name {
		case "local.binary":
			binary = &report.Checks[i]
		case "local.path":
			path = &report.Checks[i]
		}
	}
	problems, warnings := 0, 0
	line := func(check doctorCheck, text string) {
		glyph := termui.OK
		switch check.Status {
		case "error":
			glyph = termui.Fail
			problems++
		case "warning":
			glyph = termui.Warn
			warnings++
		case "skipped":
			glyph = termui.Skip
		}
		s.Print(s.G(glyph) + " " + text)
		if check.Hint != "" && (check.Status == "error" || check.Status == "warning" || verbose) {
			for _, l := range wrapPlain(check.Hint, 90) {
				s.Print("    " + s.D(l))
			}
		}
		if verbose && check.Detail != "" && check.Status == "ok" {
			s.Print("    " + s.D(check.Detail))
		}
	}
	if binary != nil {
		text := "errand " + version
		exe := strings.TrimPrefix(binary.Detail, "Running executable: ")
		if exe != "" && exe != binary.Detail {
			text += " " + s.D("· "+homeRelative(exe))
			if path != nil && path.Status == "ok" {
				text += s.D(", first on PATH")
			}
		} else if binary.Status != "ok" {
			text += ": " + binary.Detail
		}
		merged := *binary
		if path != nil && path.Status != "ok" {
			merged = *path
			text += " " + s.D("· "+path.Detail)
		}
		line(merged, text)
	}
	for _, check := range report.Checks {
		switch {
		case check.Name == "local.binary" || check.Name == "local.path":
			continue
		case check.Name == "local.runner" && check.Status == "skipped":
			line(check, "No local runner on this machine "+s.D("· optional; errand setup adds one"))
		case strings.HasPrefix(check.Name, "local."):
			line(check, "Local runner: "+check.Detail)
		case check.Name == "configuration":
			line(check, doctorConfigText(s, report, check))
		case check.Name == "environment":
			line(check, "Environment: "+check.Detail)
		case check.Name == "placement":
			line(check, check.Detail)
		case check.Name == "ssh":
			line(check, "SSH: "+check.Detail)
		case check.Name == "runner" || strings.HasPrefix(check.Name, "runner."):
			name := strings.TrimPrefix(check.Name, "runner.")
			if check.Name == "runner" && report.Effective != nil {
				name = cmpOr(report.Effective.Peer, report.Effective.URL)
			}
			line(check, doctorRunnerText(s, name, check))
		case check.Name == "automatic_apply":
			line(check, "Apply: "+check.Detail)
		default:
			line(check, check.Name+": "+check.Detail)
		}
	}
	if verbose && report.Effective != nil {
		s.Print("")
		s.Print(s.D("Workspace " + homeRelative(report.Effective.Root) + " · workdir " + workdirLabel(report.Effective.Workdir)))
		for _, l := range wrapPlain(report.Scope, 90) {
			s.Print(s.D(l))
		}
	}
	s.Print("")
	switch {
	case problems > 0:
		s.Print(s.Paint(termui.Things(problems, "problem", "problems")+".", termui.Red, termui.Bold) + warningSuffix(s, warnings))
	case warnings > 0:
		s.Print(s.Paint(termui.Things(warnings, "warning", "warnings")+".", termui.Yellow, termui.Bold))
	default:
		s.Print(s.Paint("All good.", termui.Green, termui.Bold) + " " + s.D("errand doctor -v for details, --json for scripts"))
	}
}

func warningSuffix(s *termui.Stream, warnings int) string {
	if warnings == 0 {
		return ""
	}
	return " " + s.D(termui.Things(warnings, "warning", "warnings")+" too.")
}

func doctorConfigText(s *termui.Stream, report doctorReport, check doctorCheck) string {
	if check.Status != "ok" || report.Effective == nil {
		return "Config: " + check.Detail
	}
	e := report.Effective
	source := e.Sources["peer"]
	if path, _, ok := strings.Cut(strings.TrimPrefix(source, "personal: "), " ("); ok && strings.HasPrefix(source, "personal: ") {
		source = homeRelative(path)
	}
	switch {
	case e.Where != "":
		return "Config " + s.D("· runs go to any runner matching "+e.Where)
	case e.Peer == "":
		return "Config " + s.D("· no default runner; use --on NAME")
	default:
		return "Config " + s.D("· "+source+" · default runner "+e.Peer)
	}
}

func doctorRunnerText(s *termui.Stream, name string, check doctorCheck) string {
	switch check.Status {
	case "ok":
		v := ""
		if _, rest, ok := strings.Cut(check.Detail, "Runner "); ok {
			v, _, _ = strings.Cut(rest, " ")
		}
		return name + " answered " + s.D("· errand "+v)
	case "warning":
		return name + " answered, but " + strings.TrimSuffix(check.Detail, ".")
	case "skipped":
		return name + " " + s.D("· "+check.Detail)
	default:
		cause := check.Detail
		if strings.Contains(cause, "no such host") {
			cause = "no such host"
		}
		return name + " didn't answer " + s.D("("+cause+")")
	}
}
