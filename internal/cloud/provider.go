package cloud

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

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
}

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
	var stdout limitedBuffer
	stdout.limit = maxAcquireOutput
	cmd.Stdout = &stdout
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
	err = cmd.Wait()
	if ctx.Err() != nil {
		return Machine{}, ctx.Err()
	}
	if err != nil {
		return Machine{}, fmt.Errorf("acquire command failed: %v", err)
	}
	if stdout.overflow {
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

func (p CommandProvider) Release(ctx context.Context, req ReleaseRequest) error {
	cmd := exec.CommandContext(ctx, p.ReleaseCommand[0], p.ReleaseCommand[1:]...)
	cmd.Env = append(os.Environ(), "ERRAND_LEASE_ID="+req.LeaseID, "ERRAND_OFFER="+req.Offer, "ERRAND_LEASE_STATE="+string(req.State))
	cmd.WaitDelay = 5 * time.Second
	nowindow.Hide(cmd)
	var output limitedBuffer
	output.limit = 4096
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(output.String())
		if i := strings.LastIndexByte(detail, '\n'); i >= 0 {
			detail = detail[i+1:]
		}
		return fmt.Errorf("%v %s", err, detail)
	}
	return nil
}

// checkTarget keeps leases to remote runners: a broker must not be able to
// point a client at a socket on the client's own machine.
func checkTarget(t proto.LeaseTarget) error {
	if t.URL == "" && t.SSH == "" {
		return errors.New("provider named no url or ssh target")
	}
	if t.HostKey != "" && t.SSH == "" {
		return errors.New("provider named a host_key without an ssh target")
	}
	return nil
}

// ValidSSHPublicKey accepts one authorized_keys line: a key type, its base64
// blob and an optional comment, without options.
func ValidSSHPublicKey(s string) bool {
	fields := strings.Fields(s)
	if len(fields) < 2 || len(s) > 16<<10 || strings.ContainsAny(s, "\r\n") || !strings.HasPrefix(fields[0], "ssh-") && !strings.HasPrefix(fields[0], "ecdsa-") && !strings.HasPrefix(fields[0], "sk-") {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(fields[1])
	return err == nil
}
