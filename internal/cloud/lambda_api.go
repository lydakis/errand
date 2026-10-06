package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/client"
)

const lambdaAPI = "https://cloud.lambda.ai/api/v1"

// Lambda allows one API request per second and one launch per 12 seconds,
// per account.
const (
	lambdaRequestGap = time.Second
	lambdaLaunchGap  = 12 * time.Second
)

// lambdaPacing spaces requests to Lambda so they are not refused with 429.
// It only avoids refusals: send retries any it still gets, so pacing never
// decides whether a request succeeds.
//
// Lambda's limits are per account, and API keys of one account cannot be told
// apart from keys of another, so all Lambda offers on a cloud peer share one
// pacing. Offers on separate accounts then wait on each other, which costs
// seconds.
//
// Every request passes the request gate. A launch also holds the launch gate
// from before its gap until Lambda answers, so launches go one at a time and
// in turn, while other requests keep passing the request gate.
type lambdaPacing struct {
	request, launch gate
	// passed, when set, sees each time a gate lets a caller through, as
	// that gate records it. Tests use it to check the gaps.
	passed func(launch bool, at time.Time)
}

var sharedLambdaPacing lambdaPacing

// gate is held by one caller at a time, which waits out the gap since the
// last caller went through and then records its own time. A caller that
// gives up while waiting changes nothing.
type gate struct {
	once sync.Once
	turn chan struct{} // holds a token while the gate is held
	last time.Time     // when the last caller went through; guarded by turn
}

func (g *gate) take(ctx context.Context) error {
	g.once.Do(func() { g.turn = make(chan struct{}, 1) })
	select {
	case g.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) give() { <-g.turn }

// wait lets one request through the request gate.
func (a *lambdaPacing) wait(ctx context.Context, gap time.Duration) error {
	if err := a.request.take(ctx); err != nil {
		return err
	}
	defer a.request.give()
	if err := sleep(ctx, time.Until(a.request.last.Add(gap))); err != nil {
		return err
	}
	a.request.last = time.Now()
	if a.passed != nil {
		a.passed(false, a.request.last)
	}
	return nil
}

func (p *LambdaProvider) pacing() *lambdaPacing {
	if p.Pacing != nil {
		return p.Pacing
	}
	return &sharedLambdaPacing
}

// call makes one API request, keeping to Lambda's request limit and retrying
// requests it rate-limits.
func (p *LambdaProvider) call(ctx context.Context, key, method, path string, body, out any) error {
	a := p.pacing()
	var limited error // the last 429
	for {
		if err := a.wait(ctx, p.requestGap()); err != nil {
			return firstErr(limited, err)
		}
		err := p.callOnce(ctx, key, method, path, body, out)
		if !isRateLimited(err) {
			return err
		}
		limited = err
	}
}

// launch sends a launch request, keeping to both limits and retrying while
// Lambda rate-limits it. sending runs before each attempt goes out, records
// when, and can stop it. The request follows within lambdaSendSlack of that
// record: if saving it or waiting for the request gate took longer, sending
// runs again. The error wraps errNotSent when every attempt was stopped
// before going out or refused with 429, so nothing was created.
func (p *LambdaProvider) launch(ctx context.Context, key string, body, out any, sending func() error) error {
	a := p.pacing()
	if err := a.launch.take(ctx); err != nil {
		return fmt.Errorf("%w (%w)", err, errNotSent)
	}
	defer a.launch.give()
	var limited error // the last 429
	for {
		err := sleep(ctx, time.Until(a.launch.last.Add(p.launchGap())))
		for err == nil {
			started := time.Now()
			err = sending()
			if err == nil {
				err = a.wait(ctx, p.requestGap())
			}
			if err == nil {
				// Saving may have outlasted the deadline; then nothing is sent.
				err = ctx.Err()
			}
			if err != nil || time.Since(started) <= p.sendSlack() {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("%w (%w)", firstErr(limited, err), errNotSent)
		}
		a.launch.last = time.Now() // the gap runs from here, as no launch can pass this one
		if a.passed != nil {
			a.passed(true, a.launch.last)
		}
		err = p.callOnce(ctx, key, http.MethodPost, "/instance-operations/launch", body, out)
		if !isRateLimited(err) {
			return err
		}
		limited = err
	}
}

// lambdaSendSlack bounds how long before a launch request its saved launch
// time may be, so release's settle window, which runs from that time, still
// covers the request.
const lambdaSendSlack = 30 * time.Second

func (p *LambdaProvider) sendSlack() time.Duration {
	if p.SendSlack > 0 {
		return p.SendSlack
	}
	return lambdaSendSlack
}

// errNotSent marks a launch that did not go out, or only went out to be
// refused with 429, so it created nothing.
var errNotSent = errors.New("launch not sent")

// firstErr prefers a rate-limit refusal over the wait error it led to.
func firstErr(limited, err error) error {
	if limited != nil {
		return limited
	}
	return err
}

func isRateLimited(err error) bool {
	var api *lambdaAPIError
	return errors.As(err, &api) && api.Status == http.StatusTooManyRequests
}

func (p *LambdaProvider) requestGap() time.Duration {
	if p.RequestGap > 0 {
		return p.RequestGap
	}
	return lambdaRequestGap
}

func (p *LambdaProvider) launchGap() time.Duration {
	if p.LaunchGap > 0 {
		return p.LaunchGap
	}
	return lambdaLaunchGap
}

func (p *LambdaProvider) callOnce(ctx context.Context, key, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	base := p.BaseURL
	if base == "" {
		base = lambdaAPI
	}
	r, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Accept", "application/json")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code       string `json:"code"`
				Message    string `json:"message"`
				Suggestion string `json:"suggestion"`
			} `json:"error"`
		}
		apiErr := &lambdaAPIError{Status: resp.StatusCode}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			apiErr.Msg = e.Error.Message
			if e.Error.Suggestion != "" {
				apiErr.Msg += " (" + e.Error.Suggestion + ")"
			}
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

type lambdaAPIError struct {
	Status int
	Msg    string
}

func (e *lambdaAPIError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("Lambda API %d", e.Status)
	}
	return fmt.Sprintf("Lambda API %d: %s", e.Status, e.Msg)
}

