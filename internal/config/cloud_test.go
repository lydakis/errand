package config

import (
	"errors"
	"strings"
	"testing"
	"time"
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
