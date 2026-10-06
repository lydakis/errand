package cloud

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// The catalog offers what Lambda lists with capacity, shaped and priced as
// Lambda says, cheapest first, and leaves out what cannot be rented.
func TestLambdaCatalogOffers(t *testing.T) {
	p, api, _ := newLambda(t)
	api.types = map[string]fakeInstanceType{
		"gpu_8x_h100_sxm5": {price: 2799, arch: "x86_64", gpu: "H100 (80 GB SXM5)", vcpus: 208, gpus: 8},
		"gpu_1x_gh200":     {price: 149, arch: "arm64", gpu: "GH200 (96 GB)", vcpus: 64, gpus: 1},
		"cpu_4x_general":   {price: 10, arch: "x86_64", gpu: "N/A", vcpus: 4, gpus: 0},
		"gpu_1x_a10":       {price: 75, arch: "x86_64", gpu: "A10 (24 GB PCIe)", vcpus: 30, gpus: 1, regions: []string{"us-west-1"}},
		"gpu_1x_future":    {price: 1, arch: "riscv", gpu: "X (1 GB)", vcpus: 1, gpus: 1},
	}
	c := &LambdaCatalog{Account: *p, IdleTimeout: time.Minute, MaxLifetime: time.Hour}
	c.Account.InstanceType, c.Account.Arch, c.Account.ErrandBinary, c.Account.Version = "", "", "", "v1.2.3"
	offers, err := c.Offers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range offers {
		names = append(names, fmt.Sprintf("%s $%.2f %s/%s %d cpu %s", o.Name, *o.PricePerHour, o.Facts.OS, o.Facts.Arch, o.Facts.NumCPU, describe(o.Facts.GPUs)))
	}
	// The A10 has capacity only where the account does not rent, and the
	// riscv type has no errand build.
	want := []string{
		"cpu-4x-general $0.10 linux/amd64 4 cpu none",
		"gpu-1x-gh200 $1.49 linux/arm64 64 cpu 1x GH200 (96 GiB)",
		"gpu-1x-h100-pcie $2.49 linux/amd64 26 cpu 1x H100 (80 GiB)",
		"gpu-8x-h100-sxm5 $27.99 linux/amd64 208 cpu 8x H100 (80 GiB)",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("offers:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	for _, o := range offers {
		lp := o.Provider.(*LambdaProvider)
		if lp.InstanceType != strings.ReplaceAll(o.Name, "-", "_") || lp.Arch != o.Facts.Arch || lp.APIKeyFile != p.APIKeyFile || lp.KeyDir != p.KeyDir || o.IdleTimeout != time.Minute || o.MaxLifetime != time.Hour {
			t.Errorf("offer %s has provider %+v", o.Name, lp)
		}
	}
	// A price cap keeps dearer types only as unavailable, with the reason
	// a refused request shows; a list of types narrows the catalog.
	c.Account.MaxPricePerHour = 2.49
	offers, _ = c.Offers(context.Background())
	if len(offers) != 4 || offers[2].Unavailable != "" || offers[3].Name != "gpu-8x-h100-sxm5" || offers[3].Unavailable != "costs $27.99/h, above max_price_per_hour = 2.49 in [cloud.lambda]" {
		t.Fatalf("capped: %+v", offers)
	}
	c.InstanceTypes = []string{"gpu_8x_h100_sxm5", "gpu_1x_gh200"}
	if offers, _ = c.Offers(context.Background()); len(offers) != 2 || offers[0].Name != "gpu-1x-gh200" || offers[1].Unavailable == "" {
		t.Fatalf("listed and capped: %+v", offers)
	}
	// Any region: the A10 is in.
	c.InstanceTypes, c.Account.MaxPricePerHour, c.Account.Regions = nil, 0, nil
	if offers, _ = c.Offers(context.Background()); len(offers) != 5 || offers[1].Name != "gpu-1x-a10" {
		t.Fatalf("any region: %+v", offers)
	}
	// An errand_binary for amd64 cannot run on the arm64 GH200, which is
	// listed as unavailable with what to change.
	c.Account.ErrandBinary = p.ErrandBinary
	offers, _ = c.Offers(context.Background())
	if len(offers) != 5 || slices.ContainsFunc(offers, func(o Offer) bool {
		return (o.Unavailable != "") != (o.Facts.Arch == "arm64")
	}) || offers[2].Name != "gpu-1x-gh200" || offers[2].Unavailable != "errand_binary is a linux/amd64 build, which cannot run on linux/arm64 machines" {
		t.Fatalf("errand_binary for amd64: %+v", offers)
	}
	// A development build without errand_binary has no release to install,
	// so only its own architecture, and only when it is a Linux errand
	// build itself, is available. The test binary is not one.
	c.Account.ErrandBinary, c.Account.Version = "", "0.1.0-dev"
	offers, _ = c.Offers(context.Background())
	if len(offers) != 5 || slices.ContainsFunc(offers, func(o Offer) bool {
		return !strings.Contains(o.Unavailable, "development build") || !strings.Contains(o.Unavailable, "set errand_binary to a linux/"+o.Facts.Arch)
	}) {
		t.Fatalf("development build: %+v", offers)
	}
	c.Account.Version = "v1.2.3"
	os.WriteFile(p.APIKeyFile, []byte("wrong"), 0600)
	if _, err := c.Offers(context.Background()); err == nil || !strings.Contains(err.Error(), "API key was invalid") {
		t.Fatalf("bad key: %v", err)
	}
}

// With file systems, a type is offered only with capacity in their region,
// the only one a launch may use.
func TestLambdaCatalogFileSystemRegion(t *testing.T) {
	p, api, _ := newLambda(t)
	api.types = map[string]fakeInstanceType{
		"gpu_1x_a10":       {price: 75, arch: "x86_64", gpu: "A10 (24 GB PCIe)", vcpus: 30, gpus: 1, regions: []string{"us-west-1"}},
		"gpu_1x_h100_pcie": {price: 249, arch: "x86_64", gpu: "H100 (80 GB PCIe)", vcpus: 26, gpus: 1, regions: []string{"us-east-1"}},
	}
	api.fileSystems = map[string]string{"datasets": "us-east-1"}
	c := &LambdaCatalog{Account: *p, IdleTimeout: time.Minute, MaxLifetime: time.Hour}
	c.Account.Regions, c.Account.FileSystems = nil, []string{"datasets"}
	offers, err := c.Offers(context.Background())
	if err != nil || len(offers) != 1 || offers[0].Name != "gpu-1x-h100-pcie" {
		t.Fatalf("offers %+v: %v", offers, err)
	}
	// File systems outside the account's regions offer nothing, and say why.
	c.Account.Regions = []string{"us-west-1"}
	if _, err := c.Offers(context.Background()); err == nil || !strings.Contains(err.Error(), "file systems are in us-east-1, which regions does not list") {
		t.Fatalf("file systems outside regions: %v", err)
	}
}

func describe(gpus []proto.GPU) string {
	if len(gpus) == 0 {
		return "none"
	}
	return fmt.Sprintf("%dx %s (%d GiB)", len(gpus), gpus[0].Name, gpus[0].MemoryMiB>>10)
}

// Lambda's GPU descriptions become a model that gpu=MODEL matches and the
// memory vram>=N checks.
func TestLambdaGPUDescriptions(t *testing.T) {
	for desc, want := range map[string]string{
		"H100 (80 GB SXM5)":  "H100/80",
		"H100 (80 GB PCIe)":  "H100/80",
		"A100 (40 GB SXM4)":  "A100/40",
		"GH200 (96 GB)":      "GH200/96",
		"RTX 6000 (24 GB)":   "RTX 6000/24",
		"Tesla V100 (16 GB)": "Tesla V100/16",
		"B200 (180 GB SXM6)": "B200/180",
		"Mystery GPU":        "Mystery GPU/0",
	} {
		f := lambdaInstanceType{GPUDescription: desc, GPUs: 2}.facts("amd64")
		if got := fmt.Sprintf("%s/%d", f.GPUs[0].Name, f.GPUs[0].MemoryMiB>>10); got != want || len(f.GPUs) != 2 {
			t.Errorf("%q: %s, want %s", desc, got, want)
		}
	}
	if f := (lambdaInstanceType{GPUDescription: "N/A", GPUs: 0, VCPUs: 4}).facts("arm64"); len(f.GPUs) != 0 || f.NumCPU != 4 || f.Arch != "arm64" || f.OS != "linux" {
		t.Errorf("cpu type: %+v", f)
	}
	for in, want := range map[string]string{"gpu_1x_h100_sxm5": "gpu-1x-h100-sxm5", "GPU_8x.H100": "gpu-8x-h100", strings.Repeat("a", 40): strings.Repeat("a", 32)} {
		if got := lambdaOfferName(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// fakeRelease serves one errand release the way GitHub does.
type fakeRelease struct {
	version, arch string
	binary        []byte
	sums          string
	requests      atomic.Int32
}

func newFakeRelease(t *testing.T, version, arch string, binary []byte) (*fakeRelease, *httptest.Server) {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{{"LICENSE", []byte("MIT")}, {"errand", binary}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write(f.body)
	}
	tw.Close()
	gz.Close()
	asset := fmt.Sprintf("errand_%s_linux_%s.tar.gz", version, arch)
	sum := sha256.Sum256(archive.Bytes())
	r := &fakeRelease{version: version, arch: arch, binary: archive.Bytes()}
	r.sums = fmt.Sprintf("%s  errand_%s_darwin_arm64.tar.gz\n%s  %s\n", strings.Repeat("0", 64), version, hex.EncodeToString(sum[:]), asset)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests.Add(1)
		switch req.URL.Path {
		case "/v" + version + "/checksums.txt":
			w.Write([]byte(r.sums))
		case "/v" + version + "/" + asset:
			w.Write(r.binary)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return r, srv
}

// Without errand_binary, a cloud peer that is not itself a Linux build for
// the machine fetches this version's release, checks it against the
// release's checksums, keeps it, and sends it to the machine.
func TestLambdaDownloadsRelease(t *testing.T) {
	arch := "arm64"
	if runtime.GOARCH == "arm64" {
		arch = "amd64"
	}
	binary, _ := os.ReadFile(fakeErrand(t, "linux", arch))
	release, srv := newFakeRelease(t, "1.2.3", arch, binary)
	p, api, ssh := newLambda(t)
	api.types = map[string]fakeInstanceType{"gpu_1x_gh200": {price: 149, arch: map[string]string{"arm64": "arm64", "amd64": "x86_64"}[arch], gpu: "GH200 (96 GB)", vcpus: 64, gpus: 1}}
	p.InstanceType, p.Arch, p.ErrandBinary, p.Version, p.ReleaseURL = "gpu_1x_gh200", arch, "", "v1.2.3", srv.URL
	var progress []string
	req := AcquireRequest{LeaseID: proto.NewULID(), Login: "george@github", Progress: func(s string) { progress = append(progress, s) }, Save: noSave}
	if _, err := p.Acquire(context.Background(), req); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(progress, "\n"))
	}
	if ssh.files["errand"] != string(binary) {
		t.Fatalf("machine got %d bytes of errand, want the release's %d", len(ssh.files["errand"]), len(binary))
	}
	if !strings.Contains(strings.Join(progress, "\n"), "downloading errand 1.2.3 for linux/"+arch) {
		t.Fatalf("progress %q", progress)
	}
	path := filepath.Join(p.KeyDir, "bin", "errand-1.2.3-linux-"+arch)
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Fatalf("kept build: %v %v", info, err)
	}
	// The kept build serves the next launch without another download.
	before := release.requests.Load()
	if _, err := p.Acquire(context.Background(), req); err != nil || release.requests.Load() != before {
		t.Fatalf("second launch: %v, %d more requests", err, release.requests.Load()-before)
	}
	// A release that does not match its checksum is not installed, and
	// nothing is rented.
	os.Remove(path)
	release.binary = append(release.binary, 0)
	launches := len(api.launches)
	if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "does not match its checksum") || len(api.launches) != launches {
		t.Fatalf("tampered release: %v, %d launches", err, len(api.launches)-launches)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("tampered release was kept")
	}
	// A development build has no release; it needs errand_binary.
	p.Version = "0.1.0-dev"
	if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "development build") || !strings.Contains(err.Error(), "set errand_binary") || len(api.launches) != launches {
		t.Fatalf("dev build: %v", err)
	}
	// A release that is not an errand build for the machine is refused.
	p.Version = "v1.2.3"
	other, _ := os.ReadFile(fakeErrand(t, "freebsd", arch))
	_, srv2 := newFakeRelease(t, "1.2.3", arch, other)
	p.ReleaseURL = srv2.URL
	if _, err := p.Acquire(context.Background(), req); err == nil || !strings.Contains(err.Error(), "errand for freebsd") || len(api.launches) != launches {
		t.Fatalf("wrong release: %v", err)
	}
}
