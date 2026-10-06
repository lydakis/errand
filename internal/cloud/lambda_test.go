package cloud

import (
	"archive/tar"
	"cmp"
	"context"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lydakis/errand/internal/proto"
)

// fakeInstanceType is one entry of the fake's instance type listing.
type fakeInstanceType struct {
	price       int
	arch, gpu   string
	vcpus, gpus int
	regions     []string // with capacity; nil means the fake's capacity
}

func (t fakeInstanceType) listing(name string, capacity []string) map[string]any {
	var regions []map[string]string
	if t.regions == nil {
		t.regions = capacity
	}
	for _, r := range t.regions {
		regions = append(regions, map[string]string{"name": r, "description": r})
	}
	return map[string]any{
		"instance_type": map[string]any{
			"name": name, "description": "1x " + t.gpu, "gpu_description": t.gpu, "price_cents_per_hour": t.price, "architecture": t.arch,
			"specs": map[string]any{"vcpus": t.vcpus, "memory_gib": 4 * t.vcpus, "storage_gib": 512, "gpus": t.gpus},
		},
		"regions_with_capacity_available": regions,
	}
}

// fakeLambda serves the slice of the Lambda Cloud API the provider uses.
type fakeLambda struct {
	mu          sync.Mutex
	capacity    []string
	instances   map[string]*lambdaInstance
	launches    []map[string]any
	terminated  []string
	polls       int
	rateLimited int                         // requests to refuse with 429 first
	fileSystems map[string]string           // name → region
	sshKeys     map[string]string           // name → public key
	keysAdded   int                         // POST /ssh-keys calls
	launchError int                         // status to refuse launches with
	pollErrors  int                         // status polls to fail with 502 first
	pageSize    int                         // instances per page, when set
	apiKey      string                      // the account's key, if not secret-key
	types       map[string]fakeInstanceType // instance types besides gpu_1x_h100_pcie
	attempts    []time.Time                 // when each launch request arrived
	keepAlive   int                         // terminate calls to accept without terminating
	arch        string                      // gpu_1x_h100_pcie's architecture, if not x86_64
	forget      bool                        // stop listing terminated instances at once
}

func (f *fakeLambda) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key := cmp.Or(f.apiKey, "secret-key"); r.Header.Get("Authorization") != "Bearer "+key {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":"global/invalid-api-key","message":"API key was invalid, expired, or deleted."}}`)
		return
	}
	reply := func(v any) { json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	if f.rateLimited > 0 {
		f.rateLimited--
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"global/quota-exceeded","message":"Too many requests"}}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/instance-types":
		types := map[string]any{}
		h100 := fakeInstanceType{price: 249, arch: cmp.Or(f.arch, "x86_64"), gpu: "H100 (80 GB PCIe)", vcpus: 26, gpus: 1}
		for name, t := range f.types {
			types[name] = t.listing(name, f.capacity)
		}
		types["gpu_1x_h100_pcie"] = h100.listing("gpu_1x_h100_pcie", f.capacity)
		reply(types)
	case r.Method == http.MethodGet && r.URL.Path == "/ssh-keys":
		var list []map[string]string
		for name, public := range f.sshKeys {
			list = append(list, map[string]string{"id": "key-" + name, "name": name, "public_key": public})
		}
		reply(list)
	case r.Method == http.MethodPost && r.URL.Path == "/ssh-keys":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if _, taken := f.sshKeys[body["name"]]; taken || body["public_key"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":"global/invalid-parameters","message":"Invalid SSH key"}}`)
			return
		}
		f.keysAdded++
		f.sshKeys[body["name"]] = body["public_key"]
		reply(map[string]string{"id": "key-" + body["name"], "name": body["name"], "public_key": body["public_key"]})
	case r.Method == http.MethodGet && r.URL.Path == "/file-systems":
		var list []map[string]any
		for name, region := range f.fileSystems {
			list = append(list, map[string]any{"id": "fs-" + name, "name": name, "region": map[string]string{"name": region, "description": region}})
		}
		reply(list)
	case r.Method == http.MethodPost && r.URL.Path == "/instance-operations/launch" && f.launchError != 0:
		f.attempts = append(f.attempts, time.Now())
		w.WriteHeader(f.launchError)
		io.WriteString(w, `{"error":{"code":"instance-operations/launch/insufficient-capacity","message":"Not enough capacity to fulfill launch request."}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/instance-operations/launch":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.launches = append(f.launches, body)
		id := "inst-" + string(rune('a'+len(f.launches)-1))
		f.instances[id] = &lambdaInstance{ID: id, Name: body["name"].(string), Status: "booting"}
		reply(map[string]any{"instance_ids": []string{id}})
	case r.Method == http.MethodGet && r.URL.Path == "/instances":
		var ids []string
		for id := range f.instances {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		start, end := 0, len(ids)
		if f.pageSize > 0 {
			start, _ = strconv.Atoi(r.URL.Query().Get("page_token"))
			end = min(start+f.pageSize, len(ids))
		}
		var list []*lambdaInstance
		for _, id := range ids[start:end] {
			list = append(list, f.instances[id])
		}
		next := ""
		if end < len(ids) {
			next = strconv.Itoa(end)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": list, "page_token": next})
		// Lambda lists a terminating instance as terminated a little later,
		// or stops listing it.
		for _, in := range list {
			switch {
			case in.Status == "terminating" && f.forget:
				delete(f.instances, in.ID)
			case in.Status == "terminating":
				in.Status = "terminated"
			}
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/instances/") && f.pollErrors > 0:
		f.pollErrors--
		w.WriteHeader(http.StatusBadGateway)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/instances/"):
		in, ok := f.instances[strings.TrimPrefix(r.URL.Path, "/instances/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.polls++; f.polls >= 2 && in.Status == "booting" {
			in.Status, in.IP = "active", "203.0.113.7"
		}
		reply(in)
	case r.Method == http.MethodPost && r.URL.Path == "/instance-operations/terminate":
		var body struct {
			IDs []string `json:"instance_ids"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		done := []any{}
		for _, id := range body.IDs {
			if f.keepAlive > 0 {
				f.keepAlive--
				continue
			}
			f.terminated = append(f.terminated, id)
			f.instances[id].Status = "terminating"
			done = append(done, f.instances[id])
		}
		reply(map[string]any{"terminated_instances": done})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type fakeSSH struct {
	mu         sync.Mutex
	refusals   int    // connection attempts to refuse before sshd answers
	staleKeys  int    // then connections showing a host key not yet replaced
	denial     string // when set, every connection fails with it
	commands   []string
	files      map[string]string // the install bundle, by name
	modes      map[string]int64
	knownHosts string
	lastArgs   []string
}

func (s *fakeSSH) run(_ context.Context, args []string, stdin io.Reader) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	command := args[len(args)-1]
	s.commands = append(s.commands, args[len(args)-2]+" "+command)
	s.lastArgs = args
	for _, a := range args {
		if path, ok := strings.CutPrefix(a, "UserKnownHostsFile="); ok {
			data, _ := os.ReadFile(path)
			s.knownHosts = string(data)
		}
	}
	if s.denial != "" {
		return []byte(s.denial), &os.PathError{Op: "ssh", Err: os.ErrPermission}
	}
	if s.refusals > 0 {
		s.refusals--
		return []byte("ssh: connect to host 203.0.113.7 port 22: Connection refused"), &os.PathError{Op: "ssh", Err: os.ErrDeadlineExceeded}
	}
	if s.staleKeys > 0 {
		if command != "true" {
			return []byte("sent " + command + " to a host showing the wrong key"), &os.PathError{Op: "ssh", Err: os.ErrPermission}
		}
		s.staleKeys--
		return []byte("Host key verification failed."), &os.PathError{Op: "ssh", Err: os.ErrPermission}
	}
	switch command {
	case "true":
	case lambdaInstallCommand:
		s.files, s.modes = map[string]string{}, map[string]int64{}
		tr := tar.NewReader(stdin)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return []byte("tar: " + err.Error()), err
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return []byte("tar: " + err.Error()), err
			}
			s.files[h.Name], s.modes[h.Name] = string(data), h.Mode
		}
	default:
		return []byte("unexpected command"), errors.New("exit status 127")
	}
	return nil, nil
}

