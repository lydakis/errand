package main

import (
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

// handleScope is the error scope for a HANDLE argument.
func handleScope(handle, label, on string) errorScope {
	peer := cmpOr(label, on)
	if peer == "" {
		if i := strings.LastIndexByte(handle, '/'); i > 0 {
			peer = handle[:i]
		}
	}
	id := handle
	if i := strings.LastIndexByte(handle, '/'); i >= 0 {
		id = handle[i+1:]
	}
	return errorScope{peer: peer, job: strings.ToUpper(id)}
}

// handleErrorCode is 2 when the handle itself is malformed, 1 when it names
// nothing on the runner.
func handleErrorCode(err error) int {
	var bad *badHandleError
	var unknown *config.UnknownPeerError
	if errors.As(err, &bad) || errors.As(err, &unknown) {
		return 2
	}
	return 1
}

// needHandle reports a missing HANDLE argument.
func needHandle(e *termui.Stream, command string) int {
	e.Errorf("errand %s needs a job handle", command)
	e.Hintf("for example errand %s mini/01M3BFTQ6QD4; errand ps lists them", command)
	return 2
}

func cmdAttach(args []string) int { return cmdAttachTo(args, os.Stdout, os.Stderr) }

func cmdAttachTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand attach", flag.ContinueOnError)
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	profile := fs.String("profile", "", "session preferences from workspace or personal configuration")
	var session sessionFlags
	session.bind(fs)
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "attach", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return needHandle(e, "attach")
	}
	cli, err := session.overrides(fs)
	if err != nil {
		return usageError(e, "%v", err)
	}
	emptyProfile := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "profile" && *profile == "" {
			emptyProfile = true
		}
	})
	if emptyProfile {
		return usageError(e, "--profile needs a name")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failWith(e, client.ExitTransaction, err, errorScope{})
	}
	effective, err := config.ResolveSession(cwd, *profile, cli)
	if err != nil {
		return failWith(e, client.ExitTransaction, err, errorScope{})
	}
	forwards, err := sessionForwards(effective.Forwards)
	if err != nil {
		return usageError(e, "%v", err)
	}
	peerURL, label, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		return failWith(e, handleErrorCode(err), err, handleScope(fs.Arg(0), label, *on))
	}
	return client.Attach(client.AttachOptions{
		BeforeContact: func() {
			if !output.quiet {
				warnRunnerVersion(e, peerURL, label)
			}
		},
		PeerURL: peerURL, PeerName: label, JobID: jobID, Forwards: forwards,
		Stdout: con.Out, Stderr: con.Err,
		Display: client.RunDisplay{UI: con, Quiet: output.quiet, Verbose: output.verbose},
	})
}

// killWait is how long kill waits for a job to end before suggesting -f.
var killWait = 10 * time.Second

func cmdKill(args []string) int { return cmdKillTo(args, os.Stdout, os.Stderr) }

func cmdKillTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	fs := flag.NewFlagSet("errand kill", flag.ContinueOnError)
	force := fs.Bool("force", false, "SIGKILL instead of SIGTERM")
	fs.BoolVar(force, "f", false, "SIGKILL instead of SIGTERM")
	noWait := fs.Bool("no-wait", false, "return once the signal is sent")
	on := fs.String("on", "", "peer name")
	rawURL := fs.String("url", "", "peer base URL")
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args, "kill", stdout, e); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return needHandle(e, "kill")
	}
	peerURL, label, jobID, err := resolveHandle(fs.Arg(0), *rawURL, *on)
	if err != nil {
		return failWith(e, handleErrorCode(err), err, handleScope(fs.Arg(0), label, *on))
	}
	label = cmpOr(label, peerURL)
	shown := displayHandle(e, label, jobID)
	signal := "SIGTERM"
	if *force {
		signal = "SIGKILL"
	}
	scope := errorScope{peer: label, job: jobID}
	alreadyDone := func(err error) bool {
		status, _, ok := client.RemoteMessage(err)
		if ok && status == http.StatusConflict {
			if !output.quiet {
				e.Say(termui.OK, e.ID(shown)+" had already finished")
			}
			return true
		}
		return false
	}
	if *noWait {
		if err := client.Kill(peerURL, jobID, *force); err != nil {
			if alreadyDone(err) {
				return 0
			}
			return failWith(e, 1, err, scope)
		}
		if !output.quiet {
			e.Say(termui.OK, "Sent "+signal+" to "+e.ID(shown))
		}
		return 0
	}
	var spin *termui.Spinner
	if !output.quiet {
		spin = e.Spin("Stopping " + e.ID(shown) + " " + e.D("("+signal+")") + "…")
	}
	started := time.Now()
	if err := client.Kill(peerURL, jobID, *force); err != nil {
		if spin != nil {
			spin.Stop()
		}
		if alreadyDone(err) {
			return 0
		}
		return failWith(e, 1, err, scope)
	}
	_, ended, pollErr := waitForEnd(peerURL, jobID, killWait)
	if spin != nil {
		spin.Stop()
	}
	if !ended && pollErr != nil {
		msg, _ := describeError(pollErr, errorScope{peer: label, job: jobID})
		e.Errorf("couldn't confirm %s stopped: %s", shown, msg)
		return 1
	}
	if !ended {
		e.Say(termui.Warn, e.ID(shown)+" is still running after "+termui.Duration(killWait))
		if !*force {
			e.Next("errand kill -f "+shown, "sends SIGKILL")
		}
		return 1
	}
	if output.quiet {
		return 0
	}
	e.Say(termui.OK, "Stopped "+e.ID(shown)+" "+e.D("after "+termui.Duration(time.Since(started))))
	return 0
}

// waitForEnd polls a job until it has a result or the wait runs out. When it
// runs out, the error is the last poll's, so an unreachable runner isn't
// mistaken for a job that's still running.
func waitForEnd(peerURL, jobID string, wait time.Duration) (proto.JobStatus, bool, error) {
	deadline := time.Now().Add(wait)
	delay := 100 * time.Millisecond
	for {
		details, err := client.GetJobDetails(peerURL, jobID)
		if err == nil && details.Result != nil {
			return details.JobStatus, true, nil
		}
		if time.Now().After(deadline) {
			return details.JobStatus, false, err
		}
		time.Sleep(delay)
		delay = min(delay*2, time.Second)
	}
}
