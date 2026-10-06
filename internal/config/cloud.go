package config

import (
	"fmt"
	"math"
	"path/filepath"
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
	// Lambda offers a Lambda Cloud account's instance types without an
	// offer for each: their shapes and prices come from Lambda.
	Lambda *LambdaAccount `toml:"lambda"`
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
	Price       *float64     `toml:"price_per_hour"` // USD, shown to callers
	Acquire     []string     `toml:"acquire"`
	Release     []string     `toml:"release"`
	Lambda      *LambdaOffer `toml:"lambda"`
	IdleTimeout string       `toml:"idle_timeout"` // default 20m
	MaxLifetime string       `toml:"max_lifetime"` // default 12h
}

// LambdaSettings is what every Lambda machine of one account shares.
type LambdaSettings struct {
	APIKeyFile           string   `toml:"api_key_file"`
	Regions              []string `toml:"regions"`
	FileSystems          []string `toml:"file_systems"`
	TailscaleAuthKeyFile string   `toml:"tailscale_auth_key_file"`
	ErrandBinary         string   `toml:"errand_binary"`
	AllowUsers           []string `toml:"allow_users"`
	// MaxPricePerHour refuses machines Lambda lists at a higher hourly
	// price: unset means DefaultLambdaMaxPrice, and 0 means no cap.
	MaxPricePerHour *float64 `toml:"max_price_per_hour"`
}

// LambdaOffer rents one Lambda instance type as a configured offer.
type LambdaOffer struct {
	LambdaSettings
	InstanceType string `toml:"instance_type"`
}

// LambdaAccount offers every instance type of a Lambda account that has
// capacity, or those in InstanceTypes, at up to MaxPricePerHour.
type LambdaAccount struct {
	LambdaSettings
	InstanceTypes []string `toml:"instance_types"`
	IdleTimeout   string   `toml:"idle_timeout"` // default 20m
	MaxLifetime   string   `toml:"max_lifetime"` // default 12h
}

const (
	defaultLeaseIdle     = 20 * time.Minute
	defaultLeaseLifetime = 12 * time.Hour
	// DefaultLambdaMaxPrice is the cap on a Lambda machine's hourly price
	// unless max_price_per_hour says otherwise: enough for any single-GPU
	// type, not for a sold-out type to turn into an eight-GPU box.
	DefaultLambdaMaxPrice = 10.0
)

// Broker validates the cloud section. Every runner has a broker: without
// offers it leases nothing, but it still ends leases made before its offers
// were removed, whatever other cloud settings are left.
func (c DaemonCloud) Broker() (*cloud.Config, error) {
	out := &cloud.Config{MaxLeases: c.MaxLeases}
	if c.MaxLeases < 0 {
		return nil, fmt.Errorf("cloud: max_leases must not be negative")
	}
	var err error
	if out.AcquireTimeout, err = positiveDuration("cloud acquire_timeout", c.AcquireTimeout, 15*time.Minute); err != nil {
		return nil, err
	}
	if c.Lambda != nil {
		if out.Catalog, err = c.Lambda.catalog(); err != nil {
			return nil, err
		}
	}
	if len(c.Offers) == 0 {
		if out.Catalog == nil {
			return &cloud.Config{}, nil
		}
		return out, nil
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
		if o.Price != nil && (*o.Price < 0 || math.IsNaN(*o.Price) || math.IsInf(*o.Price, 0)) {
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
		if o.GPUs > 64 {
			return nil, fmt.Errorf("%s: gpus must be at most 64", where)
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
	if l.InstanceType == "" {
		return nil, fmt.Errorf("lambda needs instance_type")
	}
	p, err := l.LambdaSettings.provider()
	if err != nil {
		return nil, err
	}
	p.InstanceType, p.Arch = l.InstanceType, arch
	return p, nil
}

// catalog offers the account's instance types as they are listed.
func (a LambdaAccount) catalog() (*cloud.LambdaCatalog, error) {
	p, err := a.LambdaSettings.provider()
	if err != nil {
		return nil, fmt.Errorf("cloud lambda: %w", err)
	}
	for _, t := range a.InstanceTypes {
		if t == "" || strings.ContainsFunc(t, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') }) {
			return nil, fmt.Errorf("cloud lambda: instance_types entry %q is not a Lambda instance type name such as gpu_1x_a10", t)
		}
	}
	c := &cloud.LambdaCatalog{Account: *p, InstanceTypes: a.InstanceTypes}
	if c.IdleTimeout, err = positiveDuration("cloud lambda idle_timeout", a.IdleTimeout, defaultLeaseIdle); err != nil {
		return nil, err
	}
	if c.MaxLifetime, err = positiveDuration("cloud lambda max_lifetime", a.MaxLifetime, defaultLeaseLifetime); err != nil {
		return nil, err
	}
	return c, nil
}

// provider checks the account settings. The Linux errand build for a
// machine is found when one is rented: errand_binary when set, otherwise
// this build or its release for the machine's architecture.
func (l LambdaSettings) provider() (*cloud.LambdaProvider, error) {
	for name, path := range map[string]string{"api_key_file": l.APIKeyFile, "tailscale_auth_key_file": l.TailscaleAuthKeyFile} {
		if !filepath.IsAbs(path) && (path != "" || name != "tailscale_auth_key_file") {
			return nil, fmt.Errorf("lambda %s must be an absolute path", name)
		}
	}
	// Without Tailscale the machine admits the SSH key of the client that
	// asked for it, not tailnet logins.
	if l.TailscaleAuthKeyFile == "" && len(l.AllowUsers) > 0 {
		return nil, fmt.Errorf("lambda allow_users names tailnet logins and needs tailscale_auth_key_file")
	}
	for _, u := range l.AllowUsers {
		if u == "" || strings.ContainsFunc(u, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return nil, fmt.Errorf("lambda allow_users entry %q is not a tailnet login", u)
		}
	}
	if l.ErrandBinary != "" && !filepath.IsAbs(l.ErrandBinary) {
		return nil, fmt.Errorf("lambda errand_binary must be an absolute path")
	}
	maxPrice := DefaultLambdaMaxPrice
	if l.MaxPricePerHour != nil {
		maxPrice = *l.MaxPricePerHour
		if maxPrice < 0 || math.IsNaN(maxPrice) || math.IsInf(maxPrice, 0) {
			return nil, fmt.Errorf("lambda max_price_per_hour must not be negative (0 means no cap)")
		}
	}
	return &cloud.LambdaProvider{
		APIKeyFile: l.APIKeyFile, Regions: l.Regions, FileSystems: l.FileSystems,
		TailscaleAuthKeyFile: l.TailscaleAuthKeyFile, ErrandBinary: l.ErrandBinary, AllowUsers: l.AllowUsers,
		MaxPricePerHour: maxPrice,
	}, nil
}