// fakeELF is the header of a Linux executable for machine, which is all the
// provider inspects.
func fakeELF(machine elf.Machine) []byte {
	h := make([]byte, 64)
	copy(h, "\x7fELF")
	h[elf.EI_CLASS], h[elf.EI_DATA], h[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(h[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(h[18:], uint16(machine))
	binary.LittleEndian.PutUint32(h[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(h[52:], 64) // header size
	return h
}

var errandBuilds = struct {
	sync.Mutex
	dir   string
	paths map[string]string
}{paths: map[string]string{}}

// fakeErrand builds a tiny program whose Go build info names errand's main
// package, for goos/goarch, once per test binary.
func fakeErrand(t *testing.T, goos, goarch string) string {
	t.Helper()
	errandBuilds.Lock()
	defer errandBuilds.Unlock()
	key := goos + "/" + goarch
	if path, ok := errandBuilds.paths[key]; ok {
		return path
	}
	if errandBuilds.dir == "" {
		dir, err := os.MkdirTemp("", "fake-errand")
		if err != nil {
			t.Fatal(err)
		}
		errandBuilds.dir = dir
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+errandMainPackage+"\n\ngo 1.21\n"), 0600)
		os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0600)
	}
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goTool); err != nil {
		goTool = "go"
	}
	path := filepath.Join(errandBuilds.dir, goos+"-"+goarch)
	cmd := exec.Command(goTool, "build", "-o", path, ".")
	cmd.Dir = errandBuilds.dir
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fake errand: %v\n%s", err, out)
	}
	errandBuilds.paths[key] = path
	return path
}

func copyFile(t *testing.T, from, to string) string {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0700); err != nil {
		t.Fatal(err)
	}
	return to
}

func noSave(json.RawMessage) error { return nil }

