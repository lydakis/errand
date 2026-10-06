package cloud

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/limitbuf"
	"github.com/lydakis/errand/internal/nowindow"
	"github.com/lydakis/errand/internal/proto"
)

// Provider creates and destroys the machines behind an offer.
type Provider interface {
	// Acquire creates a machine for a lease and returns how to reach the
	// errand runner on it once the machine exists. The broker then waits for
	// that runner to answer.
	Acquire(ctx context.Context, req AcquireRequest) (Machine, error)
	// Release destroys the lease's machine. It must succeed when run twice,
	// and when State is empty because acquire stopped before saving any.
	Release(ctx context.Context, req ReleaseRequest) error
	// ReleaseSpec is everything Release depends on besides the request.
	// Each lease records it when it is made, so the lease can be released
	// after its offer changes or is removed.
	ReleaseSpec() ReleaseSpec
}

// ReleaseSpec names one way of releasing machines.
type ReleaseSpec struct {
	Command []string       `json:"command,omitempty"`
	Lambda  *LambdaRelease `json:"lambda,omitempty"`
}

type LambdaRelease struct {
	APIKeyFile string `json:"api_key_file"`
}

func (s ReleaseSpec) equal(o ReleaseSpec) bool {
	return slices.Equal(s.Command, o.Command) && (s.Lambda == nil) == (o.Lambda == nil) && (s.Lambda == nil || *s.Lambda == *o.Lambda)
}

// provider makes a provider that releases as s says.
func (s ReleaseSpec) provider() (Provider, error) {
	switch {
	case len(s.Command) > 0 && s.Lambda == nil:
		return CommandProvider{ReleaseCommand: s.Command}, nil
	case len(s.Command) == 0 && s.Lambda != nil && s.Lambda.APIKeyFile != "":
		return &LambdaProvider{APIKeyFile: s.Lambda.APIKeyFile}, nil
	}
	return nil, errors.New("no way to release its machines is set")
}

type AcquireRequest struct {
	LeaseID string
	Offer   string
	Where   string
	// Login is the tailnet login of the caller, when it has one, so the
	// machine can admit them without a capability grant.
	Login string
	// SSHKey is the caller's SSH public key, when it sent one, so a machine
	// reached over SSH can admit them.
	SSHKey string
	// Progress shows a line to the waiting client.
	Progress func(string)
	// Save persists provider state as soon as there is any, such as an
	// instance ID, so release can find a machine whose acquire was cut short.
	// A provider must not create anything whose state it failed to save.
	Save func(json.RawMessage) error
}

type Machine struct {
	Target proto.LeaseTarget
	State  json.RawMessage // passed to Release
	// Identity is a private key file the cloud peer reaches the machine with
	// over SSH, when the provider made one.
	Identity string
	// Drain is how the cloud peer reaches the runner's local socket to hold
	// it idle before an idle release, when that is not Target: over SSH, for
	// a machine clients reach on the tailnet. Nil means Target.
	Drain *proto.LeaseTarget
}

// ReleasePending is what Release returns when the provider has accepted the
// release but does not yet show the machine gone. The broker asks again
// shortly, and the lease counts as released only once Release returns nil.
type ReleasePending struct{ Msg string }

func (e *ReleasePending) Error() string { return e.Msg }

type ReleaseRequest struct {
	LeaseID string
	Offer   string
	State   json.RawMessage
}

// CommandProvider runs two configured commands; see docs/CLOUD.md.
type CommandProvider struct {
	AcquireCommand, ReleaseCommand []string
}

func (p CommandProvider) Acquire(ctx context.Context, req AcquireRequest) (Machine, error) {
	cmd := exec.CommandContext(ctx, p.AcquireCommand[0], p.AcquireCommand[1:]...)
	cmd.Env = append(os.Environ(), "ERRAND_LEASE_ID="+req.LeaseID, "ERRAND_OFFER="+req.Offer, "ERRAND_LEASE_WHERE="+req.Where, "ERRAND_LEASE_LOGIN="+req.Login, "ERRAND_LEASE_SSH_KEY="+req.SSHKey)
	cmd.WaitDelay = 5 * time.Second
	nowindow.Hide(cmd)
	stdout := &limitbuf.Buffer{Limit: maxAcquireOutput}
	cmd.Stdout = stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Machine{}, err
	}
	if err := cmd.Start(); err != nil {
		return Machine{}, fmt.Errorf("starting acquire command: %w", err)
	}
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			req.Progress(line)
		}
	}
	// A line too long to show stops the scanner; the rest is still read, so
	// the command never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, stderr)
	err = cmd.Wait()
	if ctx.Err() != nil {
		return Machine{}, ctx.Err()
	}
	if err != nil {
		return Machine{}, fmt.Errorf("acquire command failed: %v", err)
	}
	if stdout.Truncated() {
		return Machine{}, fmt.Errorf("acquire printed more than %d bytes", maxAcquireOutput)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := json.RawMessage(strings.TrimSpace(lines[len(lines)-1]))
	var object map[string]json.RawMessage
	if err := json.Unmarshal(last, &object); err != nil {
		return Machine{}, fmt.Errorf("acquire must print a JSON object as its last stdout line")
	}
	if err := req.Save(last); err != nil {
		return Machine{}, fmt.Errorf("recording the lease's provider state: %w", err)
	}
	var t proto.LeaseTarget
	if err := json.Unmarshal(last, &t); err != nil {
		return Machine{}, fmt.Errorf("acquire output: %v", err)
	}
	return Machine{Target: t, State: last}, nil
}

func (p CommandProvider) ReleaseSpec() ReleaseSpec {
	return ReleaseSpec{Command: p.ReleaseCommand}
}

func (p CommandProvider) Release(ctx context.Context, req ReleaseRequest) error {
	cmd := exec.CommandContext(ctx, p.ReleaseCommand[0], p.ReleaseCommand[1:]...)
	cmd.Env = append(os.Environ(), "ERRAND_LEASE_ID="+req.LeaseID, "ERRAND_OFFER="+req.Offer, "ERRAND_LEASE_STATE="+string(req.State))
	cmd.WaitDelay = 5 * time.Second
	nowindow.Hide(cmd)
	output := &limitbuf.Buffer{Limit: 4096}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(output.String())
		if i := strings.LastIndexByte(detail, '\n'); i >= 0 {
			detail = detail[i+1:]
		}
		return fmt.Errorf("%v %s", err, detail)
	}
	return nil
}

// checkTarget keeps leases to remote runners a client can use: a broker
// must not be able to point a client at a socket on the client's own
// machine, and a target no client can use is refused while its machine can
// still be released at once.
func checkTarget(t proto.LeaseTarget) error {
	if err := t.Check(); err != nil {
		return fmt.Errorf("provider target: %w", err)
	}
	return nil
}
