// Package telemetry sends a small, fixed set of client usage events.
// It never accepts commands, paths, errors, or arbitrary event properties.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ProjectToken is a public ingestion token, never a personal API key.
var ProjectToken = "phc_q6XQAkzrDyMAz3gfCty4nKLhowVVYK7FrYEUrfnfG7k4"

const Host = "https://us.i.posthog.com"

var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func IsRelease(version string) bool { return releaseVersion.MatchString(version) }

// Operation returns only known, eligible operations, never arbitrary CLI input.
func Operation(name string) string {
	switch name {
	case "run", "peers", "workspaces", "config", "doctor", "attach", "push", "fetch", "ps", "status", "kill", "df", "gc":
		return name
	}
	return ""
}

type Options struct {
	Enabled        bool
	NoticeRequired bool
	Preview        bool
	Version        string
	Token          string
	IDPath         string
	Output         io.Writer
}

type Run struct {
	Transport  string `json:"transport"`
	Workspace  bool   `json:"persistent_workspace"`
	Caches     bool   `json:"caches"`
	Artifacts  bool   `json:"artifacts"`
	Forwarding bool   `json:"forwarding"`
	Apply      bool   `json:"automatic_apply"`
	Detached   bool   `json:"start_detached"`
}

type properties struct {
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Operation string `json:"operation"`
	Outcome   string `json:"outcome,omitempty"`
	*Run
	PersonProfile bool `json:"$process_person_profile"`
	DisableGeoIP  bool `json:"$geoip_disable"`
}

type event struct {
	Token      string     `json:"api_key,omitempty"`
	Name       string     `json:"event"`
	ID         string     `json:"distinct_id"`
	Properties properties `json:"properties"`
}

type Reporter struct {
	options  Options
	id       string
	endpoint string
	ctx      context.Context
	pending  sync.WaitGroup
	cancel   context.CancelFunc
}

// New is inert when disabled or without a configured release token.
// Preview prints the same schema with a placeholder ID and no token. It does
// not read/create identity state or contact PostHog, even when opted in.
func New(opts Options) *Reporter {
	return newReporter(opts, Host+"/i/v0/e/")
}

func newReporter(opts Options, endpoint string) *Reporter {
	if opts.Preview {
		if opts.Output == nil {
			opts.Output = io.Discard
		}
		return &Reporter{options: opts, id: "preview"}
	}
	if !opts.Enabled || opts.Token == "" || !IsRelease(opts.Version) {
		return nil
	}
	if opts.NoticeRequired && !noticeShown(opts.IDPath+".notice", opts.Output) {
		return nil
	}
	id, err := installationID(opts.IDPath)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Reporter{options: opts, id: id, endpoint: endpoint, ctx: ctx, cancel: cancel}
}

const notice = "Errand collects limited usage telemetry starting next time.\nDetails and opt-out: https://github.com/lydakis/errand/blob/main/docs/TELEMETRY.md\n"

// Default-on telemetry requires a previously displayed notice. On the first
// eligible invocation only the notice is written; failure to persist it keeps
// telemetry off. Explicit opt-in does not require a notice.
func noticeShown(path string, output io.Writer) bool {
	const shown = "shown\n"
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return false
		}
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, int64(len(shown)+1)))
		return err == nil && string(data) == shown
	} else if !os.IsNotExist(err) {
		return false
	}
	if output == nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false
	}
	// Reserve writable state before printing. An empty marker also keeps other
	// processes from sending or printing while this invocation shows the notice.
	// Only a complete marker enables later invocations.
	published := false
	defer func() {
		if !published {
			_ = os.Remove(path)
		}
	}()
	if err := f.Close(); err != nil {
		return false
	}
	ready, err := os.CreateTemp(filepath.Dir(path), ".telemetry-notice-*")
	if err != nil {
		return false
	}
	defer os.Remove(ready.Name())
	_, writeErr := ready.WriteString(shown)
	closeErr := ready.Close()
	if writeErr != nil || closeErr != nil {
		return false
	}
	if _, err := io.WriteString(output, notice); err != nil {
		return false
	}
	// Publish only a fully written and closed marker after successful display.
	published = os.Rename(ready.Name(), path) == nil
	return false
}

// Admitted observes confirmed admission, independent of whether the caller
// stays attached. Run settings must describe the effective admitted job.
func (r *Reporter) Admitted(run Run) {
	if r == nil {
		return
	}
	switch run.Transport {
	case "ssh", "local", "http", "https":
	default:
		run.Transport = "unknown"
	}
	r.capture("job_admitted", "run", "", &run)
}

// Finish is called once at CLI exit, after all Admitted calls. Command failure
// is deliberately coarse: remote exit codes overlap Errand's own exit codes.
// Delivery can add at most 200ms of network waiting here. Events are dropped
// on failure, with no disk queue, retries, or effect on the command exit code.
func (r *Reporter) Finish(operation string, code int) {
	if r == nil {
		return
	}
	outcome := "success"
	if code != 0 {
		outcome = "nonzero_exit"
	}
	r.capture("cli_finished", operation, outcome, nil)
	if r.options.Preview {
		return
	}
	done := make(chan struct{})
	go func() {
		r.pending.Wait()
		close(done)
	}()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	defer r.cancel()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (r *Reporter) capture(name, operation, outcome string, run *Run) {
	operation = Operation(operation)
	if operation == "" {
		operation = "unknown"
	}
	version := r.options.Version
	if !IsRelease(version) {
		version = "development"
	}
	p := event{Token: r.options.Token, Name: name, ID: r.id, Properties: properties{
		Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH, Operation: operation, Outcome: outcome, Run: run, DisableGeoIP: true,
	}}
	if r.options.Preview {
		p.Token = ""
	}
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	if r.options.Preview {
		fmt.Fprintf(r.options.Output, "[telemetry] %s\n", data)
		return
	}
	// At most two events per CLI invocation. Independent requests ensure a slow
	// admission response cannot consume the exit event's entire delivery budget.
	r.pending.Go(func() { send(r.ctx, r.endpoint, data) })
}

func send(ctx context.Context, endpoint string, payload []byte) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "errand-telemetry")
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func installationID(path string) (string, error) {
	read := func() (string, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("installation ID is not a regular file")
		}
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 65))
		if err != nil {
			return "", err
		}
		id := string(b)
		if len(id) != 64 || strings.ToLower(id) != id {
			return "", fmt.Errorf("invalid installation ID")
		}
		if _, err := hex.DecodeString(id); err != nil {
			return "", err
		}
		return id, nil
	}
	if id, err := read(); !os.IsNotExist(err) {
		return id, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	f, err := os.CreateTemp(filepath.Dir(path), ".telemetry-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.WriteString(id)
	closeErr := f.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	// Publish a complete ID without replacing another process's identity.
	if err := os.Link(f.Name(), path); err != nil {
		if os.IsExist(err) {
			return read()
		}
		return "", err
	}
	return id, nil
}