func newLambda(t *testing.T) (*LambdaProvider, *fakeLambda, *fakeSSH) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	api := &fakeLambda{capacity: []string{"us-west-1", "us-east-1"}, instances: map[string]*lambdaInstance{}, sshKeys: map[string]string{"laptop": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYG george@mac"}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	ssh := &fakeSSH{refusals: 2, staleKeys: 2}
	api.rateLimited = 2
	return &LambdaProvider{
		APIKeyFile:   write("lambda.key", "secret-key\n"),
		InstanceType: "gpu_1x_h100_pcie",
		Regions:      []string{"us-east-1"},
		KeyDir:       filepath.Join(dir, "lambda"),
		Keygen: func(context.Context, string, string) (string, error) {
			return "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE errand-cloud-peer", nil
		},
		TailscaleAuthKeyFile: write("ts.key", "tskey-auth-FAKE\n"),
		ErrandBinary:         copyFile(t, fakeErrand(t, "linux", "amd64"), filepath.Join(dir, "errand")),
		Arch:                 "amd64",
		AllowUsers:           []string{"broker@example", `odd%u${HOME}"'x`},
		BaseURL:              srv.URL,
		SSH:                  ssh.run,
		HostKey: func(context.Context) (string, string, error) {
			return "-----BEGIN OPENSSH PRIVATE KEY-----\nhost-key-body\n-----END OPENSSH PRIVATE KEY-----\n", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUF errand-lease", nil
		},
		Poll:       time.Millisecond,
		LaunchGap:  time.Millisecond,
		RequestGap: time.Millisecond,
		Pacing:     &lambdaPacing{},
	}, api, ssh
}

func TestLambdaAcquireAndRelease(t *testing.T) {
	p, api, ssh := newLambda(t)
	var progress []string
	var saved []string
	id := proto.NewULID()
	machine, err := p.Acquire(context.Background(), AcquireRequest{
		LeaseID: id, Offer: "h100", Where: "gpu=h100", Login: "george@github",
		Progress: func(s string) { progress = append(progress, s) },
		Save:     func(s json.RawMessage) error { saved = append(saved, string(s)); return nil },
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(progress, "\n"))
	}
	host := LeaseHostname(id)
	if machine.Target.URL != "http://"+host+":7443" {
		t.Fatalf("target %+v", machine.Target)
	}
	launch := api.launches[0]
	if launch["region_name"] != "us-east-1" || launch["name"] != host || launch["instance_type_name"] != "gpu_1x_h100_pcie" {
		t.Fatalf("launch %v", launch)
	}
	// The cloud peer's own key is added to the account once and named on
	// every launch; the account's other keys are left alone.
	keyName := "errand-" + keyID("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE")
	if names, _ := launch["ssh_key_names"].([]any); len(names) != 1 || names[0] != keyName || api.sshKeys[keyName] != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE errand-cloud-peer" || len(api.sshKeys) != 2 {
		t.Fatalf("ssh keys: launch %v, account %v", launch["ssh_key_names"], api.sshKeys)
	}
	if !slices.Contains(ssh.lastArgs, filepath.Join(p.KeyDir, "key", "lambda_ed25519")) {
		t.Fatalf("install did not use the cloud peer's key: %q", ssh.lastArgs)
	}
	// The instance gets the host key SSH then pins; the Tailscale key never
	// travels in launch metadata.
	userData, _ := launch["user_data"].(string)
	if !strings.Contains(userData, "    -----BEGIN OPENSSH PRIVATE KEY-----\n    host-key-body\n") || !strings.Contains(userData, "ed25519_public: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUF errand-lease") || strings.Contains(userData, "tskey") {
		t.Fatalf("user_data %q", userData)
	}
	if ssh.knownHosts != "203.0.113.7 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUF errand-lease\n" || !slices.Contains(ssh.lastArgs, "StrictHostKeyChecking=yes") {
		t.Fatalf("host key not pinned: %q %q", ssh.knownHosts, ssh.lastArgs)
	}
	// That a launch was sent is saved before sending it, the instance ID
	// before waiting, and that Lambda has shown it as soon as it has, so
	// release can always find the machine and tell when it is gone.
	if len(saved) < 3 || !strings.HasPrefix(saved[0], `{"sent":"`) || !strings.HasSuffix(saved[0], `","key_id":"`+keyID("secret-key")+`","region":"us-east-1"}`) || !strings.Contains(saved[1], `"instance_id":"inst-a"`) || strings.Contains(saved[1], "url") || strings.Contains(saved[1], "seen") || !strings.Contains(saved[2], `"seen":true`) {
		t.Fatalf("saved %q", saved)
	}
	for _, want := range []string{"launching gpu_1x_h100_pcie in us-east-1 ($2.49/h)", "instance booting", "instance active", "instance is up at 203.0.113.7; installing errand"} {
		if !strings.Contains(strings.Join(progress, "\n"), want) {
			t.Errorf("progress lacks %q: %q", want, progress)
		}
	}
	// Everything the install needs arrives as files, and the script that
	// uses them never changes.
	binary, _ := os.ReadFile(p.ErrandBinary)
	var runner struct {
		Transport  string   `toml:"transport"`
		Listen     string   `toml:"listen"`
		AllowUsers []string `toml:"allow_users"`
	}
	if _, err := toml.Decode(ssh.files["errandd.toml"], &runner); err != nil {
		t.Fatalf("errandd.toml: %v\n%s", err, ssh.files["errandd.toml"])
	}
	if want := []string{"broker@example", `odd%u${HOME}"'x`, "george@github"}; runner.Transport != "tailscale" || runner.Listen != "tailnet:7443" || !slices.Equal(runner.AllowUsers, want) {
		t.Errorf("runner config %+v, want allow_users %q", runner, want)
	}
	for name, want := range map[string]string{
		"install.sh":         lambdaInstallScript,
		"tailscale-auth-key": "tskey-auth-FAKE\n",
		"hostname":           host + "\n",
		"errand":             string(binary),
	} {
		if got, ok := ssh.files[name]; !ok || got != want {
			t.Errorf("bundle %s is %d bytes, want %d", name, len(got), len(want))
		}
	}
	if _, ok := ssh.files["authorized-keys"]; ok {
		t.Error("a tailnet machine got a client SSH key")
	}
	if ssh.modes["tailscale-auth-key"] != 0o600 || ssh.modes["errand"] != 0o755 {
		t.Errorf("bundle modes %v", ssh.modes)
	}
	for _, c := range ssh.commands {
		if strings.Contains(c, "tskey") {
			t.Fatalf("auth key on an ssh command line: %q", c)
		}
	}

	release := ReleaseRequest{LeaseID: id, Offer: "h100", State: machine.State}
	// Asked to terminate, then terminating, then listed as terminated.
	if calls := releaseUntilDone(t, p, release); calls != 3 {
		t.Fatalf("released after %d calls", calls)
	}
	if err := p.Release(context.Background(), release); err != nil {
		t.Fatalf("release once terminated: %v", err)
	}
	if len(api.terminated) != 1 || api.terminated[0] != "inst-a" {
		t.Fatalf("terminated %v", api.terminated)
	}
}

