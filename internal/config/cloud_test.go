package config

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/cloud"
	"github.com/lydakis/errand/internal/proto"
)

func TestCloudOffers(t *testing.T) {
	// Without offers a runner still ends the leases it made before.
	if b, err := (DaemonCloud{}).Broker(); b == nil || len(b.Offers) != 0 || err != nil {
		t.Fatalf("no offers must mean a broker without offers: %v %v", b, err)
	}
	// Removing the last offer while leaving other settings must not stop
	// the runner, since it still has leases to end.
	if b, err := (DaemonCloud{MaxLeases: 4, AcquireTimeout: "20m"}).Broker(); b == nil || len(b.Offers) != 0 || err != nil {
		t.Fatalf("leftover settings without offers: %v %v", b, err)
	}
	b, err := DaemonCloud{Offers: []CloudOffer{{Name: "a100x8", OS: "linux", GPU: "A100-SXM4-80GB", GPUs: 8, VRAM: 80, Tools: []string{"docker"},
		Acquire: []string{"/opt/p", "acquire"}, Release: []string{"/opt/p", "release"}, IdleTimeout: "5m"}}}.Broker()
	if err != nil {
		t.Fatal(err)
	}
	o := b.Offers[0]
	if len(o.Facts.GPUs) != 8 || o.Facts.GPUs[0].MemoryMiB != 80<<10 || o.Facts.Tools["docker"] == "" || o.IdleTimeout != 5*time.Minute || o.MaxLifetime != 12*time.Hour || b.AcquireTimeout != 15*time.Minute {
		t.Fatalf("offer %+v", b)
	}
	valid := CloudOffer{Name: "x", Acquire: []string{"/a"}, Release: []string{"/r"}}
	for _, tc := range []struct {
		edit func(*CloudOffer)
		want string
	}{
		{func(o *CloudOffer) { o.Name = "H100" }, "lowercase"},
		{func(o *CloudOffer) { o.Acquire = []string{"provider.sh"} }, "absolute"},
		{func(o *CloudOffer) { o.Release = nil }, "absolute"},
		{func(o *CloudOffer) { o.VRAM = 80 }, "gpu model"},
		{func(o *CloudOffer) { o.GPU, o.GPUs = "H100", 1<<30 }, "at most 64"},
		{func(o *CloudOffer) { o.IdleTimeout = "0s" }, "positive duration"},
		{func(o *CloudOffer) { o.OS = "plan9" }, "os must"},
	} {
		o := valid
		tc.edit(&o)
		if _, err := (DaemonCloud{Offers: []CloudOffer{o}}).Broker(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v", o, err)
		}
	}
	if _, err := (DaemonCloud{Offers: []CloudOffer{valid, valid}}).Broker(); err == nil {
		t.Error("accepted a duplicate offer")
	}
	if _, err := (DaemonCloud{MaxLeases: -1}).Broker(); err == nil {
		t.Error("accepted a negative max_leases without offers")
	}
}

