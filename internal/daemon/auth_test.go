package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

func testDestination() net.Addr { return stringAddr("100.101.102.103:7443") }

func fakeWhoisSocket(t *testing.T, response any) string {
	return fakeWhoisSocketVersion(t, response, "1.100.0")
}

func fakeWhoisSocketWithConnState(t *testing.T, response any, connState func(net.Conn, http.ConnState)) string {
	return fakeWhoisSocketHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Tailscale-Version", "1.100.0")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}, connState)
}

func fakeWhoisSocketVersion(t *testing.T, response any, version string) string {
	return fakeWhoisSocketHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		if version != "" {
			w.Header().Set("Tailscale-Version", version)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}, nil)
}

func fakeWhoisSocketHandler(t *testing.T, handler http.HandlerFunc, connState func(net.Conn, http.ConnState)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "errand-whois-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "tailscaled.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ConnState: connState}
	go server.Serve(listener)
	t.Cleanup(func() {
		server.Shutdown(context.Background())
	})
	return socket
}

func TestHandlerScopesWhoIsToAcceptedDestination(t *testing.T) {
	response := map[string]any{
		"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
		"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
		"CapMap": map[string]any{
			proto.DefaultCapability: []any{map[string]any{"actions": []string{proto.ActionSubmit}}},
		},
	}
	destination := make(chan string, 1)
	socket := fakeWhoisSocketHandler(t, func(w http.ResponseWriter, r *http.Request) {
		destination <- r.URL.Query().Get("dst_ip")
		w.Header().Set("Tailscale-Version", "1.100.0")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}, nil)
	d, err := New(Config{StateDir: t.TempDir(), TailscaledSocket: socket})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ts := httptest.NewServer(d.Handler())
	defer ts.Close()

	res, err := ts.Client().Get(ts.URL + "/v0/info")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("destination-scoped info request = %s, want 200", res.Status)
	}
	want := ts.Listener.Addr().(*net.TCPAddr).IP.String()
	select {
	case got := <-destination:
		if got != want {
			t.Fatalf("WhoIs dst_ip = %q, want accepted destination %q", got, want)
		}
	default:
		t.Fatal("WhoIs request was not observed")
	}
}

func TestHandlerRejectsTailscaledWithoutDestinationScopedWhoIs(t *testing.T) {
	response := map[string]any{
		"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
		"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
		"CapMap": map[string]any{
			proto.DefaultCapability: []any{map[string]any{"actions": []string{proto.ActionSubmit}}},
		},
	}
	for _, version := range []string{"", "1.98.2"} {
		t.Run("version="+version, func(t *testing.T) {
			d, err := New(Config{
				StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocketVersion(t, response, version),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ts := httptest.NewServer(d.Handler())
			defer ts.Close()
			res, err := ts.Client().Get(ts.URL + "/v0/info")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("info with tailscaled %q = %s, want 403", version, res.Status)
			}
		})
	}
}

func TestAcceptedDestinationIP(t *testing.T) {
	tests := []struct {
		name string
		addr net.Addr
		want string
		ok   bool
	}{
		{name: "ipv4", addr: stringAddr("100.101.102.103:7443"), want: "100.101.102.103", ok: true},
		{name: "ipv6", addr: stringAddr("[fd7a:115c:a1e0::1]:7443"), want: "fd7a:115c:a1e0::1", ok: true},
		{name: "mapped ipv4", addr: stringAddr("[::ffff:100.101.102.103]:7443"), want: "100.101.102.103", ok: true},
		{name: "missing", addr: nil},
		{name: "ipv4 wildcard", addr: stringAddr("0.0.0.0:7443")},
		{name: "ipv6 wildcard", addr: stringAddr("[::]:7443")},
		{name: "hostname", addr: stringAddr("runner.example.com:7443")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := acceptedDestinationIP(tt.addr)
			if tt.ok {
				if err != nil || got != tt.want {
					t.Fatalf("acceptedDestinationIP(%v) = %q, %v; want %q", tt.addr, got, err, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("acceptedDestinationIP(%v) = %q, want an error", tt.addr, got)
			}
		})
	}
}

func TestSameHostConnection(t *testing.T) {
	for _, tt := range []struct {
		name   string
		remote string
		local  net.Addr
		want   bool
	}{
		{name: "tailnet self", remote: "100.101.102.103:52123", local: stringAddr("100.101.102.103:7443"), want: true},
		{name: "loopback self", remote: "127.0.0.1:52123", local: stringAddr("127.0.0.1:7443"), want: true},
		{name: "mapped self", remote: "[::ffff:100.101.102.103]:52123", local: stringAddr("100.101.102.103:7443"), want: true},
		{name: "other peer", remote: "100.101.102.104:52123", local: stringAddr("100.101.102.103:7443")},
		{name: "malformed remote", remote: "not-an-address", local: stringAddr("100.101.102.103:7443")},
		{name: "missing local", remote: "100.101.102.103:52123"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameHostConnection(tt.remote, tt.local); got != tt.want {
				t.Fatalf("sameHostConnection(%q, %v) = %v, want %v", tt.remote, tt.local, got, tt.want)
			}
		})
	}
}

func TestHandlerRejectsSelfTarget(t *testing.T) {
	d := &Daemon{}
	called := false
	handler := d.auth("", func(http.ResponseWriter, *http.Request, Identity) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/v0/info", nil)
	req.RemoteAddr = "100.101.102.103:52123"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, stringAddr("100.101.102.103:7443")))
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "self-targeting is not supported") {
		t.Fatalf("self-target response = %d %q, want 403 self-targeting error", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("self-target request reached the endpoint")
	}
}