// lambdaArch maps Lambda's architecture names to Go's. ok is false for a
// name errand does not know.
func lambdaArch(name string) (arch string, ok bool) {
	switch strings.ToLower(name) {
	case "x86_64", "amd64":
		return "amd64", true
	case "arm64", "aarch64":
		return "arm64", true
	}
	return "", false
}

func (p *LambdaProvider) pickRegion(ctx context.Context, key string) (string, int, error) {
	types, err := p.instanceTypes(ctx, key)
	if err != nil {
		return "", 0, err
	}
	i := slices.IndexFunc(types, func(t lambdaInstanceType) bool { return t.Name == p.InstanceType })
	if i < 0 {
		return "", 0, fmt.Errorf("Lambda has no instance type %q", p.InstanceType)
	}
	t := types[i]
	// errand_binary was checked against the offer's arch; the machine must
	// match it, or the paid instance could never run errand.
	arch, ok := lambdaArch(t.Architecture)
	if !ok {
		return "", 0, fmt.Errorf("Lambda instance type %s has architecture %q, which errand does not know, so it cannot tell whether errand_binary runs there", p.InstanceType, t.Architecture)
	}
	if arch != p.Arch {
		return "", 0, fmt.Errorf("Lambda instance type %s is %s but the offer says arch = %q", p.InstanceType, t.Architecture, p.Arch)
	}
	// The price may have changed since the offer was listed.
	if price := float64(t.PriceCentsPerHour) / 100; p.MaxPricePerHour > 0 && price > p.MaxPricePerHour {
		return "", 0, fmt.Errorf("Lambda now charges $%.2f/h for %s, above max_price_per_hour = %g in [cloud.lambda]", price, p.InstanceType, p.MaxPricePerHour)
	}
	regions, err := p.launchRegions(ctx, key)
	if err != nil {
		return "", 0, err
	}
	available := t.Regions
	if len(regions) == 0 && len(available) > 0 {
		return available[0], t.PriceCentsPerHour, nil
	}
	for _, want := range regions {
		if slices.Contains(available, want) {
			return want, t.PriceCentsPerHour, nil
		}
	}
	where := "any region"
	if len(regions) > 0 {
		where = strings.Join(regions, ", ")
	}
	return "", 0, fmt.Errorf("Lambda has no %s capacity in %s right now", p.InstanceType, where)
}

