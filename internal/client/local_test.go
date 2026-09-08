package client

import (
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalTransportRoutesAllClientsToTheirSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "errand-client-local-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for _, name := range []string{"first", "second"} {
		socket := filepath.Join(dir, name+".sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(name + ":" + r.RequestURI))
		})}
		go server.Serve(listener)
		t.Cleanup(func() { server.Close() })
		target := "unix://" + hex.EncodeToString([]byte(socket))
		for _, c := range []*http.Client{directHTTP, maintenanceHTTP, forwardHTTP} {
			res, err := c.Get(target + "/v0/info?check=1")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil || string(body) != name+":/v0/info?check=1" {
				t.Fatalf("wrong socket or request: %q %v", body, err)
			}
		}
	}
}
func TestLocalTransportRejectsMalformedSocketIdentity(t *testing.T) {
	for _, authority := range []string{"not-hex", hex.EncodeToString([]byte("relative")), hex.EncodeToString([]byte("/tmp/../socket")), hex.EncodeToString([]byte("/tmp/a\x00b")), "user@2f746d702f73"} {
		if _, err := directHTTP.Get("unix://" + authority + "/v0/info"); err == nil {
			t.Fatalf("accepted %s", authority)
		}
	}
}