func TestInsecureTestHandlerAllowsSameHost(t *testing.T) {
	d := &Daemon{cfg: Config{InsecureNoAuth: true}}
	called := false
	handler := d.auth("", func(http.ResponseWriter, *http.Request, Identity) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/v0/info", nil)
	req.RemoteAddr = "127.0.0.1:52123"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, stringAddr("127.0.0.1:7443")))
	handler(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("insecure in-process test request was rejected as a self-target")
	}
}

func TestWhoIsReusesHTTPTransport(t *testing.T) {
	response := map[string]any{
		"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
		"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
		"CapMap": map[string]any{
			proto.DefaultCapability: []any{map[string]any{"actions": []string{proto.ActionSubmit}}},
		},
	}
	var connections atomic.Int32
	socket := fakeWhoisSocketWithConnState(t, response, func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	})
	d, err := New(Config{StateDir: t.TempDir(), TailscaledSocket: socket})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i := 0; i < 10; i++ {
		if _, err := d.identify("100.64.0.1:1234", testDestination()); err != nil {
			t.Fatal(err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("WhoIs opened %d LocalAPI connections for 10 requests, want 1", got)
	}
}

func TestIdentifyCapabilityAndAllowlist(t *testing.T) {
	if proto.DefaultCapability != "lydakis.dev/cap/errand" {
		t.Fatalf("default capability = %q, want documented key", proto.DefaultCapability)
	}
	response := map[string]any{
		"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
		"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
		"CapMap": map[string]any{
			proto.DefaultCapability: []any{map[string]any{
				"actions": []string{proto.ActionSubmit, proto.ActionReadOwn},
			}},
		},
	}
	d, err := New(Config{
		StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.identify("100.64.0.1:1234", testDestination())
	if err != nil {
		t.Fatal(err)
	}
	if id.Owner() != "tailnet-user:42" || !id.Allowed(proto.ActionSubmit) ||
		!id.Allowed(proto.ActionReadOwn) || id.Allowed(proto.ActionKillOwn) {
		t.Fatalf("unexpected capability identity: %+v", id)
	}

	d.cfg.AllowUsers = []string{"george@example.com"}
	id, err = d.identify("100.64.0.1:1234", testDestination())
	if err != nil || !id.Allowed(proto.ActionKillOwn) {
		t.Fatalf("allowlist did not grant all actions: %+v, %v", id, err)
	}
}

func TestCacheGCEndpointRequiresManageCachesAction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		actions []string
		want    int
	}{
		{name: "submit only", actions: []string{proto.ActionSubmit}, want: http.StatusForbidden},
		{name: "cache manager", actions: []string{proto.ActionCaches}, want: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := map[string]any{
				"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
				"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
				"CapMap": map[string]any{
					proto.DefaultCapability: []any{map[string]any{"actions": tt.actions}},
				},
			}
			d, err := New(Config{
				StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.Close() })
			ts := httptest.NewServer(d.Handler())
			t.Cleanup(ts.Close)

			res, err := ts.Client().Post(ts.URL+"/v0/cache/gc", "application/json", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != tt.want {
				t.Fatalf("POST /v0/cache/gc = %s, want %d", res.Status, tt.want)
			}
		})
	}
}

func TestStorageInspectionEndpointRequiresReadOwnAction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		actions []string
		want    int
	}{
		{name: "cache manager", actions: []string{proto.ActionCaches}, want: http.StatusForbidden},
		{name: "job reader", actions: []string{proto.ActionReadOwn}, want: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := map[string]any{
				"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
				"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
				"CapMap": map[string]any{
					proto.DefaultCapability: []any{map[string]any{"actions": tt.actions}},
				},
			}
			d, err := New(Config{StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.Close() })
			ts := httptest.NewServer(d.Handler())
			t.Cleanup(ts.Close)

			res, err := ts.Client().Get(ts.URL + "/v0/storage")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != tt.want {
				t.Fatalf("GET /v0/storage = %s, want %d", res.Status, tt.want)
			}
		})
	}
}