// Names no peer is configured under go to FindLeasePeer, once per process.
func TestLeasePeersAreFound(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	asked := 0
	FindLeasePeer = func(c Client, name string) (Peer, bool, error) {
		asked++
		switch name {
		case "cloud-7f3a":
			return Peer{SSH: "ubuntu@203.0.113.7", RemoteCommand: "/opt/errand"}, true, nil
		case "cloud-dead":
			return Peer{}, false, errors.New("cloud has no active lease ending in dead")
		}
		return Peer{}, false, nil
	}
	leasePeers.Clear()
	t.Cleanup(func() { FindLeasePeer = nil; leasePeers.Clear() })
	c := Client{Peers: map[string]Peer{"cloud": {URL: "http://cloud:7443"}}}
	for range 2 {
		if url, err := c.PeerURL("cloud-7f3a"); err != nil || url != "ssh://ubuntu@203.0.113.7" || c.SSHRemoteCommand("cloud-7f3a") != "/opt/errand" {
			t.Fatalf("lease peer: %q %v", url, err)
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times", asked)
	}
	if _, err := c.PeerURL("cloud-dead"); err == nil || !strings.Contains(err.Error(), "no active lease") {
		t.Fatalf("ended lease: %v", err)
	}
	if _, err := c.PeerURL("elsewhere"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unknown peer: %v", err)
	}
	if url, err := c.PeerURL("cloud"); err != nil || url != "http://cloud:7443" || asked != 3 {
		t.Fatalf("configured peer: %q %v (asked %d)", url, err, asked)
	}
}

func TestLambdaOffers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "errandd.toml")
	os.WriteFile(path, []byte(`
[[cloud.offers]]
name = "h100"
gpu = "H100 PCIe"
vram = 80
price_per_hour = 2.49

[cloud.offers.lambda]
instance_type = "gpu_1x_h100_pcie"
regions = ["us-east-1"]
api_key_file = "/etc/errand/lambda.key"
tailscale_auth_key_file = "/etc/errand/ts.key"
errand_binary = "/opt/errand-linux-amd64"
allow_users = ["broker@example"]
`), 0600)
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Cloud.Broker()
	if err != nil {
		t.Fatal(err)
	}
	o := b.Offers[0]
	p, ok := o.Provider.(*cloud.LambdaProvider)
	if !ok || p.InstanceType != "gpu_1x_h100_pcie" || p.ErrandBinary != "/opt/errand-linux-amd64" || o.PricePerHour == nil || *o.PricePerHour != 2.49 || o.Facts.OS != "linux" || o.Facts.Arch != "amd64" {
		t.Fatalf("offer %+v provider %+v", o, o.Provider)
	}

	lambda := LambdaOffer{InstanceType: "t", APIKeyFile: "/a", TailscaleAuthKeyFile: "/t", ErrandBinary: "/e"}
	for _, tc := range []struct {
		edit func(*CloudOffer)
		want string
	}{
		{func(o *CloudOffer) { o.Acquire = []string{"/a"} }, "not both"},
		{func(o *CloudOffer) { o.Lambda.APIKeyFile = "lambda.key" }, "api_key_file must be an absolute path"},
		{func(o *CloudOffer) { o.Lambda.InstanceType = "" }, "instance_type"},
		{func(o *CloudOffer) { o.Lambda.AllowUsers = []string{" "} }, "not a tailnet login"},
		{func(o *CloudOffer) { o.Lambda.AllowUsers = []string{"a@github", ""} }, "not a tailnet login"},
		{func(o *CloudOffer) { o.Lambda.AllowUsers = []string{"a@github\n"} }, "not a tailnet login"},
		{func(o *CloudOffer) { o.Lambda.TailscaleAuthKeyFile = "ts.key" }, "tailscale_auth_key_file must be an absolute path"},
		{func(o *CloudOffer) { o.Lambda.TailscaleAuthKeyFile = ""; o.Lambda.AllowUsers = []string{"a@github"} }, "needs tailscale_auth_key_file"},
		{func(o *CloudOffer) { o.OS = "windows" }, "Lambda offers run linux"},
		{func(o *CloudOffer) { o.Lambda.ErrandBinary = ""; o.Arch = "riscv" }, "arch must"},
		{func(o *CloudOffer) { o.Price = new(-1.0) }, "price_per_hour"},
		{func(o *CloudOffer) { o.Price = new(math.NaN()) }, "price_per_hour"},
		{func(o *CloudOffer) { o.Price = new(math.Inf(1)) }, "price_per_hour"},
	} {
		l := lambda
		o := CloudOffer{Name: "x", Lambda: &l}
		tc.edit(&o)
		if _, err := (DaemonCloud{Offers: []CloudOffer{o}}).Broker(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.want, err)
		}
	}
	// Without a Tailscale key, the API key alone is enough.
	ssh := LambdaOffer{InstanceType: "t", APIKeyFile: "/a", ErrandBinary: "/e"}
	if b, err := (DaemonCloud{Offers: []CloudOffer{{Name: "x", Lambda: &ssh}}}).Broker(); err != nil {
		t.Fatal(err)
	} else if p := b.Offers[0].Provider.(*cloud.LambdaProvider); p.TailscaleAuthKeyFile != "" {
		t.Fatalf("provider %+v", p)
	}
	// Without errand_binary the broker installs itself, which only fits a
	// machine of its own platform.
	l := lambda
	l.ErrandBinary = ""
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	if _, err := (DaemonCloud{Offers: []CloudOffer{{Name: "x", Arch: other, Lambda: &l}}}).Broker(); err == nil || !strings.Contains(err.Error(), "errand_binary must name a linux/"+other) {
		t.Errorf("foreign arch without errand_binary: %v", err)
	}
	// Command offers may now declare Windows machines.
	if _, err := (DaemonCloud{Offers: []CloudOffer{{Name: "win", OS: "windows", Acquire: []string{"/a"}, Release: []string{"/r"}}}}).Broker(); err != nil {
		t.Errorf("windows offer: %v", err)
	}
}

// A price of zero is kept as a price all the way to clients, so a free
// offer is not taken for an unpriced one.
func TestFreeOfferKeepsItsPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errandd.toml")
	os.WriteFile(path, []byte(`
[[cloud.offers]]
name = "free"
price_per_hour = 0
acquire = ["/a"]
release = ["/r"]

[[cloud.offers]]
name = "unpriced"
acquire = ["/a"]
release = ["/r"]
`), 0600)
	d, err := LoadDaemon(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Cloud.Broker()
	if err != nil {
		t.Fatal(err)
	}
	for i, wantPriced := range []bool{true, false} {
		data, _ := json.Marshal(b.Offers[i].Offer())
		var o proto.Offer
		if err := json.Unmarshal(data, &o); err != nil {
			t.Fatal(err)
		}
		if priced := o.PricePerHour != nil; priced != wantPriced || priced && *o.PricePerHour != 0 {
			t.Errorf("%s: sent as %s", b.Offers[i].Name, data)
		}
	}
}
