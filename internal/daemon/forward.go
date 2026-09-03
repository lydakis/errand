package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// JobEndpoint reaches services exposed by one running job. Host jobs share
// the runner's loopback namespace; isolated backends can replace this adapter.
type JobEndpoint interface {
	DialTCP(context.Context, uint16) (net.Conn, error)
}

type hostJobEndpoint struct{}

var errInvalidForwardLifecycle = errors.New("job forwarding lifecycle is unavailable")

func (hostJobEndpoint) DialTCP(ctx context.Context, port uint16) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	service := strconv.FormatUint(uint64(port), 10)
	ipv4, ipv4Err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", service))
	if ipv4Err == nil {
		return ipv4, nil
	}
	ipv6, ipv6Err := dialer.DialContext(ctx, "tcp6", net.JoinHostPort("::1", service))
	if ipv6Err == nil {
		return ipv6, nil
	}
	return nil, errors.Join(ipv4Err, ipv6Err)
}

func (d *Daemon) handleForward(w http.ResponseWriter, r *http.Request, id Identity) {
	portValue, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || portValue == 0 {
		httpError(w, http.StatusBadRequest, "forward port must be between 1 and 65535")
		return
	}
	j := d.lookup(w, r, id)
	if j == nil {
		return
	}
	if !j.acquireForwardReader() {
		httpError(w, http.StatusNotFound, "no such job")
		return
	}
	defer j.releaseForwardReader()

	j.mu.Lock()
	state := j.state
	endpoint := j.endpoint
	executionDone := j.executionDone
	j.mu.Unlock()
	if endpoint == nil || executionDone == nil {
		httpError(w, http.StatusInternalServerError, errInvalidForwardLifecycle.Error())
		return
	}
	if state != proto.StateRunning {
		httpError(w, http.StatusConflict, "job is not running")
		return
	}
	select {
	case <-executionDone:
		httpError(w, http.StatusConflict, "job is no longer running")
		return
	default:
	}
	controller := http.NewResponseController(w)
	if err := controller.EnableFullDuplex(); err != nil {
		httpError(w, http.StatusInternalServerError, "full-duplex streaming unsupported")
		return
	}
	if err := controller.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		httpError(w, http.StatusInternalServerError, "clearing request read deadline: "+err.Error())
		return
	}
	tunnelCtx, cancelTunnel := context.WithCancel(r.Context())
	defer cancelTunnel()
	go func() {
		select {
		case <-executionDone:
			cancelTunnel()
		case <-tunnelCtx.Done():
		}
	}()
	upstream, err := endpoint.DialTCP(tunnelCtx, uint16(portValue))
	if err != nil {
		if r.Context().Err() == nil {
			select {
			case <-executionDone:
				httpError(w, http.StatusConflict, "job is no longer running")
			default:
				httpError(w, http.StatusBadGateway, "connecting to job port: "+err.Error())
			}
		}
		return
	}
	defer upstream.Close()
	stopClose := context.AfterFunc(tunnelCtx, func() { _ = upstream.Close() })
	defer stopClose()
	select {
	case <-executionDone:
		httpError(w, http.StatusConflict, "job is no longer running")
		return
	default:
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		return
	}

	go func() {
		_, _ = io.Copy(upstream, r.Body)
		cancelTunnel()
	}()
	_, _ = io.Copy(flushingResponseWriter{writer: w, controller: controller}, upstream)
	cancelTunnel()
}

type flushingResponseWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w flushingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil {
		return n, err
	}
	if err := w.controller.Flush(); err != nil {
		return n, err
	}
	return n, nil
}