// Without a Tailscale key the machine is reached over SSH: the runner opens
// no port, the target pins the host key the machine booted with, and the
// clients' keys are installed for the login it runs as.
func TestLambdaAcquireOverSSH(t *testing.T) {
	p, _, ssh := newLambda(t)
	p.TailscaleAuthKeyFile = ""
	id := proto.NewULID()
	// A request without a tailnet login is fine: the client's key decides
	// who gets in.
	machine, err := p.Acquire(context.Background(), AcquireRequest{LeaseID: id, Offer: "h100", Where: "gpu", SSHKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB errand", Progress: func(string) {}, Save: noSave})
	if err != nil {
		t.Fatal(err)
	}
	if want := (proto.LeaseTarget{SSH: "ubuntu@203.0.113.7", HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUF errand-lease"}); machine.Target != want {
		t.Fatalf("target %+v", machine.Target)
	}
	if !strings.Contains(string(machine.State), `"ssh":"ubuntu@203.0.113.7"`) {
		t.Fatalf("state %s", machine.State)
	}
	var runner map[string]any
	if _, err := toml.Decode(ssh.files["errandd.toml"], &runner); err != nil || len(runner) != 1 || runner["transport"] != "ssh" {
		t.Fatalf("runner config %v %v", runner, err)
	}
	if _, ok := ssh.files["tailscale-auth-key"]; ok {
		t.Fatal("bundle carries an auth key")
	}
	if got := ssh.files["authorized-keys"]; got != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB errand\n" {
		t.Fatalf("authorized-keys %q", got)
	}
	// This cloud peer watches the machine with its own key.
	if machine.Identity != filepath.Join(p.KeyDir, "key", "lambda_ed25519") {
		t.Fatalf("identity %q", machine.Identity)
	}
}

// lambdaStateJSON is saved state for a launch sent at sent.
func lambdaStateJSON(sent time.Time, key, instanceID string) json.RawMessage {
	data, _ := json.Marshal(lambdaState{Sent: sent.UTC(), KeyID: keyID(key), Region: "us-east-1", InstanceID: instanceID})
	return data
}

// Release terminates the lease's instance whether or not its ID was saved,
// and only concludes there is none when that is proof.
func TestLambdaReleaseFindsUnsavedInstance(t *testing.T) {
	p, api, _ := newLambda(t)
	now := time.Now()
	p.Now = func() time.Time { return now }
	// Acquire that stopped before the launch request leaves nothing to find,
	// even when Lambda would refuse every call.
	os.WriteFile(p.APIKeyFile, []byte("wrong"), 0600)
	if err := p.Release(context.Background(), ReleaseRequest{LeaseID: proto.NewULID(), Offer: "h100"}); err != nil {
		t.Fatalf("never launched: %v", err)
	}
	os.WriteFile(p.APIKeyFile, []byte("secret-key"), 0600)
	release := func(state json.RawMessage) error {
		return p.Release(context.Background(), ReleaseRequest{LeaseID: proto.NewULID(), Offer: "h100", State: state})
	}
	// While the launch may still be registering, finding nothing is not
	// proof that nothing was launched, with or without a saved ID.
	for _, id := range []string{"", "inst-unlisted"} {
		if err := release(lambdaStateJSON(now.Add(-time.Minute), "secret-key", id)); err == nil || !strings.Contains(err.Error(), "checking again") {
			t.Fatalf("recent launch, id %q: %v", id, err)
		}
	}
	old := now.Add(-lambdaLaunchSettle - time.Second)
	if err := release(lambdaStateJSON(old, "secret-key", "")); err != nil {
		t.Fatalf("settled launch: %v", err)
	}
	// Under another API key, which may be another account, not finding the
	// instance proves nothing.
	if err := release(lambdaStateJSON(old, "other-key", "")); err == nil || !strings.Contains(err.Error(), "API key changed") {
		t.Fatalf("instance launched under another key: %v", err)
	}
	// State that does not say when the launch went out is not guessed at.
	if err := release(json.RawMessage(`{"key_id":"x"}`)); err == nil || !strings.Contains(err.Error(), "unreadable Lambda state") {
		t.Fatalf("state without a launch time: %v", err)
	}
	id := proto.NewULID()
	sent := lambdaStateJSON(now, "secret-key", "")
	api.instances["inst-z"] = &lambdaInstance{ID: "inst-z", Name: LeaseHostname(id), Status: "booting"}
	api.instances["inst-other"] = &lambdaInstance{ID: "inst-other", Name: "someone-else", Status: "active"}
	api.instances["inst-another"] = &lambdaInstance{ID: "inst-another", Name: "someone-else-too", Status: "active"}
	api.pageSize = 1 // the lease's instance is on the last page
	// A termination Lambda accepts but does not carry out is not a
	// release: it is asked again until Lambda lists the instance as
	// terminated.
	api.keepAlive = 1
	if calls := releaseUntilDone(t, p, ReleaseRequest{LeaseID: id, Offer: "h100", State: sent}); calls != 4 {
		t.Fatalf("released after %d calls", calls)
	}
	if len(api.terminated) != 1 || api.terminated[0] != "inst-z" {
		t.Fatalf("terminated %v", api.terminated)
	}
	// Once the instance is listed as ended, release is done at once.
	if err := p.Release(context.Background(), ReleaseRequest{LeaseID: id, Offer: "h100", State: sent}); err != nil {
		t.Fatalf("ended instance: %v", err)
	}
}

// An instance Lambda has shown is gone once Lambda stops listing it, even
// within the launch's settle window, and that holds after a restart, since
// the lease's state says so. One never seen may still be registering.
func TestLambdaReleaseOfUnlistedSeenInstance(t *testing.T) {
	p, api, _ := newLambda(t)
	now := time.Now()
	p.Now = func() time.Time { return now }
	api.forget = true
	id := proto.NewULID()
	api.instances["inst-z"] = &lambdaInstance{ID: "inst-z", Name: LeaseHostname(id), Status: "active"}
	var state lambdaState
	json.Unmarshal(lambdaStateJSON(now.Add(-time.Minute), "secret-key", "inst-z"), &state)
	state.Seen = true
	seen, _ := json.Marshal(state)
	req := ReleaseRequest{LeaseID: id, Offer: "h100", State: seen}
	// Asked to terminate, then terminating, then no longer listed.
	if calls := releaseUntilDone(t, p, req); calls != 3 {
		t.Fatalf("released after %d calls", calls)
	}
	if len(api.terminated) != 1 || len(api.instances) != 0 {
		t.Fatalf("terminated %v, still listed %v", api.terminated, api.instances)
	}
	// A restarted cloud peer has only the lease's state.
	restarted := &LambdaProvider{APIKeyFile: p.APIKeyFile, BaseURL: p.BaseURL, Now: p.Now, RequestGap: time.Millisecond, Pacing: &lambdaPacing{}}
	if err := restarted.Release(context.Background(), req); err != nil {
		t.Fatalf("release after a restart: %v", err)
	}
	// To another key, perhaps another account's, absence proves nothing.
	json.Unmarshal(lambdaStateJSON(now.Add(-time.Minute), "another-key", "inst-z"), &state)
	state.Seen = true
	req.State, _ = json.Marshal(state)
	if err := restarted.Release(context.Background(), req); err == nil || !strings.Contains(err.Error(), "key changed") {
		t.Fatalf("release of an instance seen under another key: %v", err)
	}
	// Without Seen, not finding the instance proves nothing yet.
	req.State = lambdaStateJSON(now.Add(-time.Minute), "secret-key", "inst-z")
	if err := restarted.Release(context.Background(), req); err == nil || !strings.Contains(err.Error(), "checking again") {
		t.Fatalf("release of an instance never seen: %v", err)
	}
}

// The install sends the very errand_binary acquire checked, even when the
// file is rewritten after the check, and leaves no copy of it behind.
func TestLambdaInstallsTheCheckedBinary(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, tmp)
	}
	p, _, ssh := newLambda(t)
	want, err := os.ReadFile(p.ErrandBinary)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := false
	p.SSH = func(ctx context.Context, args []string, stdin io.Reader) ([]byte, error) {
		if !rewritten {
			rewritten = true
			if err := os.WriteFile(p.ErrandBinary, []byte("#!/bin/sh\necho unchecked\n"), 0o700); err != nil {
				t.Error(err)
			}
		}
		return ssh.run(ctx, args, stdin)
	}
	req := AcquireRequest{LeaseID: proto.NewULID(), Login: "george@github", Progress: func(string) {}, Save: noSave}
	if _, err := p.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !rewritten || ssh.files["errand"] != string(want) {
		t.Fatalf("sent %d bytes of errand, want the %d checked", len(ssh.files["errand"]), len(want))
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("acquire left %v in the temporary directory", left)
	}
}

