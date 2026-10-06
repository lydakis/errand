package cloud

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// LambdaCatalog offers a Lambda account's instance types that have capacity
// right now, so nothing about them needs configuring: each type's GPUs, CPUs,
// architecture and price come from Lambda. See docs/CLOUD.md.
type LambdaCatalog struct {
	// Account is what every offer shares; InstanceType and Arch are set per
	// offer.
	Account LambdaProvider
	// InstanceTypes limits the offers to these Lambda names; empty means
	// every type. Dearer types than Account.MaxPricePerHour are listed only
	// as unavailable, so a request for one says what to raise.
	InstanceTypes []string
	IdleTimeout   time.Duration
	MaxLifetime   time.Duration
}

func (c *LambdaCatalog) useStateDir(dir string) { c.Account.useStateDir(dir) }
func (c *LambdaCatalog) useVersion(v string)    { c.Account.useVersion(v) }

// Offers lists the account's instance types with capacity in a region a
// launch may use, cheapest first.
func (c *LambdaCatalog) Offers(ctx context.Context) ([]Offer, error) {
	key, err := readSecret(c.Account.APIKeyFile, "Lambda API key")
	if err != nil {
		return nil, err
	}
	types, err := c.Account.instanceTypes(ctx, key)
	if err != nil {
		return nil, err
	}
	regions, err := c.Account.launchRegions(ctx, key)
	if err != nil {
		return nil, err
	}
	// errand_binary runs on one architecture. One that cannot be read
	// leaves every type listed, so a lease says what is wrong with it.
	binaryArch := ""
	if c.Account.ErrandBinary != "" {
		binaryArch, _ = linuxBinaryArch(c.Account.ErrandBinary)
	}
	var offers []Offer
	for _, t := range types {
		if len(c.InstanceTypes) > 0 && !slices.Contains(c.InstanceTypes, t.Name) {
			continue
		}
		if !slices.ContainsFunc(t.Regions, func(r string) bool { return len(regions) == 0 || slices.Contains(regions, r) }) {
			continue
		}
		price := float64(t.PriceCentsPerHour) / 100
		unavailable := ""
		if limit := c.Account.MaxPricePerHour; limit > 0 && price > limit {
			unavailable = fmt.Sprintf("costs $%.2f/h, above max_price_per_hour = %g in [cloud.lambda]", price, limit)
		}
		arch, ok := lambdaArch(t.Architecture)
		if !ok || binaryArch != "" && arch != binaryArch {
			continue // errand has no build for it, or errand_binary is for another
		}
		p := c.Account
		p.InstanceType, p.Arch = t.Name, arch
		offers = append(offers, Offer{
			Name: lambdaOfferName(t.Name), Facts: t.facts(arch), Provider: &p, PricePerHour: &price,
			IdleTimeout: c.IdleTimeout, MaxLifetime: c.MaxLifetime, Unavailable: unavailable,
		})
	}
	sort.SliceStable(offers, func(i, j int) bool { return *offers[i].PricePerHour < *offers[j].PricePerHour })
	return offers, nil
}

// lambdaInstanceType is what Lambda says about one instance type.
type lambdaInstanceType struct {
	Name              string
	GPUDescription    string
	PriceCentsPerHour int
	Architecture      string
	VCPUs, GPUs       int
	Regions           []string // with capacity right now
}

// instanceTypes lists the account's instance types, by name.
func (p *LambdaProvider) instanceTypes(ctx context.Context, key string) ([]lambdaInstanceType, error) {
	var listed struct {
		Data map[string]struct {
			InstanceType struct {
				Name              string `json:"name"`
				GPUDescription    string `json:"gpu_description"`
				PriceCentsPerHour int    `json:"price_cents_per_hour"`
				Architecture      string `json:"architecture"`
				Specs             struct {
					VCPUs int `json:"vcpus"`
					GPUs  int `json:"gpus"`
				} `json:"specs"`
			} `json:"instance_type"`
			Regions []struct {
				Name string `json:"name"`
			} `json:"regions_with_capacity_available"`
		} `json:"data"`
	}
	if err := p.call(ctx, key, http.MethodGet, "/instance-types", nil, &listed); err != nil {
		return nil, fmt.Errorf("listing Lambda instance types: %w", err)
	}
	var types []lambdaInstanceType
	for name, t := range listed.Data {
		it := lambdaInstanceType{
			Name: name, GPUDescription: t.InstanceType.GPUDescription, PriceCentsPerHour: t.InstanceType.PriceCentsPerHour,
			Architecture: t.InstanceType.Architecture, VCPUs: t.InstanceType.Specs.VCPUs, GPUs: t.InstanceType.Specs.GPUs,
		}
		for _, r := range t.Regions {
			it.Regions = append(it.Regions, r.Name)
		}
		types = append(types, it)
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Name < types[j].Name })
	return types, nil
}

// lambdaGPU reads Lambda's GPU descriptions, such as "H100 (80 GB SXM5)",
// "GH200 (96 GB)" or "RTX 6000 (24 GB)": the model and its memory. A variant
// such as SXM5 is left out of the name: the driver's name for the GPU, which
// the leased machine is checked against, may not carry it.
var lambdaGPU = regexp.MustCompile(`^(.*?)\s*\((\d+)\s*GB\s*([^)]*)\)\s*$`)

// facts are the offer's claim about the machine, which the leased machine
// must then measure up to.
func (t lambdaInstanceType) facts(arch string) proto.Facts {
	f := proto.Facts{OS: "linux", Arch: arch, NumCPU: t.VCPUs}
	if t.GPUs <= 0 {
		return f
	}
	gpu := proto.GPU{Name: strings.TrimSpace(t.GPUDescription)}
	if m := lambdaGPU.FindStringSubmatch(t.GPUDescription); m != nil {
		gpu.Name = strings.TrimSpace(m[1])
		gib, _ := strconv.Atoi(m[2])
		gpu.MemoryMiB = gib << 10
	}
	for range min(t.GPUs, 64) {
		f.GPUs = append(f.GPUs, gpu)
	}
	return f
}

// lambdaOfferName is an offer name for an instance type: Lambda's own name
// with hyphens, such as gpu-1x-h100-sxm5, which offer names allow.
func lambdaOfferName(instanceType string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '-'
	}, instanceType)
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

var _ Catalog = (*LambdaCatalog)(nil)