// launchRegions are the regions a launch may use, in preference order, or
// none for any: the region of the offer's file systems when it attaches any,
// since Lambda attaches a file system only to instances in its own region,
// and otherwise Regions.
func (p *LambdaProvider) launchRegions(ctx context.Context, key string) ([]string, error) {
	if len(p.FileSystems) == 0 {
		return p.Regions, nil
	}
	fsRegion, err := p.fileSystemRegion(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(p.Regions) > 0 && !slices.Contains(p.Regions, fsRegion) {
		return nil, fmt.Errorf("the offer's file systems are in %s, which regions does not list", fsRegion)
	}
	return []string{fsRegion}, nil
}

type lambdaFileSystem struct {
	Name   string `json:"name"`
	Region struct {
		Name string `json:"name"`
	} `json:"region"`
}

// fileSystemRegion finds the one region holding every configured filesystem.
func (p *LambdaProvider) fileSystemRegion(ctx context.Context, key string) (string, error) {
	var list struct {
		Data []lambdaFileSystem `json:"data"`
	}
	if err := p.call(ctx, key, http.MethodGet, "/file-systems", nil, &list); err != nil {
		return "", fmt.Errorf("listing Lambda file systems: %w", err)
	}
	region := ""
	for _, name := range p.FileSystems {
		i := slices.IndexFunc(list.Data, func(fs lambdaFileSystem) bool { return fs.Name == name })
		if i < 0 {
			return "", fmt.Errorf("Lambda has no file system %q", name)
		}
		switch r := list.Data[i].Region.Name; {
		case region == "":
			region = r
		case r != region:
			return "", fmt.Errorf("file systems %q and %q are in different regions (%s, %s); an instance attaches only its own region's", p.FileSystems[0], name, region, r)
		}
	}
	return region, nil
}

// lambdaKeyMu keeps launches from making or registering the key twice.
var lambdaKeyMu sync.Mutex

// registerKey returns the name Lambda knows the cloud peer's own SSH key by,
// making the key and adding it to the account first if needed. Lambda puts
// it on every machine the cloud peer launches, which then installs errand
// and watches the runner with it.
func (p *LambdaProvider) registerKey(ctx context.Context, apiKey string) (string, error) {
	lambdaKeyMu.Lock()
	defer lambdaKeyMu.Unlock()
	if p.KeyDir == "" {
		return "", errors.New("lambda provider has no directory for its SSH key")
	}
	public, err := p.keygen()(ctx, p.keyFile(), "errand-cloud-peer")
	if err != nil {
		return "", err
	}
	var list struct {
		Data []struct {
			Name      string `json:"name"`
			PublicKey string `json:"public_key"`
		} `json:"data"`
	}
	if err := p.call(ctx, apiKey, http.MethodGet, "/ssh-keys", nil, &list); err != nil {
		return "", fmt.Errorf("listing Lambda SSH keys: %w", err)
	}
	for _, k := range list.Data {
		if sshKeyBody(k.PublicKey) == sshKeyBody(public) {
			return k.Name, nil
		}
	}
	name := "errand-" + keyID(sshKeyBody(public))
	var added struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := p.call(ctx, apiKey, http.MethodPost, "/ssh-keys", map[string]string{"name": name, "public_key": public}, &added); err != nil {
		return "", fmt.Errorf("adding errand's SSH key to Lambda: %w", err)
	}
	return name, nil
}

func (p *LambdaProvider) keyFile() string {
	return filepath.Join(p.KeyDir, "lambda_ed25519")
}

func (p *LambdaProvider) keygen() func(context.Context, string, string) (string, error) {
	if p.Keygen != nil {
		return p.Keygen
	}
	return client.EnsureSSHKey
}

// sshKeyBody is a public key's type and data, without its comment.
func sshKeyBody(public string) string {
	fields := strings.Fields(public)
	if len(fields) < 2 {
		return public
	}
	return fields[0] + " " + fields[1]
}

type lambdaInstance struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	IP     string `json:"ip"`
}

func lambdaGone(status string) bool {
	return status == "terminated" || status == "terminating" || status == "preempted"
}

// instances lists every running instance, following all pages: release must
// not conclude a machine is gone because it was on a later page.
func (p *LambdaProvider) instances(ctx context.Context, key string) ([]lambdaInstance, error) {
	var all []lambdaInstance
	seen := map[string]bool{}
	token := ""
	for {
		path := "/instances"
		if token != "" {
			path += "?page_token=" + url.QueryEscape(token)
		}
		var page struct {
			Data      []lambdaInstance `json:"data"`
			PageToken string           `json:"page_token"`
		}
		if err := p.call(ctx, key, http.MethodGet, path, nil, &page); err != nil {
			return nil, fmt.Errorf("listing Lambda instances: %w", err)
		}
		all = append(all, page.Data...)
		if page.PageToken == "" {
			return all, nil
		}
		if seen[page.PageToken] {
			return nil, fmt.Errorf("listing Lambda instances: page token %q repeated", page.PageToken)
		}
		seen[page.PageToken] = true
		token = page.PageToken
	}
}

// waitActive polls the instance until it is active and returns its address.
// seen runs once, the first time Lambda shows the instance, and stops the
// wait if it fails.
func (p *LambdaProvider) waitActive(ctx context.Context, key, id string, progress func(string), seen func() error) (string, error) {
	last := ""
	for {
		var got struct {
			Data lambdaInstance `json:"data"`
		}
		if err := p.call(ctx, key, http.MethodGet, "/instances/"+id, nil, &got); err != nil {
			// The machine is booting either way; a dropped connection or a
			// server error is no reason to give it up.
			var api *lambdaAPIError
			if ctx.Err() != nil || errors.As(err, &api) && api.Status/100 == 4 {
				return "", fmt.Errorf("checking instance %s: %w", id, err)
			}
			if last != "retrying" {
				progress(fmt.Sprintf("checking instance %s failed (%v); retrying", id, err))
			}
			last = "retrying"
			if err := sleep(ctx, p.poll()); err != nil {
				return "", err
			}
			continue
		}
		if seen != nil {
			if err := seen(); err != nil {
				return "", err
			}
			seen = nil
		}
		in := got.Data
		if in.Status != last {
			progress("instance " + in.Status)
			last = in.Status
		}
		switch {
		case in.Status == "active" && in.IP != "":
			return in.IP, nil
		case in.Status == "unhealthy" || lambdaGone(in.Status):
			return "", fmt.Errorf("instance %s is %s", id, in.Status)
		}
		if err := sleep(ctx, p.poll()); err != nil {
			return "", err
		}
	}
}

func (p *LambdaProvider) poll() time.Duration {
	if p.Poll > 0 {
		return p.Poll
	}
	return 5 * time.Second
}
