package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/lydakis/errand/internal/fsowner"
	"github.com/lydakis/errand/internal/proto"
)

// LambdaProvider rents Lambda Cloud instances. It launches one, installs
// errand on it over SSH and terminates it on release. With a Tailscale auth
// key the machine joins the tailnet and clients reach it there; without one
// they reach it over SSH, with its host key pinned. See docs/CLOUD.md.
//
// Lambda's launch request cannot be retried safely: a launch whose answer is
// lost may still have created a billed instance. So the provider keeps one
// rule, which TestLambdaReleaseAfterAnyStop checks at every step acquire can
// stop at: release finds and terminates every instance that may belong to
// the lease. To make that possible, acquire saves lambdaState before each
// launch attempt goes out, names the instance after the lease, and takes the
// state back only when Lambda refused every attempt.
type LambdaProvider struct {
	APIKeyFile           string
	InstanceType         string
	Regions              []string // preference order; empty means any with capacity
	FileSystems          []string
	TailscaleAuthKeyFile string   // empty: clients reach the machine over SSH
	ErrandBinary         string   // a linux build for Arch; empty: this build, or its release
	Arch                 string   // the instance's architecture, amd64 or arm64
	Version              string   // errand's version, for fetching its release
	AllowUsers           []string // tailnet logins admitted besides the caller
	// MaxPricePerHour refuses a launch at a higher price than this, in USD,
	// as Lambda lists it right before launching; 0 means no cap.
	MaxPricePerHour float64
	// KeyDir holds the cloud peer's own SSH key, made and registered with
	// Lambda on first use. The broker sets it to its state directory.
	KeyDir string

	// Tests replace these.
	BaseURL    string
	ReleaseURL string
	HTTP       *http.Client
	SSH        func(ctx context.Context, args []string, stdin io.Reader) ([]byte, error)
	HostKey    func(ctx context.Context) (private, public string, err error)
	Keygen     func(ctx context.Context, path, comment string) (public string, err error)
	Now        func() time.Time
	Poll       time.Duration
	LaunchGap  time.Duration
	RequestGap time.Duration
	SendSlack  time.Duration
	Pacing     *lambdaPacing // defaults to the one all Lambda offers share
}

// lambdaState is what release needs to find a lease's instance. Acquire
// saves it before the first launch attempt and adds to it as it learns more;
// no saved state means no launch request ever went out.
type lambdaState struct {
	// Sent is when the latest launch attempt went out, give or take
	// lambdaSendSlack. An instance it created may take a while to be listed,
	// so until lambdaLaunchSettle after it, not finding one proves nothing.
	Sent time.Time `json:"sent"`
	// KeyID fingerprints the API key that launched. Another key may belong
	// to another account, which does not list the instance.
	KeyID      string `json:"key_id"`
	Region     string `json:"region"`
	InstanceID string `json:"instance_id,omitempty"` // once Lambda answered
	// Seen is set once Lambda has shown the instance. A lease has at most
	// one instance: launch sends no attempt after one that Lambda did not
	// refuse with 429, and a 429 created nothing. So once it has been seen,
	// the launch has settled, and when Lambda no longer lists it, it has
	// been terminated.
	Seen bool `json:"seen,omitempty"`
	proto.LeaseTarget
}

// lambdaLaunchSettle bounds how long a launched instance may go unlisted.
const lambdaLaunchSettle = 10 * time.Minute

// LeaseHostname is the instance and tailnet name of a lease's machine, so
// release can find it even when the launch's answer was lost.
func LeaseHostname(leaseID string) string {
	return "errand-" + strings.ToLower(leaseID)
}