func TestJobGCEndpointRequiresGCAction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		actions []string
		want    int
	}{
		{name: "read only", actions: []string{proto.ActionReadOwn}, want: http.StatusForbidden},
		{name: "job collector", actions: []string{proto.ActionGCJobs}, want: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := map[string]any{
				"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
				"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
				"CapMap": map[string]any{
					proto.DefaultCapability: []any{map[string]any{"actions": tt.actions}},
				},
			}
			d, err := New(Config{StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.Close() })
			ts := httptest.NewServer(d.Handler())
			t.Cleanup(ts.Close)
			keep := 1
			clientID := "0123456789abcdef0123456789abcdef"
			jobGCBody, _ := json.Marshal(proto.JobGCRequest{Keep: &keep})
			ackBody, _ := json.Marshal(proto.ChangeReconciliationAck{
				ClientID: clientID, JobIDs: []string{proto.NewULID()},
			})
			for _, endpoint := range []struct {
				method string
				path   string
				body   []byte
			}{
				{method: http.MethodPost, path: "/v0/jobs/gc", body: jobGCBody},
				{method: http.MethodGet, path: "/v0/change-reconciliation?client_id=" + clientID},
				{method: http.MethodPost, path: "/v0/change-reconciliation/ack", body: ackBody},
			} {
				req, err := http.NewRequest(endpoint.method, ts.URL+endpoint.path, bytes.NewReader(endpoint.body))
				if err != nil {
					t.Fatal(err)
				}
				resp, err := ts.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != tt.want {
					t.Fatalf("%s %s = %s, want %d", endpoint.method, endpoint.path, resp.Status, tt.want)
				}
			}
		})
	}
}

func TestChangeEndpointRequiresReadOwnAction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		actions []string
		want    int
	}{
		{name: "submit only", actions: []string{proto.ActionSubmit}, want: http.StatusForbidden},
		{name: "reader", actions: []string{proto.ActionReadOwn}, want: http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := map[string]any{
				"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
				"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
				"CapMap": map[string]any{
					proto.DefaultCapability: []any{map[string]any{"actions": tt.actions}},
				},
			}
			d, err := New(Config{StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response)})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ts := httptest.NewServer(d.Handler())
			defer ts.Close()
			resp, err := http.Get(ts.URL + "/v0/jobs/" + proto.NewULID() + "/changes")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("GET changes = %s, want %d", resp.Status, tt.want)
			}
		})
	}
}

