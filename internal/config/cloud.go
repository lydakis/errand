package config

import (
	"fmt"
	"math"
	"path/filepath"
	"time"

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
	Name        string   `toml:"name"`
	OS          string   `toml:"os"`
	Arch        string   `toml:"arch"`
	CPUs        int      `toml:"cpus"`
	Tools       []string `toml:"tools"`
	GPU         string   `toml:"gpu"`            // model name as the driver reports it, e.g. "H100 80GB"
	GPUs        int      `toml:"gpus"`           // defaults to 1 when gpu or vram is set
	VRAM        int      `toml:"vram"`           // GiB per GPU
	Price       float64  `toml:"price_per_hour"` // USD, shown to callers
	Acquire     []string `toml:"acquire"`
	Release     []string `toml:"release"`
	IdleTimeout string   `toml:"idle_timeout"` // default 20m
	MaxLifetime string   `toml:"max_lifetime"` // default 12h
}

const (
	defaultLeaseIdle     = 20 * time.Minute
	defaultLeaseLifetime = 12 * time.Hour
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
	if len(c.Offers) == 0 {
		return &cloud.Config{}, nil
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
		if o.OS != "" && o.OS != "linux" && o.OS != "darwin" && o.OS != "windows" {
			return nil, fmt.Errorf("%s: os must be linux, darwin or windows", where)
		}
		if o.Arch != "" && o.Arch != "amd64" && o.Arch != "arm64" {
			return nil, fmt.Errorf("%s: arch must be amd64 or arm64", where)
		}
		if o.Price < 0 || math.IsNaN(o.Price) || math.IsInf(o.Price, 0) {
			return nil, fmt.Errorf("%s: price_per_hour must not be negative", where)
		}
		for _, argv := range [][]string{o.Acquire, o.Release} {
			if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
				return nil, fmt.Errorf("%s: acquire and release must be commands with an absolute executable path", where)
			}
		}
		provider := cloud.CommandProvider{AcquireCommand: o.Acquire, ReleaseCommand: o.Release}
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
