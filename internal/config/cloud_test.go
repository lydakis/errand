package config

import (
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
	if b, err := (DaemonCloud{}).Broker(); b != nil || err != nil {
		t.Fatalf("no offers must mean no broker: %v %v", b, err)
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
	if _, err := (DaemonCloud{MaxLeases: 3}).Broker(); err == nil {
		t.Error("accepted settings without offers")
	}
}

func TestLeaseRecordsBecomePeers(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	id := proto.NewULID()
	rec := LeaseRecord{Broker: "cloud", ID: id, Offer: "h100", Target: proto.LeaseTarget{URL: "http://box:7443"}}
	suffix := strings.ToLower(id[len(id)-4:])
	name, err := RecordLease(rec, map[string]Peer{"cloud-" + suffix: {URL: "http://taken:7443"}})
	if err != nil || name != "cloud-"+strings.ToLower(id[len(id)-5:]) {
		t.Fatalf("a configured name must not be reused: %q %v", name, err)
	}
	if again, _ := RecordLease(rec, nil); again != name {
		t.Fatalf("recording twice renamed the lease: %q", again)
	}
	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if url, err := c.PeerURL(name); err != nil || url != "http://box:7443" || c.Leases[name].ID != id {
		t.Fatalf("lease peer: %q %v %+v", url, err, c.Leases)
	}
	dropped, err := ForgetLeases(func(string, LeaseRecord) bool { return false })
	if err != nil || len(dropped) != 1 || dropped[0] != name {
		t.Fatalf("forget: %v %v", dropped, err)
	}
	if c, _ := LoadClient(); len(c.Peers) != 0 {
		t.Fatalf("forgotten lease still a peer: %+v", c.Peers)
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
ssh_key_name = "errand"
ssh_private_key_file = "/etc/errand/lambda_ed25519"
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
	if !ok || p.InstanceType != "gpu_1x_h100_pcie" || p.User != "ubuntu" || p.ErrandBinary != "/opt/errand-linux-amd64" || o.PricePerHour != 2.49 || o.Facts.OS != "linux" || o.Facts.Arch != "amd64" {
		t.Fatalf("offer %+v provider %+v", o, o.Provider)
	}

	lambda := LambdaOffer{InstanceType: "t", SSHKeyName: "k", APIKeyFile: "/a", SSHPrivateKeyFile: "/s", TailscaleAuthKeyFile: "/t", ErrandBinary: "/e"}
	for _, tc := range []struct {
		edit func(*CloudOffer)
		want string
	}{
		{func(o *CloudOffer) { o.Acquire = []string{"/a"} }, "not both"},
		{func(o *CloudOffer) { o.Lambda.APIKeyFile = "lambda.key" }, "api_key_file must be an absolute path"},
		{func(o *CloudOffer) { o.Lambda.InstanceType = "" }, "instance_type"},
		{func(o *CloudOffer) { o.Lambda.User = "root; reboot" }, "plain login name"},
		{func(o *CloudOffer) { o.Lambda.User = "-" }, "plain login name"},
		{func(o *CloudOffer) { o.Lambda.User = "9" }, "plain login name"},
		{func(o *CloudOffer) { o.Lambda.User = strings.Repeat("a", 33) }, "plain login name"},
		{func(o *CloudOffer) { o.Lambda.AllowUsers = []string{" "} }, "not a tailnet login"},
		{func(o *CloudOffer) { o.Lambda.AllowUsers = []string{"a@github", ""} }, "not a tailnet login"},
		{func(o *CloudOffer) { o.Lambda.AllowUsers = []string{"a@github\n"} }, "not a tailnet login"},
		{func(o *CloudOffer) { o.Lambda.TailscaleAuthKeyFile = "ts.key" }, "tailscale_auth_key_file must be an absolute path"},
		{func(o *CloudOffer) { o.Lambda.TailscaleAuthKeyFile = ""; o.Lambda.AllowUsers = []string{"a@github"} }, "needs tailscale_auth_key_file"},
		{func(o *CloudOffer) { o.Lambda.AuthorizedKeys = []string{"AAAA george@mac"} }, "not one SSH public key"},
		{func(o *CloudOffer) { o.Lambda.AuthorizedKeys = []string{"ssh-ed25519 AAAA a\nssh-ed25519 AAAA b"} }, "not one SSH public key"},
		{func(o *CloudOffer) { o.Lambda.AuthorizedKeys = []string{`command="sh" ssh-ed25519 AAAA`} }, "not one SSH public key"},
		{func(o *CloudOffer) { o.OS = "windows" }, "Lambda offers run linux"},
		{func(o *CloudOffer) { o.Lambda.ErrandBinary = ""; o.Arch = "riscv" }, "arch must"},
		{func(o *CloudOffer) { o.Price = -1 }, "price_per_hour"},
		{func(o *CloudOffer) { o.Price = math.NaN() }, "price_per_hour"},
		{func(o *CloudOffer) { o.Price = math.Inf(1) }, "price_per_hour"},
	} {
		l := lambda
		o := CloudOffer{Name: "x", Lambda: &l}
		tc.edit(&o)
		if _, err := (DaemonCloud{Offers: []CloudOffer{o}}).Broker(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.want, err)
		}
	}
	// Without a Tailscale key, clients' SSH keys are installed instead.
	ssh := lambda
	ssh.TailscaleAuthKeyFile = ""
	ssh.AuthorizedKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 george@mac"}
	if b, err := (DaemonCloud{Offers: []CloudOffer{{Name: "x", Lambda: &ssh}}}).Broker(); err != nil {
		t.Fatal(err)
	} else if p := b.Offers[0].Provider.(*cloud.LambdaProvider); p.TailscaleAuthKeyFile != "" || len(p.AuthorizedKeys) != 1 {
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
