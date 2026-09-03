package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
)

// PortForward maps one local loopback TCP port to a port reachable from the
// running job's network namespace.
type PortForward struct {
	Local  uint16
	Remote uint16
}

var forwardHTTP = &http.Client{
	Transport: func() *http.Transport {
		t := directTransport.Clone()
		t.ResponseHeaderTimeout = 0
		t.DisableCompression = true
		return t
	}(),
	CheckRedirect: directHTTP.CheckRedirect,
}

type forwardListener struct {
	mapping  PortForward
	listener net.Listener
}

type forwardSession struct {
	ctx       context.Context
	cancel    context.CancelFunc
	forwards  []PortForward
	listeners []forwardListener
	stderr    io.Writer

	mu          sync.Mutex
	outputMu    sync.Mutex
	connections map[net.Conn]struct{}
	started     bool
	closed      bool
	wg          sync.WaitGroup
}

func bindPortForwards(forwards []PortForward, stderr io.Writer) (*forwardSession, error) {
	if len(forwards) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &forwardSession{
		ctx: ctx, cancel: cancel, forwards: append([]PortForward(nil), forwards...),
		stderr: stderr, connections: map[net.Conn]struct{}{},
	}
	for _, mapping := range forwards {
		if mapping.Local == 0 || mapping.Remote == 0 {
			session.Close()
			return nil, fmt.Errorf("forward ports must be between 1 and 65535")
		}
		service := strconv.FormatUint(uint64(mapping.Local), 10)
		addresses := []struct {
			network string
			host    string
		}{
			{network: "tcp4", host: "127.0.0.1"},
			{network: "tcp6", host: "::1"},
		}
		for _, address := range addresses {
			listenAddress := net.JoinHostPort(address.host, service)
			listener, err := net.Listen(address.network, listenAddress)
			if err != nil {
				if address.network == "tcp6" && ipv6Unavailable(err) {
					continue
				}
				session.Close()
				return nil, fmt.Errorf("binding forward %s: %w", listenAddress, err)
			}
			session.listeners = append(session.listeners, forwardListener{mapping: mapping, listener: listener})
		}
	}
	return session, nil
}

func ipv6Unavailable(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT) ||
		errors.Is(err, syscall.EADDRNOTAVAIL) ||
		errors.Is(err, syscall.ENETUNREACH)
}

func (s *forwardSession) Start(peerURL, jobID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return
	}
	s.started = true
	forwards := append([]PortForward(nil), s.forwards...)
	listeners := append([]forwardListener(nil), s.listeners...)
	s.mu.Unlock()
	for _, mapping := range forwards {
		s.writef("errand: forwarding localhost:%d to job port %d\n", mapping.Local, mapping.Remote)
	}
	for _, bound := range listeners {
		s.wg.Add(1)
		go s.accept(peerURL, jobID, bound)
	}
}

func (s *forwardSession) accept(peerURL, jobID string, bound forwardListener) {
	defer s.wg.Done()
	for {
		connection, err := bound.listener.Accept()
		if err != nil {
			if s.ctx.Err() == nil {
				s.writef("errand: accepting forward on %s: %v\n", bound.listener.Addr(), err)
			}
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = connection.Close()
			return
		}
		s.connections[connection] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.forward(peerURL, jobID, bound.mapping, connection)
	}
}

func (s *forwardSession) forward(peerURL, jobID string, mapping PortForward, local net.Conn) {
	defer s.wg.Done()
	defer func() {
		_ = local.Close()
		s.mu.Lock()
		delete(s.connections, local)
		s.mu.Unlock()
	}()

	connectionCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	requestReader, requestWriter := io.Pipe()
	uploadDone := make(chan struct{})
	defer func() {
		cancel()
		_ = local.Close()
		_ = requestReader.CloseWithError(context.Canceled)
		<-uploadDone
	}()
	requestURL := fmt.Sprintf("%s/v0/jobs/%s/ports/%d/connect", peerURL, jobID, mapping.Remote)
	request, err := http.NewRequestWithContext(connectionCtx, http.MethodPost, requestURL, requestReader)
	if err != nil {
		close(uploadDone)
		s.report(mapping, err)
		return
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Close = true
	go func() {
		_, copyErr := io.Copy(requestWriter, local)
		if copyErr == nil {
			copyErr = context.Canceled
		}
		_ = requestWriter.CloseWithError(copyErr)
		cancel()
		close(uploadDone)
	}()

	response, err := forwardHTTP.Do(request)
	if err != nil {
		if connectionCtx.Err() == nil {
			s.report(mapping, err)
		}
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if response.StatusCode != http.StatusBadGateway && response.StatusCode != http.StatusConflict {
			s.report(mapping, fmt.Errorf("runner returned %s: %s", response.Status, apiError(body)))
		}
		return
	}
	_, copyErr := io.Copy(local, response.Body)
	if copyErr != nil && !errors.Is(copyErr, net.ErrClosed) && connectionCtx.Err() == nil {
		s.report(mapping, copyErr)
	}
}

func (s *forwardSession) report(mapping PortForward, err error) {
	s.writef("errand: forward localhost:%d to job port %d failed: %v\n",
		mapping.Local, mapping.Remote, err)
}

func (s *forwardSession) writef(format string, args ...any) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	fmt.Fprintf(s.stderr, format, args...)
}

func (s *forwardSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	listeners := append([]forwardListener(nil), s.listeners...)
	connections := make([]net.Conn, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.mu.Unlock()
	for _, bound := range listeners {
		_ = bound.listener.Close()
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
	s.wg.Wait()
}
