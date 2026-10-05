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
	"slices"
	"strings"
	"sync"
	"time"
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

// lambdaArch maps Lambda's architecture names to Go's; unknown names map
// to "" and are not checked.
func lambdaArch(name string) string {
	switch strings.ToLower(name) {
	case "x86_64", "amd64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	}
	return ""
}

func (p *LambdaProvider) pickRegion(ctx context.Context, key string) (string, int, error) {
	var types struct {
		Data map[string]struct {
			InstanceType struct {
				PriceCentsPerHour int    `json:"price_cents_per_hour"`
				Architecture      string `json:"architecture"`
			} `json:"instance_type"`
			Regions []struct {
				Name string `json:"name"`
			} `json:"regions_with_capacity_available"`
		} `json:"data"`
	}
	if err := p.call(ctx, key, http.MethodGet, "/instance-types", nil, &types); err != nil {
		return "", 0, fmt.Errorf("listing Lambda instance types: %w", err)
	}
	t, ok := types.Data[p.InstanceType]
	if !ok {
		return "", 0, fmt.Errorf("Lambda has no instance type %q", p.InstanceType)
	}
	// errand_binary was checked against the offer's arch; the machine must
	// match it, or the paid instance could never run errand.
	if arch := lambdaArch(t.InstanceType.Architecture); arch != "" && p.Arch != "" && arch != p.Arch {
		return "", 0, fmt.Errorf("Lambda instance type %s is %s but the offer says arch = %q", p.InstanceType, t.InstanceType.Architecture, p.Arch)
	}
	var available []string
	for _, r := range t.Regions {
		available = append(available, r.Name)
	}
	regions := p.Regions
	if len(p.FileSystems) > 0 {
		// Lambda attaches a filesystem only to instances in its own region.
		fsRegion, err := p.fileSystemRegion(ctx, key)
		if err != nil {
			return "", 0, err
		}
		if len(regions) > 0 && !slices.Contains(regions, fsRegion) {
			return "", 0, fmt.Errorf("the offer's file systems are in %s, which regions does not list", fsRegion)
		}
		regions = []string{fsRegion}
	}
	if len(regions) == 0 && len(available) > 0 {
		return available[0], t.InstanceType.PriceCentsPerHour, nil
	}
	for _, want := range regions {
		if slices.Contains(available, want) {
			return want, t.InstanceType.PriceCentsPerHour, nil
		}
	}
	where := "any region"
	if len(regions) > 0 {
		where = strings.Join(regions, ", ")
	}
	return "", 0, fmt.Errorf("Lambda has no %s capacity in %s right now", p.InstanceType, where)
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

// checkSSHKeyName makes sure ssh_key_name is registered with Lambda and, when
// the private key's public half is known, that it is that key: otherwise the
// machine would be rented and then refuse the install.
func (p *LambdaProvider) checkSSHKeyName(ctx context.Context, key, public string) error {
	var list struct {
		Data []struct {
			Name      string `json:"name"`
			PublicKey string `json:"public_key"`
		} `json:"data"`
	}
	if err := p.call(ctx, key, http.MethodGet, "/ssh-keys", nil, &list); err != nil {
		return fmt.Errorf("listing Lambda SSH keys: %w", err)
	}
	for _, k := range list.Data {
		if k.Name != p.SSHKeyName {
			continue
		}
		if public != "" && sshKeyBody(k.PublicKey) != sshKeyBody(public) {
			return fmt.Errorf("Lambda SSH key %q is not the public half of ssh_private_key_file %s", p.SSHKeyName, p.SSHPrivateKeyFile)
		}
		return nil
	}
	return fmt.Errorf("Lambda has no SSH key named %q; add it in the Lambda console", p.SSHKeyName)
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

func (p *LambdaProvider) waitActive(ctx context.Context, key, id string, progress func(string)) (string, error) {
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
