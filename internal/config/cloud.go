package config

import (
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/lydakis/errand/internal/cloud"
	"github.com/lydakis/errand/internal/proto"
)

// DaemonCloud turns a runner into a cloud peer that leases machines from
// the configured offers. See docs/CLOUD.md.
type DaemonCloud struct {
	MaxLeases      int          `toml:"max_leases"`
	AcquireTimeout string       `toml:"acquire_timeout"`
	Offers         []CloudOffer `toml:"offers"`
}

type CloudOffer struct {
	Name        string       `toml:"name"`
	OS          string       `toml:"os"`
	Arch        string       `toml:"arch"`
	CPUs        int          `toml:"cpus"`
	Tools       []string     `toml:"tools"`
	GPU         string       `toml:"gpu"`            // model name as the driver reports it, e.g. "H100 80GB"
	GPUs        int          `toml:"gpus"`           // defaults to 1 when gpu or vram is set
	VRAM        int          `toml:"vram"`           // GiB per GPU
	Price       float64      `toml:"price_per_hour"` // USD, shown to callers
	Acquire     []string     `toml:"acquire"`
	Release     []string     `toml:"release"`
	Lambda      *LambdaOffer `toml:"lambda"`
	IdleTimeout string       `toml:"idle_timeout"` // default 20m
	MaxLifetime string       `toml:"max_lifetime"` // default 12h
}

// LambdaOffer rents Lambda Cloud instances instead of running commands.
type LambdaOffer struct {
	InstanceType         string   `toml:"instance_type"`
	Regions              []string `toml:"regions"`
	FileSystems          []string `toml:"file_systems"`
	APIKeyFile           string   `toml:"api_key_file"`
	SSHKeyName           string   `toml:"ssh_key_name"`
	SSHPrivateKeyFile    string   `toml:"ssh_private_key_file"`
	User                 string   `toml:"user"`
	TailscaleAuthKeyFile string   `toml:"tailscale_auth_key_file"`
	ErrandBinary         string   `toml:"errand_binary"`
	AllowUsers           []string `toml:"allow_users"`
	AuthorizedKeys       []string `toml:"authorized_keys"`
}

const (
	defaultLeaseIdle     = 20 * time.Minute
	defaultLeaseLifetime = 12 * time.Hour
)

// Broker validates the cloud section. It returns nil when no offers are
// configured, so an ordinary runner stays an ordinary runner.
func (c DaemonCloud) Broker() (*cloud.Config, error) {
	if len(c.Offers) == 0 {
		if c.MaxLeases != 0 || c.AcquireTimeout != "" {
			return nil, fmt.Errorf("cloud: settings need at least one [[cloud.offers]] entry")
		}
		return nil, nil
	}
	out := &cloud.Config{MaxLeases: c.MaxLeases}
	if c.MaxLeases < 0 {
		return nil, fmt.Errorf("cloud: max_leases must not be negative")
	}
	var err error
	if out.AcquireTimeout, err = positiveDuration("cloud acquire_timeout", c.AcquireTimeout, 15*time.Minute); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, o := range c.Offers {
		where := fmt.Sprintf("cloud offer %d", i+1)
		if err := validateOfferName(o.Name); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		where = fmt.Sprintf("cloud offer %q", o.Name)
		if seen[o.Name] {
			return nil, fmt.Errorf("%s is defined twice", where)
		}
		seen[o.Name] = true
		if o.OS == "" && o.Lambda != nil {
			o.OS = "linux"
		}
		if o.Arch == "" && o.Lambda != nil {
			o.Arch = "amd64"
		}
		if o.OS != "" && o.OS != "linux" && o.OS != "darwin" && o.OS != "windows" {
			return nil, fmt.Errorf("%s: os must be linux, darwin or windows", where)
		}
		if o.Arch != "" && o.Arch != "amd64" && o.Arch != "arm64" {
			return nil, fmt.Errorf("%s: arch must be amd64 or arm64", where)
		}
		if o.Price < 0 || math.IsNaN(o.Price) || math.IsInf(o.Price, 0) {
			return nil, fmt.Errorf("%s: price_per_hour must not be negative", where)
		}
		var provider cloud.Provider
		switch {
		case o.Lambda != nil && (len(o.Acquire) > 0 || len(o.Release) > 0):
			return nil, fmt.Errorf("%s: use either acquire and release commands or [cloud.offers.lambda], not both", where)
		case o.Lambda != nil:
			if o.OS != "linux" {
				return nil, fmt.Errorf("%s: Lambda offers run linux", where)
			}
			if provider, err = o.Lambda.provider(o.Arch); err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
		default:
			for _, argv := range [][]string{o.Acquire, o.Release} {
				if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
					return nil, fmt.Errorf("%s: acquire and release must be commands with an absolute executable path", where)
				}
			}
			provider = cloud.CommandProvider{AcquireCommand: o.Acquire, ReleaseCommand: o.Release}
		}
		if o.CPUs < 0 || o.GPUs < 0 || o.VRAM < 0 {
			return nil, fmt.Errorf("%s: cpus, gpus and vram must not be negative", where)
		}
		facts := proto.Facts{OS: o.OS, Arch: o.Arch, NumCPU: o.CPUs}
		if len(o.Tools) > 0 {
			facts.Tools = map[string]string{}
			for _, tool := range o.Tools {
				facts.Tools[tool] = "declared by offer"
			}
		}
		gpus := o.GPUs
		if gpus == 0 && (o.GPU != "" || o.VRAM > 0) {
			gpus = 1
		}
		if gpus > 0 && o.GPU == "" {
			return nil, fmt.Errorf("%s: gpus and vram need a gpu model name", where)
		}
		for range gpus {
			facts.GPUs = append(facts.GPUs, proto.GPU{Name: o.GPU, MemoryMiB: o.VRAM << 10})
		}
		offer := cloud.Offer{Name: o.Name, Facts: facts, Provider: provider, PricePerHour: o.Price}
		if offer.IdleTimeout, err = positiveDuration(where+" idle_timeout", o.IdleTimeout, defaultLeaseIdle); err != nil {
			return nil, err
		}
		if offer.MaxLifetime, err = positiveDuration(where+" max_lifetime", o.MaxLifetime, defaultLeaseLifetime); err != nil {
			return nil, err
		}
		out.Offers = append(out.Offers, offer)
	}
	return out, nil
}