func TestForwardEndpointRequiresForwardOwnAction(t *testing.T) {
	for _, tt := range []struct {
		name    string
		actions []string
		want    int
	}{
		{name: "read only", actions: []string{proto.ActionReadOwn}, want: http.StatusForbidden},
		{name: "forwarder", actions: []string{proto.ActionForwardOwn}, want: http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := map[string]any{
				"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
				"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
				"CapMap": map[string]any{
					proto.DefaultCapability: []any{map[string]any{"actions": tt.actions}},
				},
			}
			d, err := New(Config{StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response)})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ts := httptest.NewServer(d.Handler())
			defer ts.Close()
			resp, err := http.Post(ts.URL+"/v0/jobs/"+proto.NewULID()+"/ports/3000/connect",
				"application/octet-stream", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("POST forward = %s, want %d", resp.Status, tt.want)
			}
		})
	}
}

func TestIdentifyFailsClosedWithoutValidGrant(t *testing.T) {
	response := map[string]any{
		"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
		"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
		"CapMap": map[string]any{
			proto.DefaultCapability: []any{"malformed"},
		},
	}
	d, err := New(Config{
		StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.identify("100.64.0.1:1234", testDestination()); err == nil {
		t.Fatal("malformed capability unexpectedly authorized caller")
	}
}

func TestIdentifyHonorsCustomCapabilityOverride(t *testing.T) {
	const customCapability = "example.com/cap/custom-errand"
	response := map[string]any{
		"Node":        map[string]any{"Name": "laptop.tailnet.ts.net.", "StableID": "node-1"},
		"UserProfile": map[string]any{"ID": 42, "LoginName": "george@example.com"},
		"CapMap": map[string]any{
			customCapability: []any{map[string]any{"actions": []string{proto.ActionSubmit}}},
		},
	}
	d, err := New(Config{
		StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response),
		Capability: customCapability,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id, err := d.identify("100.64.0.1:1234", testDestination())
	if err != nil || !id.Allowed(proto.ActionSubmit) {
		t.Fatalf("custom capability override failed: %+v, %v", id, err)
	}
}

func TestIdentifyUsesStableNodeIDWithoutUserIdentity(t *testing.T) {
	response := map[string]any{
		"Node":        map[string]any{"Name": "tagged-runner.tailnet.ts.net.", "StableID": "node-42"},
		"UserProfile": map[string]any{},
		"CapMap": map[string]any{
			proto.DefaultCapability: []any{map[string]any{"actions": []string{proto.ActionSubmit}}},
		},
	}
	d, err := New(Config{StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id, err := d.identify("100.64.0.2:1234", testDestination())
	if err != nil {
		t.Fatal(err)
	}
	if got := id.Owner(); got != "tailnet-node:node-42" {
		t.Fatalf("node owner = %q, want stable node ID", got)
	}
}

func TestIdentifyFailsClosedWithoutStableIdentity(t *testing.T) {
	response := map[string]any{
		"Node":        map[string]any{"Name": "laptop.tailnet.ts.net."},
		"UserProfile": map[string]any{"LoginName": "george@example.com"},
		"CapMap": map[string]any{
			proto.DefaultCapability: []any{map[string]any{"actions": []string{proto.ActionSubmit}}},
		},
	}
	d, err := New(Config{StateDir: t.TempDir(), TailscaledSocket: fakeWhoisSocket(t, response)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.identify("100.64.0.2:1234", testDestination()); err == nil {
		t.Fatal("WhoIs response without stable IDs authorized a caller")
	}
}

func TestLookupRejectsDifferentOwner(t *testing.T) {
	id := proto.NewULID()
	d := &Daemon{jobs: map[string]*Job{
		id: {ID: id, Admission: proto.Admission{UserID: 1, UserLogin: "owner@example.com"}},
	}}
	req := httptest.NewRequest(http.MethodGet, "/v0/jobs/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	if got := d.lookup(w, req, Identity{UserID: 2, Login: "other@example.com"}); got != nil {
		t.Fatal("different owner retrieved the job")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("different owner status = %d, want 403", w.Code)
	}
}

func TestOwnerUsesStableTailscaleIdentity(t *testing.T) {
	beforeRename := Identity{UserID: 42, Login: "old@example.com", NodeID: "node-1", Node: "old-name"}
	afterRename := Identity{UserID: 42, Login: "new@example.com", NodeID: "node-1", Node: "new-name"}
	if beforeRename.Owner() != afterRename.Owner() {
		t.Fatalf("user rename changed owner: %q != %q", beforeRename.Owner(), afterRename.Owner())
	}
	if beforeRename.Owner() == (Identity{UserID: 43, Login: "old@example.com"}).Owner() {
		t.Fatal("reused login name preserved ownership across different user IDs")
	}
	if (Identity{NodeID: "node-1", Node: "old-name"}).Owner() !=
		(Identity{NodeID: "node-1", Node: "new-name"}).Owner() {
		t.Fatal("node rename changed ownership")
	}
	if (Identity{NodeID: "node-1", Node: "old-name"}).Owner() ==
		(Identity{NodeID: "node-2", Node: "old-name"}).Owner() {
		t.Fatal("reused node name preserved ownership across different stable IDs")
	}
}

func idempotentSubmitRequest(t *testing.T, jobID string, spec proto.Spec, manifest proto.Manifest) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, err := mw.CreateFormField("spec")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(p).Encode(spec); err != nil {
		t.Fatal(err)
	}
	p, err = mw.CreateFormField("manifest")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(p).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	p, err = mw.CreateFormFile("workspace", "workspace.tar")
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(p)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v0/jobs/"+jobID, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", jobID)
	return req
}

func TestIdempotentSubmitRejectsDifferentOwner(t *testing.T) {
	jobID := proto.NewULID()
	manifest := proto.Manifest{}
	spec := proto.Spec{
		Argv:         []string{"/bin/true"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(), ChangeClientID: testChangeClientID,
	}
	d := &Daemon{
		cfg: Config{MaxUploadBytes: 1 << 20, MaxLimits: proto.DefaultLimits()},
		jobs: map[string]*Job{jobID: {
			ID: jobID, RequestDigest: spec.Digest(), state: proto.StateExited,
			Admission: proto.Admission{UserID: 1, UserLogin: "owner@example.com"},
		}},
	}

	w := httptest.NewRecorder()
	d.handleSubmit(w, idempotentSubmitRequest(t, jobID, spec, manifest), Identity{UserID: 2, Login: "other@example.com"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("different-owner idempotent submit status = %d, want 403", w.Code)
	}
	differentSpec := spec
	differentSpec.Argv = []string{"/bin/false"}
	w = httptest.NewRecorder()
	d.handleSubmit(w, idempotentSubmitRequest(t, jobID, differentSpec, manifest), Identity{UserID: 2, Login: "other@example.com"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("different-owner digest-mismatch status = %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	d.handleSubmit(w, idempotentSubmitRequest(t, jobID, spec, manifest), Identity{UserID: 1, Login: "owner@example.com"})
	if w.Code != http.StatusOK {
		t.Fatalf("owner idempotent submit status = %d, want 200", w.Code)
	}
}

func TestIdempotentSubmitChecksOwnerBeforeUnavailableDigest(t *testing.T) {
	jobID := proto.NewULID()
	manifest := proto.Manifest{}
	spec := proto.Spec{
		Argv: []string{"/bin/true"},
		Env:  map[string]string{"PIN": "0427"}, EnvSources: map[string]string{"PIN": "literal"},
		ManifestRoot: manifest.RootHash(), Limits: proto.DefaultLimits(), ChangeClientID: testChangeClientID,
	}
	d := &Daemon{
		cfg: Config{MaxUploadBytes: 1 << 20, MaxLimits: proto.DefaultLimits()},
		jobs: map[string]*Job{jobID: {
			ID: jobID, state: proto.StateExited,
			Admission: proto.Admission{UserID: 1, UserLogin: "owner@example.com"},
		}},
	}

	w := httptest.NewRecorder()
	d.handleSubmit(w, idempotentSubmitRequest(t, jobID, spec, manifest), Identity{UserID: 2, Login: "other@example.com"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("different-owner unavailable-digest status = %d, want 403", w.Code)
	}
	w = httptest.NewRecorder()
	d.handleSubmit(w, idempotentSubmitRequest(t, jobID, spec, manifest), Identity{UserID: 1, Login: "owner@example.com"})
	if w.Code != http.StatusConflict {
		t.Fatalf("owner unavailable-digest status = %d, want 409", w.Code)
	}
}
