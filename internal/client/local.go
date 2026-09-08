package client

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/unixpeer"
)

func init() {
	for _, outer := range []*http.Transport{directTransport, maintenanceTransport, forwardHTTP.Transport.(*http.Transport)} {
		outer.RegisterProtocol("unix", &localRoundTripper{responseHeaderTimeout: outer.ResponseHeaderTimeout})
	}
}

type localRoundTripper struct {
	responseHeaderTimeout time.Duration
	transports            sync.Map
}

func (rt *localRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	socketBytes, err := hex.DecodeString(req.URL.Host)
	socket := string(socketBytes)
	if err != nil || req.URL.User != nil || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || strings.ContainsRune(socket, 0) {
		return nil, fmt.Errorf("invalid local runner URL; use --on local or a personal peer socket path")
	}
	value, _ := rt.transports.LoadOrStore(socket, &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := unixpeer.Dial(ctx, socket, unixpeer.CurrentUID())
			if err != nil {
				return nil, fmt.Errorf("local runner at %s: %w (check this peer's socket setting and runner service; for the default local runner, use errand setup --local)", socket, err)
			}
			return conn, nil
		},
		ResponseHeaderTimeout: rt.responseHeaderTimeout,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	})
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = "http"
	u.Host = "errand"
	clone.URL = &u
	clone.Host = "errand"
	return value.(*http.Transport).RoundTrip(clone)
}
