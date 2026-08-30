package client

import (
	"context"
	"os"
	"time"
)

// interruptTarget contains the immutable facts needed to control one admitted
// job. The signal owner is the only goroutine that reads signals for the job.
type interruptTarget struct {
	peerURL       string
	jobID         string
	handle        string
	report        func(string, ...any)
	notifications interruptNotifications
}

func newInterruptTarget(
	peerURL, jobID, handle string,
	report func(string, ...any),
	notifications interruptNotifications,
) interruptTarget {
	if report == nil {
		panic("nil interrupt reporter")
	}
	return interruptTarget{
		peerURL: peerURL, jobID: jobID, handle: handle,
		report: report, notifications: notifications,
	}
}

// interruptNotifications owns the process-wide os/signal registration. A
// successful detach first stops delivery, then drains the signal channel. If a
// signal was already in flight, delivery is resumed so the usual second Ctrl-C
// force-kill behavior remains available. Stop must be idempotent because it can
// be called again after a resume.
type interruptNotifications struct {
	stop   func()
	resume func()
}

func newInterruptNotifications(stop, resume func()) interruptNotifications {
	if stop == nil || resume == nil {
		panic("interrupt notifications require stop and resume")
	}
	return interruptNotifications{stop: stop, resume: resume}
}

// admitJobController is the admission linearization point. The caller is the
// sole sigCh reader before this function. A queued signal remains local; once
// the final drain is clear, the admitted controller becomes the sole reader
// before submit begins so it can control a job whose response is delayed.
func admitJobController(
	ctx context.Context,
	sigCh <-chan os.Signal,
	target interruptTarget,
) *admittedJobController {
	select {
	case <-ctx.Done():
		return nil
	case <-sigCh:
		return nil
	default:
	}
	return startAdmittedJobController(ctx, sigCh, target)
}

// admittedJobController represents the phase in which Ctrl-C controls the
// remote job. Every field is valid for every admitted controller.
type admittedJobController struct {
	target        interruptTarget
	detachRequest chan chan bool
	remote        chan struct{}
	forwarded     chan error
	done          chan struct{}
}

func newAdmittedJobController(target interruptTarget) *admittedJobController {
	return &admittedJobController{
		target:        target,
		detachRequest: make(chan chan bool),
		remote:        make(chan struct{}),
		forwarded:     make(chan error, 1),
		done:          make(chan struct{}),
	}
}

func startAdmittedJobController(
	ctx context.Context,
	sigCh <-chan os.Signal,
	target interruptTarget,
) *admittedJobController {
	controller := newAdmittedJobController(target)
	go controller.run(ctx, sigCh)
	return controller
}

// detach is the successful-detachment linearization point. The controller
// either confirms no signal was pending and restores normal local SIGINT
// handling, or reports that remote control has begun.
func (c *admittedJobController) detach(ctx context.Context) bool {
	reply := make(chan bool, 1)
	select {
	case c.detachRequest <- reply:
	case <-c.remote:
		return false
	case <-ctx.Done():
		return false
	}
	select {
	case detached := <-reply:
		return detached
	case <-c.remote:
		return false
	case <-ctx.Done():
		return false
	}
}

// run owns sigCh from admission until the caller's context ends. A 404/409
// from the first control request can be an admission race, so SIGINT delivery
// is retried until accepted or the job transaction ends.
func (c *admittedJobController) run(
	ctx context.Context,
	sigCh <-chan os.Signal,
) {
	defer close(c.done)

firstSignal:
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigCh:
			break firstSignal
		case reply := <-c.detachRequest:
			// signal.Stop guarantees no send remains in flight when it returns,
			// closing the check-then-stop race around successful detachment.
			c.target.notifications.stop()
			select {
			case <-sigCh:
				c.target.notifications.resume()
				reply <- false
				break firstSignal
			default:
				reply <- true
				return
			}
		}
	}

	close(c.remote)
	c.target.report("forwarding SIGINT to %s (Ctrl-C again to force-kill)", c.target.handle)
	forwardCtx, cancelForward := context.WithCancel(ctx)
	defer cancelForward()
	go func() {
		err := retryJobControl(
			forwardCtx,
			c.target.peerURL+"/v0/jobs/"+c.target.jobID+"/signal",
			map[string]string{"signal": "SIGINT"},
			true,
		)
		c.forwarded <- err
		if err != nil && ctx.Err() == nil {
			c.target.report("forwarding SIGINT failed: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
		return
	case <-sigCh:
	}
	cancelForward()
	c.target.report("force-killing %s", c.target.handle)
	// Further Ctrl-Cs must recover their normal local behavior even if the
	// force-kill control request loses contact with the peer.
	c.target.notifications.stop()
	killCtx, cancelKill := context.WithTimeout(ctx, controlRequestTimeout)
	defer cancelKill()
	if err := retryJobControl(
		killCtx,
		c.target.peerURL+"/v0/jobs/"+c.target.jobID+"/kill?force=1",
		nil,
		false,
	); err != nil && ctx.Err() == nil {
		c.target.report("force-kill failed: %v; process may still be running; handle %s", err, c.target.handle)
	}
}

func (c *admittedJobController) completeDetach(ctx context.Context) int {
	if c.detach(ctx) {
		c.target.report("detached; reattach with: errand attach %s", c.target.handle)
		return 0
	}
	timer := time.NewTimer(controlRequestTimeout)
	defer timer.Stop()
	select {
	case forwardErr := <-c.forwarded:
		if forwardErr != nil {
			c.target.report("SIGINT delivery is uncertain: %v; inspect or control job %s", forwardErr, c.target.handle)
			return ExitTransaction
		}
		c.target.report("SIGINT forwarded to %s", c.target.handle)
		return signalExit("interrupt", 2)
	case <-timer.C:
		c.target.report("SIGINT delivery is uncertain; inspect or control job %s", c.target.handle)
		return ExitTransaction
	}
}
