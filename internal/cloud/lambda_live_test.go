package cloud

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/placement"
)

// TestLambdaLiveCatalog lists a real Lambda account's instance types when
// ERRAND_LAMBDA_API_KEY_FILE names its API key file, and checks that every
// GPU type parsed into a model and memory. It rents nothing.
func TestLambdaLiveCatalog(t *testing.T) {
	keyFile := os.Getenv("ERRAND_LAMBDA_API_KEY_FILE")
	if keyFile == "" {
		t.Skip("set ERRAND_LAMBDA_API_KEY_FILE to list a real Lambda account")
	}
	c := &LambdaCatalog{Account: LambdaProvider{APIKeyFile: keyFile, Pacing: &lambdaPacing{}}, IdleTimeout: time.Minute, MaxLifetime: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	key, err := readSecret(keyFile, "Lambda API key")
	if err != nil {
		t.Fatal(err)
	}
	types, err := c.Account.instanceTypes(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range types {
		f := it.facts(lambdaArch(it.Architecture))
		t.Logf("%-28s $%6.2f/h %-6s %3d cpu  %-24q -> %-20s capacity in %s", it.Name, float64(it.PriceCentsPerHour)/100, it.Architecture, it.VCPUs, it.GPUDescription, placement.DescribeGPUs(f.GPUs), strings.Join(it.Regions, ","))
		if it.GPUs > 0 && (len(f.GPUs) != it.GPUs || f.GPUs[0].MemoryMiB == 0) {
			t.Errorf("%s: %q parsed as %+v", it.Name, it.GPUDescription, f.GPUs)
		}
		if lambdaArch(it.Architecture) == "" {
			t.Errorf("%s: unknown architecture %q", it.Name, it.Architecture)
		}
	}
	offers, err := c.Offers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, o := range offers {
		lines = append(lines, fmt.Sprintf("%-28s $%6.2f/h %s", o.Name, o.PricePerHour, placement.DescribeGPUs(o.Facts.GPUs)))
	}
	t.Logf("%d offers with capacity now:\n%s", len(offers), strings.Join(lines, "\n"))
}