func TestLambdaRefusals(t *testing.T) {
	p, api, _ := newLambda(t)
	api.capacity = []string{"us-west-1"}
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: noSave}
	if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no gpu_1x_h100_pcie capacity in us-east-1") {
		t.Fatalf("capacity: %v", err)
	}
	if len(api.launches) != 0 {
		t.Fatal("launched without capacity")
	}
	// A key the instance rejects fails at once instead of at the timeout.
	p2, _, _ := newLambda(t)
	p2.SSH = (&fakeSSH{denial: "ubuntu@203.0.113.7: Permission denied (publickey)."}).run
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p2.Acquire(ctx, req); err == nil || !strings.Contains(err.Error(), "Permission denied") || ctx.Err() != nil {
		t.Fatalf("ssh denial: %v", err)
	}
	// An offer whose arch disagrees with the instance type launches nothing.
	pa, apiA, _ := newLambda(t)
	pa.Arch = "arm64"
	pa.ErrandBinary = fakeErrand(t, "linux", "arm64")
	if _, err := pa.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), `is x86_64 but the offer says arch = "arm64"`) || len(apiA.launches) != 0 {
		t.Fatalf("arch mismatch: %v, %d launches", err, len(apiA.launches))
	}
	// A price above the cap, as Lambda lists it right before launching,
	// launches nothing, even when the offer was listed cheaper.
	pc, apiC, _ := newLambda(t)
	pc.MaxPricePerHour = 2
	if _, err := pc.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "Lambda now charges $2.49/h for gpu_1x_h100_pcie, above max_price_per_hour = 2") || len(apiC.launches) != 0 {
		t.Fatalf("price above the cap: %v, %d launches", err, len(apiC.launches))
	}
	// So does an instance type whose architecture errand does not know.
	pu, apiU, _ := newLambda(t)
	apiU.arch = "riscv64"
	if _, err := pu.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), `has architecture "riscv64"`) || len(apiU.launches) != 0 {
		t.Fatalf("unknown arch: %v, %d launches", err, len(apiU.launches))
	}
	// A missing or wrong install input fails before anything is launched.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "arm"), fakeELF(elf.EM_AARCH64), 0700)
	os.WriteFile(filepath.Join(dir, "script"), []byte("#!/bin/sh\n"), 0700)
	os.WriteFile(filepath.Join(dir, "other"), fakeELF(elf.EM_X86_64), 0700)
	for binary, want := range map[string]string{
		filepath.Join(dir, "missing"):     "errand_binary",
		filepath.Join(dir, "arm"):         "not a linux/amd64 executable",
		filepath.Join(dir, "script"):      "not a Linux executable",
		filepath.Join(dir, "other"):       "not an errand build",
		fakeErrand(t, "freebsd", "amd64"): "errand for freebsd, not linux",
		dir:                               "not a regular file",
	} {
		p3, api3, _ := newLambda(t)
		p3.ErrandBinary = binary
		if _, err := p3.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), want) || len(api3.launches) != 0 {
			t.Errorf("%s: %v, %d launches", binary, err, len(api3.launches))
		}
	}
	// Over SSH, a request without the client's key would rent a machine
	// that admits no one.
	pk, apiK, _ := newLambda(t)
	pk.TailscaleAuthKeyFile = ""
	if _, err := pk.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no SSH key") || len(apiK.launches) != 0 {
		t.Errorf("no client key: %v, %d launches", err, len(apiK.launches))
	}
	// With no caller login and no allow_users, the machine would admit no one.
	pl, apiL, _ := newLambda(t)
	for _, users := range [][]string{nil, {""}, {" ", "\t\x01"}} {
		pl.AllowUsers = users
		if _, err := pl.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "no tailnet login") || len(apiL.launches) != 0 {
			t.Errorf("no login, allow_users %q: %v, %d launches", users, err, len(apiL.launches))
		}
	}
	withLogin := req
	withLogin.Login = "george@github"
	if _, err := pl.Acquire(context.Background(), withLogin); err != nil {
		t.Errorf("caller login alone: %v", err)
	}
	os.WriteFile(p.APIKeyFile, []byte("wrong"), 0600)
	if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "API key was invalid") {
		t.Fatalf("bad key: %v", err)
	}
}

// The cloud peer makes its own key on first use and adds it to the Lambda
// account once; a key the account already has is used under its name.
func TestLambdaRegistersOwnKey(t *testing.T) {
	p, api, _ := newLambda(t)
	p.Keygen = nil
	ctx := context.Background()
	name, err := p.registerKey(ctx, "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(filepath.Join(p.KeyDir, "key", "lambda_ed25519.pub"))
	if err != nil || !strings.HasPrefix(name, "errand-") || api.sshKeys[name] != strings.TrimSpace(string(public)) {
		t.Fatalf("registered %q: %v, account %v", name, err, api.sshKeys)
	}
	if again, err := p.registerKey(ctx, "secret-key"); err != nil || again != name || api.keysAdded != 1 {
		t.Fatalf("second call: %q %v, %d keys added", again, err, api.keysAdded)
	}
	// Someone added the same key by hand under another name.
	delete(api.sshKeys, name)
	api.sshKeys["mine"] = strings.TrimSpace(string(public)) + " edited comment"
	if got, err := p.registerKey(ctx, "secret-key"); err != nil || got != "mine" || api.keysAdded != 1 {
		t.Fatalf("existing key: %q %v, %d keys added", got, err, api.keysAdded)
	}
}

// With file systems, only their region is considered, since Lambda attaches a
// file system only to instances in its own region.
func TestLambdaFileSystemRegion(t *testing.T) {
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: noSave}
	p, api, _ := newLambda(t)
	p.Regions = nil
	p.FileSystems = []string{"datasets"}
	api.fileSystems = map[string]string{"datasets": "us-east-1"}
	if _, err := p.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := api.launches[0]["region_name"]; got != "us-east-1" {
		t.Fatalf("launched in %v, want the file system's us-east-1", got)
	}
	for _, c := range []struct {
		regions []string
		fs      map[string]string
		names   []string
		want    string
	}{
		{nil, map[string]string{"datasets": "us-south-1"}, []string{"datasets"}, "no gpu_1x_h100_pcie capacity in us-south-1"},
		{[]string{"us-west-1"}, map[string]string{"datasets": "us-east-1"}, []string{"datasets"}, "file systems are in us-east-1, which regions does not list"},
		{nil, map[string]string{"a": "us-east-1", "b": "us-west-1"}, []string{"a", "b"}, "different regions"},
		{nil, map[string]string{}, []string{"datasets"}, `no file system "datasets"`},
	} {
		p, api, _ := newLambda(t)
		p.Regions, p.FileSystems, api.fileSystems = c.regions, c.names, c.fs
		if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), c.want) || len(api.launches) != 0 {
			t.Errorf("%v: %v, %d launches", c.fs, err, len(api.launches))
		}
	}
}