func positiveDuration(name, value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 20m", name)
	}
	return d, nil
}

// Offer names become part of lease peer names, so they stay short and plain.
func validateOfferName(name string) error {
	if name == "" || len(name) > 32 {
		return fmt.Errorf("name must be 1 to 32 characters")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("name %q may contain only lowercase letters, digits and '-'", name)
		}
	}
	return nil
}

func (l LambdaOffer) provider(arch string) (*cloud.LambdaProvider, error) {
	if l.InstanceType == "" || l.SSHKeyName == "" {
		return nil, fmt.Errorf("lambda needs instance_type and ssh_key_name")
	}
	for name, path := range map[string]string{"api_key_file": l.APIKeyFile, "ssh_private_key_file": l.SSHPrivateKeyFile, "tailscale_auth_key_file": l.TailscaleAuthKeyFile} {
		if !filepath.IsAbs(path) && (path != "" || name != "tailscale_auth_key_file") {
			return nil, fmt.Errorf("lambda %s must be an absolute path", name)
		}
	}
	// Without Tailscale the machine admits SSH keys, not tailnet logins.
	if l.TailscaleAuthKeyFile == "" && len(l.AllowUsers) > 0 {
		return nil, fmt.Errorf("lambda allow_users names tailnet logins and needs tailscale_auth_key_file; without it, list SSH public keys in authorized_keys")
	}
	for _, k := range l.AuthorizedKeys {
		if !sshPublicKey(k) {
			return nil, fmt.Errorf("lambda authorized_keys entry %q is not one SSH public key", k)
		}
	}
	user := l.User
	if user == "" {
		user = "ubuntu"
	}
	// A portable Linux login name: a lowercase letter or underscore, then
	// lowercase letters, digits, - and _, at most 32 in all.
	if len(user) > 32 || !(user[0] >= 'a' && user[0] <= 'z' || user[0] == '_') {
		return nil, fmt.Errorf("lambda user %q is not a plain login name", user)
	}
	for _, r := range user {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return nil, fmt.Errorf("lambda user %q is not a plain login name", user)
		}
	}
	for _, u := range l.AllowUsers {
		if u == "" || strings.ContainsFunc(u, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return nil, fmt.Errorf("lambda allow_users entry %q is not a tailnet login", u)
		}
	}
	binary := l.ErrandBinary
	if binary == "" {
		// The broker's own build serves when the machine matches it.
		if runtime.GOOS != "linux" || runtime.GOARCH != arch {
			return nil, fmt.Errorf("lambda errand_binary must name a linux/%s errand build (this runner is %s/%s)", arch, runtime.GOOS, runtime.GOARCH)
		}
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("lambda errand_binary: %w", err)
		}
		binary = self
	} else if !filepath.IsAbs(binary) {
		return nil, fmt.Errorf("lambda errand_binary must be an absolute path")
	}
	return &cloud.LambdaProvider{
		APIKeyFile: l.APIKeyFile, InstanceType: l.InstanceType, Regions: l.Regions, FileSystems: l.FileSystems,
		SSHKeyName: l.SSHKeyName, SSHPrivateKeyFile: l.SSHPrivateKeyFile, User: user,
		TailscaleAuthKeyFile: l.TailscaleAuthKeyFile, ErrandBinary: binary, Arch: arch, AllowUsers: l.AllowUsers,
		AuthorizedKeys: l.AuthorizedKeys,
	}, nil
}

// sshPublicKey accepts one authorized_keys line: a key type, its base64
// blob and an optional comment, without options.
func sshPublicKey(s string) bool {
	fields := strings.Fields(s)
	if len(fields) < 2 || strings.ContainsAny(s, "\r\n") || !strings.HasPrefix(fields[0], "ssh-") && !strings.HasPrefix(fields[0], "ecdsa-") && !strings.HasPrefix(fields[0], "sk-") {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(fields[1])
	return err == nil
}