func (p *LambdaProvider) Acquire(ctx context.Context, req AcquireRequest) (Machine, error) {
	tailnet := p.TailscaleAuthKeyFile != ""
	// On the tailnet the machine admits tailnet logins, and a request over
	// SSH or the local socket has none; refuse before paying for a machine
	// that would admit no one.
	if tailnet && req.Login == "" && len(p.allowUsers()) == 0 {
		return Machine{}, errors.New("this lease request has no tailnet login to admit (it came over SSH or the local socket); ask over the tailnet, or set allow_users on the offer")
	}
	// Over SSH the machine admits only the key the client sent.
	if !tailnet && req.SSHKey == "" {
		return Machine{}, errors.New("this lease request carries no SSH key for the machine to admit; update errand where you ran it")
	}
	key, err := readSecret(p.APIKeyFile, "Lambda API key")
	if err != nil {
		return Machine{}, err
	}
	var authKey string
	if tailnet {
		if authKey, err = readSecret(p.TailscaleAuthKeyFile, "Tailscale auth key"); err != nil {
			return Machine{}, err
		}
	}
	// Everything the install needs is checked, and fetched, before paying
	// for a machine.
	if err := p.checkInstall(); err != nil {
		return Machine{}, err
	}
	binary, err := p.errandBinary(ctx, req.Progress)
	if err != nil {
		return Machine{}, err
	}
	defer removeCopy(binary)
	keyName, err := p.registerKey(ctx, key)
	if err != nil {
		return Machine{}, err
	}
	region, price, err := p.pickRegion(ctx, key)
	if err != nil {
		return Machine{}, err
	}
	req.Progress(fmt.Sprintf("launching %s in %s ($%.2f/h)", p.InstanceType, region, float64(price)/100))
	name := LeaseHostname(req.LeaseID)
	// The instance boots with a host key made here, so the one SSH
	// connection that carries the auth key can verify whom it talks to.
	hostPrivate, hostPublic, err := p.hostKey()(ctx)
	if err != nil {
		return Machine{}, fmt.Errorf("making the machine's SSH host key: %w", err)
	}
	launch := map[string]any{
		"region_name":        region,
		"instance_type_name": p.InstanceType,
		"ssh_key_names":      []string{keyName},
		"name":               name,
		"user_data":          hostKeyCloudConfig(hostPrivate, hostPublic),
	}
	if len(p.FileSystems) > 0 {
		launch["file_system_names"] = p.FileSystems
	}
	var launched struct {
		Data struct {
			InstanceIDs []string `json:"instance_ids"`
		} `json:"data"`
	}
	state := lambdaState{KeyID: keyID(key), Region: region}
	// Nothing is sent unless its state is saved first.
	sending := func() error {
		state.Sent = p.now()
		return saveState(req, state)
	}
	if err := p.launch(ctx, key, launch, &launched, sending); err != nil {
		// Only a launch Lambda refused, or one that never went out, is known
		// to have created nothing. Any other failure leaves the state saved,
		// and release looks for the instance.
		var api *lambdaAPIError
		if errors.As(err, &api) && api.Status/100 == 4 || errors.Is(err, errNotSent) {
			_ = req.Save(nil) // if this fails, release only looks for nothing
		}
		return Machine{}, fmt.Errorf("launching %s: %w", p.InstanceType, err)
	}
	if len(launched.Data.InstanceIDs) != 1 {
		return Machine{}, fmt.Errorf("Lambda launched %d instances, want 1", len(launched.Data.InstanceIDs))
	}
	state.InstanceID = launched.Data.InstanceIDs[0]
	if err := saveState(req, state); err != nil {
		return Machine{}, err
	}

	seen := func() error {
		state.Seen = true
		return saveState(req, state)
	}
	ip, err := p.waitActive(ctx, key, state.InstanceID, req.Progress, seen)
	if err != nil {
		return Machine{}, err
	}
	req.Progress("instance is up at " + ip + "; installing errand")
	if err := p.install(ctx, binary, ip, hostPublic, name, authKey, req.Login, req.SSHKey); err != nil {
		return Machine{}, err
	}
	if tailnet {
		state.LeaseTarget = proto.LeaseTarget{URL: "http://" + name + ":7443"}
	} else {
		// The runner listens on no port; jobs reach it over SSH, as the
		// login it runs as. This cloud peer watches it with its own key.
		state.LeaseTarget = proto.LeaseTarget{SSH: lambdaUser + "@" + ip, HostKey: hostPublic}
	}
	if err := saveState(req, state); err != nil {
		return Machine{}, err
	}
	data, _ := json.Marshal(state)
	m := Machine{Target: state.LeaseTarget, State: data}
	if !tailnet {
		m.Identity = p.keyFile()
	}
	return m, nil
}

func saveState(req AcquireRequest, state lambdaState) error {
	data, _ := json.Marshal(state)
	if err := req.Save(data); err != nil {
		return fmt.Errorf("recording the lease's Lambda state: %w", err)
	}
	return nil
}