// A launch Lambda refuses leaves nothing for release to look for, while a
// failed status poll of a booting machine is retried.
func TestLambdaLaunchRefusalAndPollErrors(t *testing.T) {
	p, api, _ := newLambda(t)
	api.launchError = http.StatusBadRequest
	var saved []json.RawMessage
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: func(s json.RawMessage) error { saved = append(saved, s); return nil }}
	if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "Not enough capacity") {
		t.Fatalf("refused launch: %v", err)
	}
	if len(saved) == 0 || len(saved[len(saved)-1]) != 0 {
		t.Fatalf("a refused launch must leave no state, saved %q", saved)
	}
	if err := p.Release(context.Background(), ReleaseRequest{LeaseID: req.LeaseID, State: saved[len(saved)-1]}); err != nil {
		t.Fatalf("release after refusal: %v", err)
	}
	p2, api2, _ := newLambda(t)
	api2.pollErrors = 2
	if _, err := p2.Acquire(context.Background(), AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: noSave}); err != nil {
		t.Fatalf("transient poll errors: %v", err)
	}
}

// A launch whose record cannot be saved is never sent: a restarted cloud
// peer would not know to look for the machine.
func TestLambdaUnsavedLaunchIsNotSent(t *testing.T) {
	p, api, _ := newLambda(t)
	var held json.RawMessage // what the broker keeps in memory
	save := func(s json.RawMessage) error { held = s; return errors.New("disk full") }
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: save}
	if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "disk full") || len(api.launches) != 0 {
		t.Fatalf("unsaved launch: %v, %d launches", err, len(api.launches))
	}
	if len(held) != 0 {
		t.Fatalf("an unsent launch left state %s for release to chase", held)
	}
}

// A deadline that passes while the launch record is being saved stops the
// launch, and the record is taken back.
func TestLambdaLaunchDeadlineWhileSaving(t *testing.T) {
	p, api, _ := newLambda(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var held json.RawMessage
	save := func(s json.RawMessage) error {
		if len(s) > 0 {
			<-ctx.Done() // a slow disk
		}
		held = s
		return nil
	}
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: save}
	if _, err := p.Acquire(ctx, req); !errors.Is(err, context.DeadlineExceeded) || len(api.launches) != 0 {
		t.Fatalf("deadline while saving: %v, %d launches", err, len(api.launches))
	}
	if len(held) != 0 {
		t.Fatalf("an unsent launch left state %s for release to chase", held)
	}
}

// A launch canceled while waiting its turn was never sent, so it records
// nothing that release would have to look for.
func TestLambdaLaunchCanceledWhileWaiting(t *testing.T) {
	p, api, _ := newLambda(t)
	p.LaunchGap = time.Hour
	key := "waiting-key"
	os.WriteFile(p.APIKeyFile, []byte(key), 0600)
	api.apiKey = key
	p.Pacing.launch.last = time.Now() // another launch just went out
	var saved []json.RawMessage
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: func(s json.RawMessage) error { saved = append(saved, s); return nil }}
	if _, err := p.Acquire(ctx, req); !errors.Is(err, context.DeadlineExceeded) || len(api.launches) != 0 || slices.ContainsFunc(saved, func(s json.RawMessage) bool { return len(s) > 0 }) {
		t.Fatalf("canceled wait: %v, %d launches, saved %q", err, len(api.launches), saved)
	}
}

// Every launch attempt keeps to the launch gap, and a launch that Lambda
// rate-limited until the deadline counts as refused, leaving no state.
//
// The deadline must land while acquire waits between attempts: one that
// lands while a launch request is in flight leaves it uncertain, and its
// state rightly stays saved. So rather than a wall-clock timeout, the
// context ends as acquire saves the third attempt, before it is sent.
func TestLambdaLaunchRateLimitedUntilDeadline(t *testing.T) {
	p, api, _ := newLambda(t)
	key := "limited-key"
	os.WriteFile(p.APIKeyFile, []byte(key), 0600)
	api.apiKey, api.launchError = key, http.StatusTooManyRequests
	p.LaunchGap = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var saved []json.RawMessage
	var sending []time.Time // when acquire readied each attempt
	save := func(s json.RawMessage) error {
		saved = append(saved, s)
		if len(s) > 0 {
			if sending = append(sending, time.Now()); len(sending) == 3 {
				cancel()
			}
		}
		return nil
	}
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: save}
	_, err := p.Acquire(ctx, req)
	var api429 *lambdaAPIError
	if !errors.As(err, &api429) || api429.Status != http.StatusTooManyRequests {
		t.Fatalf("want the last 429, got %v", err)
	}
	if len(saved) == 0 || len(saved[len(saved)-1]) != 0 {
		t.Fatalf("a launch only ever refused must leave no state, saved %q", saved)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	// The third attempt was readied but never sent.
	if n := len(api.attempts); n != 2 || len(sending) != 3 {
		t.Fatalf("%d launch attempts sent of %d readied, want 2 of 3", n, len(sending))
	}
	// Each attempt is readied only once the launch gap has passed since the
	// previous one went out, which was after it was readied. Retrying at
	// the general request pace would ready them closer together.
	for i := 1; i < len(sending); i++ {
		if gap := sending[i].Sub(sending[i-1]); gap < p.LaunchGap {
			t.Fatalf("attempt %d readied %v after the one before, want at least %v", i+1, gap, p.LaunchGap)
		}
	}
}

// pacedLambda is a provider whose requests Lambda answers at once, logging
// when each arrived.
type pacedLambda struct {
	mu                 sync.Mutex
	requests, launches []time.Time
	launchOrder        []string
}

func (l *pacedLambda) RoundTrip(r *http.Request) (*http.Response, error) {
	var body struct {
		Name string `json:"name"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	l.mu.Lock()
	now := time.Now()
	l.requests = append(l.requests, now)
	if strings.HasSuffix(r.URL.Path, "/launch") {
		l.launches = append(l.launches, now)
		l.launchOrder = append(l.launchOrder, body.Name)
	}
	l.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{}}`)), Request: r}, nil
}

func newPacedLambda(requestGap, launchGap time.Duration) (*LambdaProvider, *pacedLambda) {
	log := &pacedLambda{}
	return &LambdaProvider{BaseURL: "http://lambda.invalid", HTTP: &http.Client{Transport: log}, RequestGap: requestGap, LaunchGap: launchGap, Pacing: &lambdaPacing{}}, log
}

func sendLaunch(ctx context.Context, p *LambdaProvider, name string) error {
	return p.launch(ctx, "k", map[string]any{"name": name}, nil, func() error { return nil })
}

// Launches go through launchGap apart, and all requests requestGap apart,
// however they interleave. The times are the ones each gate records as it
// lets a caller through: a request may reach Lambda any time later, so
// arrival times would bound nothing.
func TestLambdaPacingKeepsGaps(t *testing.T) {
	const requestGap, launchGap = 20 * time.Millisecond, 120 * time.Millisecond
	p, log := newPacedLambda(requestGap, launchGap)
	var mu sync.Mutex
	var requests, launches []time.Time
	p.Pacing.passed = func(launch bool, at time.Time) {
		mu.Lock()
		defer mu.Unlock()
		if launch {
			launches = append(launches, at)
		} else {
			requests = append(requests, at)
		}
	}
	var wg sync.WaitGroup
	for i := range 9 {
		wg.Go(func() {
			var err error
			if i%3 == 0 {
				err = sendLaunch(context.Background(), p, strconv.Itoa(i))
			} else {
				err = p.call(context.Background(), "k", http.MethodGet, "/instances", nil, nil)
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	check := func(what string, times []time.Time, gap time.Duration) {
		for i := 1; i < len(times); i++ {
			if d := times[i].Sub(times[i-1]); d < gap {
				t.Errorf("%s %d went through %v after the one before, want at least %v", what, i, d, gap)
			}
		}
	}
	// Each gate records its callers in turn, so the times are in order.
	check("request", requests, requestGap)
	check("launch", launches, launchGap)
	if len(requests) != 9 || len(launches) != 3 || len(log.requests) != 9 || len(log.launches) != 3 {
		t.Fatalf("%d requests and %d launches went through, %d and %d were sent", len(requests), len(launches), len(log.requests), len(log.launches))
	}
}

// An ordinary request passes a launch still waiting out the launch gap, but
// launches keep their order.
func TestLambdaRequestsPassWaitingLaunch(t *testing.T) {
	p, log := newPacedLambda(time.Millisecond, 200*time.Millisecond)
	sendLaunch(context.Background(), p, "1") // a launch goes out
	var wg sync.WaitGroup
	wg.Go(func() { sendLaunch(context.Background(), p, "2") })
	time.Sleep(20 * time.Millisecond) // launch 2 is waiting first
	wg.Go(func() { sendLaunch(context.Background(), p, "3") })
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	if err := p.call(context.Background(), "k", http.MethodGet, "/instances", nil, nil); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > 100*time.Millisecond {
		t.Fatalf("a poll waited %v behind launches", waited)
	}
	wg.Wait()
	if !slices.Equal(log.launchOrder, []string{"1", "2", "3"}) {
		t.Fatalf("launch order %q", log.launchOrder)
	}
}

// Launches that give up while waiting leave no trace, in whatever order they
// give up.
func TestLambdaCanceledLaunchLeavesNoTrace(t *testing.T) {
	p, log := newPacedLambda(time.Millisecond, time.Hour)
	sendLaunch(context.Background(), p, "1") // a launch goes out
	sent := p.Pacing.launch.last
	for range 3 {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for range 2 { // one waiting out the gap, one waiting for its turn
			wg.Go(func() {
				if err := sendLaunch(ctx, p, "canceled"); !errors.Is(err, errNotSent) {
					t.Errorf("canceled launch: %v", err)
				}
			})
		}
		time.Sleep(10 * time.Millisecond)
		cancel()
		wg.Wait()
	}
	if !p.Pacing.launch.last.Equal(sent) || len(log.launches) != 1 {
		t.Fatalf("canceled launches moved the schedule by %v, %d launches", p.Pacing.launch.last.Sub(sent), len(log.launches))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Pacing.launch.take(ctx); err != nil {
		t.Fatalf("a canceled launch kept its turn: %v", err)
	}
}

// Offers share one schedule whatever their API keys, since keys alone do not
// say which account they belong to.
func TestLambdaOffersSharePacing(t *testing.T) {
	a := &LambdaProvider{APIKeyFile: "one"}
	b := &LambdaProvider{APIKeyFile: "two"}
	if a.pacing() != b.pacing() {
		t.Fatal("offers with different API keys pace separately")
	}
}

// Credentials other users could read are refused.
func TestLambdaSecretPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows files use ACLs, not mode bits")
	}
	p, api, _ := newLambda(t)
	os.Chmod(p.TailscaleAuthKeyFile, 0644)
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: noSave}
	if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "readable by other users") || len(api.launches) != 0 {
		t.Fatalf("world-readable auth key: %v, %d launches", err, len(api.launches))
	}
}

// The broker drives a Lambda offer through its whole life.
func TestBrokerWithLambdaProvider(t *testing.T) {
	p, api, _ := newLambda(t)
	machine := &fakeMachine{gpus: []proto.GPU{{Name: "NVIDIA H100 PCIe", MemoryMiB: 81559}}}
	b, err := New(Config{
		StateDir: t.TempDir(),
		Offers: []Offer{{
			Name: "h100", Provider: p, PricePerHour: new(2.49),
			Facts:       proto.Facts{OS: "linux", Arch: "amd64", GPUs: []proto.GPU{{Name: "H100", MemoryMiB: 80 << 10}}},
			IdleTimeout: time.Hour, MaxLifetime: time.Hour,
		}},
		Probe: func(ctx context.Context, target proto.LeaseTarget, identity, where string) (proto.Info, error) {
			return machine.probe(ctx, proto.LeaseTarget{URL: "http://box:7443"}, identity, where)
		},
		AdmitKeys: machine.admit,
		ReadyPoll: time.Millisecond,
		IdlePoll:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if offers := b.Offers(context.Background()); offers[0].PricePerHour == nil || *offers[0].PricePerHour != 2.49 {
		t.Fatalf("offers %+v", offers)
	}
	l, err := b.Acquire("george", "george@github", "gpu=h100", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ready := waitState(t, b, "george", l.ID, proto.LeaseReady)
	if ready.Target.URL != "http://"+LeaseHostname(l.ID)+":7443" {
		t.Fatalf("target %+v", ready.Target)
	}
	b.Release("george", l.ID)
	waitState(t, b, "george", l.ID, proto.LeaseReleased)
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.terminated) != 1 {
		t.Fatalf("terminated %v", api.terminated)
	}
}

func TestLambdaHostKeyGenerator(t *testing.T) {
	private, public, err := (&LambdaProvider{}).hostKey()(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(private, "-----BEGIN OPENSSH PRIVATE KEY-----") || !strings.HasPrefix(public, "ssh-ed25519 ") || strings.Contains(public, "\n") {
		t.Fatalf("keys %q %q", private, public)
	}
	config := hostKeyCloudConfig(private, public)
	if !strings.HasPrefix(config, "#cloud-config\n") || strings.Count(config, "\n    ") != strings.Count(strings.TrimSpace(private), "\n")+1 {
		t.Fatalf("cloud-config:\n%s", config)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if errandBuilds.dir != "" {
		os.RemoveAll(errandBuilds.dir)
	}
	os.Exit(code)
}

// The remote command unpacks the bundle with the real tar into a private
// directory and hands the script, which must parse, to sudo.
func TestLambdaInstallCommandUnpacksBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the remote side is Linux")
	}
	for _, tool := range []string{"sh", "bash", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	home, bin := t.TempDir(), t.TempDir()
	sudo := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOME/sudo-args\"\ncp -Rp \"$HOME/.errand-lease\" \"$HOME/seen\"\nexec bash -n \"$2\"\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	p, _, _ := newLambda(t)
	config, err := lambdaRunnerConfig(true, []string{"george@github"})
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Open(p.ErrandBinary)
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	r, w := io.Pipe()
	go func() {
		w.CloseWithError(writeInstallBundle(w, binary, config, "errand-lease-host", "tskey-auth-FAKE", ""))
	}()
	cmd := exec.Command("sh", "-c", lambdaInstallCommand)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = r
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install command: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".errand-lease")); !os.IsNotExist(err) {
		t.Errorf("the bundle directory is still there: %v", err)
	}
	args, _ := os.ReadFile(filepath.Join(home, "sudo-args"))
	if want := "bash\n" + home + "/.errand-lease/install.sh\n"; string(args) != want {
		t.Fatalf("sudo ran %q, want %q", args, want)
	}
	seen := filepath.Join(home, "seen")
	for name, mode := range map[string]os.FileMode{".": 0o700, "tailscale-auth-key": 0o600, "errand": 0o755} {
		if info, err := os.Stat(filepath.Join(seen, name)); err != nil {
			t.Error(err)
		} else if info.Mode().Perm() != mode {
			t.Errorf("%s has mode %v, want %v", name, info.Mode().Perm(), mode)
		}
	}
	if key, _ := os.ReadFile(filepath.Join(seen, "tailscale-auth-key")); string(key) != "tskey-auth-FAKE\n" {
		t.Errorf("auth key file %q", key)
	}

	// When sudo cannot run the script, the directory, auth key and all,
	// still goes.
	os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\necho 'sudo: a password is required' >&2\nexit 1\n"), 0o755)
	r, w = io.Pipe()
	go func() {
		w.CloseWithError(writeInstallBundle(w, binary, config, "errand-lease-host", "tskey-auth-FAKE", ""))
	}()
	cmd = exec.Command("sh", "-c", lambdaInstallCommand)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = r
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "password is required") {
		t.Fatalf("failing sudo: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".errand-lease")); !os.IsNotExist(err) {
		t.Errorf("failing sudo left the bundle directory: %v", err)
	}
}

// A credential path that is a FIFO is refused without opening it, which
// would block until something wrote to it.
func TestLambdaSecretFIFO(t *testing.T) {
	if _, err := exec.LookPath("mkfifo"); err != nil || runtime.GOOS == "windows" {
		t.Skip("no mkfifo")
	}
	path := filepath.Join(t.TempDir(), "key")
	if out, err := exec.Command("mkfifo", "-m", "600", path).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v %s", err, out)
	}
	done := make(chan error, 1)
	go func() { _, err := readSecret(path, "Lambda API key"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO credential blocked")
	}
}

// A launch whose saved time has gone stale while saving or waiting is
// saved again before it goes out, so the time release counts from is never
// much older than the request.
func TestLambdaSlowSaveIsRedone(t *testing.T) {
	p, api, _ := newLambda(t)
	p.SendSlack = 50 * time.Millisecond
	var sents []time.Time
	slow := true
	save := func(s json.RawMessage) error {
		var st lambdaState
		if json.Unmarshal(s, &st) == nil && !st.Sent.IsZero() && st.InstanceID == "" {
			sents = append(sents, st.Sent)
			if slow {
				slow = false
				time.Sleep(150 * time.Millisecond) // a stalled disk
			}
		}
		return nil
	}
	req := AcquireRequest{LeaseID: proto.NewULID(), Progress: func(string) {}, Save: save}
	if _, err := p.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(sents) != 2 || !sents[1].After(sents[0]) || len(api.launches) != 1 {
		t.Fatalf("saved launch times %v, %d launches", sents, len(api.launches))
	}
}

// releaseUntilDone calls Release as the broker would until it reports the
// machine gone, and returns how many calls that took.
func releaseUntilDone(t *testing.T, p *LambdaProvider, req ReleaseRequest) int {
	t.Helper()
	for calls := 1; calls <= 10; calls++ {
		err := p.Release(context.Background(), req)
		if err == nil {
			return calls
		}
		var pending *ReleasePending
		if !errors.As(err, &pending) {
			t.Fatalf("release call %d: %v", calls, err)
		}
	}
	t.Fatal("release never finished")
	return 0
}