// Release terminates every instance that may belong to the lease: the one
// whose ID was saved and any named for the lease, which covers a launch
// whose answer was lost. It succeeds only once none is left, or when not
// finding one is proof that none exists.
func (p *LambdaProvider) Release(ctx context.Context, req ReleaseRequest) error {
	if len(req.State) == 0 {
		return nil // no launch request ever went out
	}
	var state lambdaState
	if err := json.Unmarshal(req.State, &state); err != nil || state.Sent.IsZero() {
		return fmt.Errorf("lease %s has unreadable Lambda state %s; terminate %s in the Lambda console if it exists", req.LeaseID, req.State, LeaseHostname(req.LeaseID))
	}
	key, err := readSecret(p.APIKeyFile, "Lambda API key")
	if err != nil {
		return err
	}
	instances, err := p.instances(ctx, key)
	if err != nil {
		return err
	}
	// Released means Lambda itself lists every instance of the lease as
	// terminated, or no longer lists it at all.
	name := LeaseHostname(req.LeaseID)
	var running, terminating []string
	ended := false
	for _, in := range instances {
		if in.ID != state.InstanceID && in.Name != name {
			continue
		}
		switch {
		case in.Status == "terminated" || in.Status == "preempted":
			ended = true
		case in.Status == "terminating":
			terminating = append(terminating, in.ID)
		default:
			running = append(running, in.ID)
		}
	}
	if len(running) > 0 {
		if err := p.call(ctx, key, http.MethodPost, "/instance-operations/terminate", map[string]any{"instance_ids": running}, nil); err != nil {
			return fmt.Errorf("terminating %s: %w", strings.Join(running, ", "), err)
		}
		return &ReleasePending{"asked Lambda to terminate " + strings.Join(running, ", ") + "; waiting for it to list them as terminated"}
	}
	if len(terminating) > 0 {
		return &ReleasePending{"waiting for Lambda to finish terminating " + strings.Join(terminating, ", ")}
	}
	// Absence means something only to the key that launched: another key
	// may belong to another account. To that key, an instance Lambda has
	// shown and no longer lists has been terminated. Only one never seen may
	// still be registering.
	switch {
	case ended:
		return nil
	case state.KeyID != keyID(key):
		return fmt.Errorf("the Lambda API key changed since %s was launched and this key does not see it; restore the old key in api_key_file, or terminate %s in the Lambda console", name, name)
	case state.Seen:
		return nil
	case p.now().Sub(state.Sent) < lambdaLaunchSettle:
		return fmt.Errorf("no instance named %s yet; checking again in case its launch is still registering", name)
	}
	return nil
}

func (p *LambdaProvider) ReleaseSpec() ReleaseSpec {
	return ReleaseSpec{Lambda: &LambdaRelease{APIKeyFile: p.APIKeyFile}}
}

func (p *LambdaProvider) useStateDir(dir string) {
	if p.KeyDir == "" {
		p.KeyDir = filepath.Join(dir, "lambda")
	}
}

// lambdaUser is the login on the instance, the only one Lambda's images have.
const lambdaUser = "ubuntu"

func (p *LambdaProvider) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// allowUsers is AllowUsers without entries that are only blanks or control
// characters, which can never match a tailnet login.
func (p *LambdaProvider) allowUsers() []string {
	var users []string
	for _, u := range p.AllowUsers {
		if strings.TrimFunc(u, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) != "" {
			users = append(users, u)
		}
	}
	return users
}

// keyID fingerprints an API key without revealing it.
func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// readSecret reads a one-line secret from the file at path, which must be
// this user's and readable by no one else. Its errors name the file by what
// it holds, never by path: they reach logs and clients, and the path is in
// the cloud peer's own configuration.
func readSecret(path, what string) (string, error) {
	fail := func(problem string, err error) (string, error) {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		if err != nil {
			return "", fmt.Errorf("%s file: %s: %w", what, problem, err)
		}
		return "", fmt.Errorf("%s file %s", what, problem)
	}
	// Opening a FIFO would block until something writes to it, so the type
	// is checked before opening, and again on what was opened.
	if info, err := os.Stat(path); err != nil {
		return fail("reading", err)
	} else if !info.Mode().IsRegular() {
		return fail("is not a regular file", nil)
	}
	f, err := os.Open(path)
	if err != nil {
		return fail("reading", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fail("reading", err)
	}
	if !info.Mode().IsRegular() {
		return fail("is not a regular file", nil)
	}
	if owned, err := fsowner.OwnedByCurrentUser(f); err != nil || !owned {
		return fail("must be owned by the user errand runs as", nil)
	}
	if private, err := fsowner.Private(f); err != nil {
		return fail("checking who can read it", err)
	} else if !private {
		if runtime.GOOS == "windows" {
			return fail("is readable by other users; let only your user, SYSTEM and Administrators read it", nil)
		}
		return fail("is readable by other users; chmod 600 it", nil)
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return fail("reading", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" || strings.ContainsAny(secret, "\r\n") {
		return fail("must hold one line", nil)
	}
	return secret, nil
}

func lastLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

var _ Provider = (*LambdaProvider)(nil)
