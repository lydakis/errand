package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const sshScheme = "ssh"

func init() {
	for _, t := range []*http.Transport{
		directTransport,
		maintenanceTransport,
		forwardHTTP.Transport.(*http.Transport),
	} {
		registerSSHProtocol(t)
	}
}

func registerSSHProtocol(outer *http.Transport) {
	outer.RegisterProtocol(sshScheme, &sshRoundTripper{
		responseHeaderTimeout: outer.ResponseHeaderTimeout,
	})
}

type sshRoundTripper struct {
	responseHeaderTimeout time.Duration
	transports            sync.Map
}

type sshTransportKey struct {
	target  string
	command string
	socket  string
}

func (rt *sshRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	endpoint := sshEndpointForRequest(req.URL)
	key := sshTransportKey{target: endpoint.target, command: endpoint.command, socket: endpoint.socket}
	value, _ := rt.transports.LoadOrStore(key, &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialSSHConnection(ctx, endpoint.target, sshRemoteInvocation(endpoint))
		},
		ResponseHeaderTimeout: rt.responseHeaderTimeout,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	})
	inner := value.(*http.Transport)

	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = "http"
	u.User = nil
	u.Host = "errand"
	clone.URL = &u
	clone.Host = "errand"
	return inner.RoundTrip(clone)
}

var dialSSHConnection = dialSSH

type sshEndpoint struct {
	target  string
	command string
	socket  string
}

var sshEndpoints sync.Map

// ConfigureSSHPeer gives one configured peer an immutable transport identity.
// This keeps aliases that share an SSH host from overwriting each other's
// remote command or daemon socket.
func ConfigureSSHPeer(peerURL, identity, command, socket string) string {
	target, ok := sshTarget(peerURL)
	if !ok {
		return peerURL
	}
	command = effectiveSSHCommand(command)
	sum := sha256.Sum256([]byte(identity + "\x00" + target))
	configuredURL := fmt.Sprintf("ssh://peer-%x.%x.errand", sum[:16], sum[16:])
	sshEndpoints.Store(configuredURL, sshEndpoint{target: target, command: command, socket: socket})
	return configuredURL
}

func restoreSSHPeer(peerURL, target, command, socket string) {
	if !IsSSHPeer(peerURL) || target == "" {
		return
	}
	sshEndpoints.Store(strings.TrimSuffix(peerURL, "/"), sshEndpoint{
		target: target, command: effectiveSSHCommand(command), socket: socket,
	})
}

func sshEndpointForPeer(peerURL string) sshEndpoint {
	key := strings.TrimSuffix(peerURL, "/")
	if value, ok := sshEndpoints.Load(key); ok {
		return value.(sshEndpoint)
	}
	target, ok := sshTarget(key)
	if !ok {
		return sshEndpoint{}
	}
	return sshEndpoint{target: target, command: effectiveSSHCommand("")}
}

func sshEndpointForRequest(u *url.URL) sshEndpoint {
	base := u.Scheme + "://"
	if u.User != nil {
		base += u.User.String() + "@"
	}
	return sshEndpointForPeer(base + u.Host)
}

func sshTarget(peerURL string) (string, bool) {
	u, err := url.Parse(peerURL)
	if err != nil || u.Scheme != sshScheme || u.Host == "" {
		return "", false
	}
	target := u.Host
	if u.User != nil {
		target = u.User.String() + "@" + target
	}
	return target, true
}

func effectiveSSHCommand(command string) string {
	if command != "" {
		return command
	}
	if v := os.Getenv("ERRAND_SSH_COMMAND"); v != "" {
		return v
	}
	return "errand"
}

func sshRemoteInvocation(endpoint sshEndpoint) string {
	command := shellQuote(endpoint.command) + " _stdio"
	if endpoint.socket != "" {
		command += " --socket " + shellQuote(endpoint.socket)
	}
	return command
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func dialSSH(ctx context.Context, target, remoteInvocation string) (net.Conn, error) {
	controlDir, err := sshControlDir()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-T",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + filepath.Join(controlDir, "%C"),
		"-o", "ControlPersist=60s",
		"-o", "ServerAliveInterval=30",
		"--", target, remoteInvocation,
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stderr = os.Stderr // ssh prompts and host-key warnings stay visible
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stdout = stdoutWriter
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stdoutWriter.Close()
		return nil, fmt.Errorf("starting ssh to %s: %w", target, err)
	}
	// Cmd.Wait owns and closes pipes created by StdoutPipe, which can discard
	// unread buffered output. Keep the read side ourselves and close only the
	// parent's writer; the child closes its inherited writer when it exits.
	stdoutWriter.Close()
	conn := &stdioConn{cmd: cmd, r: stdout, w: stdin, host: target}
	go func() {
		err := cmd.Wait()
		conn.exited(err)
	}()
	return conn, nil
}

func sshControlDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", err
		}
		base = filepath.Join(home, ".cache")
	}
	dir := filepath.Join(base, "errand", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

type stdioConn struct {
	cmd  *exec.Cmd
	r    io.ReadCloser
	w    io.WriteCloser
	host string

	mu      sync.Mutex
	closed  bool
	exitErr error
	done    bool
}

func (c *stdioConn) exited(err error) {
	c.mu.Lock()
	c.done = true
	c.exitErr = err
	c.mu.Unlock()
}

func (c *stdioConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && n == 0 {
		c.mu.Lock()
		exit := c.exitErr
		done := c.done
		c.mu.Unlock()
		if done && exit != nil {
			return 0, fmt.Errorf("ssh to %s ended: %w", c.host, exit)
		}
	}
	return n, err
}

func (c *stdioConn) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil {
		c.mu.Lock()
		exit := c.exitErr
		c.mu.Unlock()
		if exit != nil {
			return n, fmt.Errorf("ssh to %s ended: %w", c.host, exit)
		}
	}
	return n, err
}

func (c *stdioConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	werr := c.w.Close()
	rerr := c.r.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return errors.Join(werr, rerr)
}

type sshAddr struct{ host string }

func (a sshAddr) Network() string { return "ssh" }
func (a sshAddr) String() string  { return a.host }

func (c *stdioConn) LocalAddr() net.Addr              { return sshAddr{host: "local"} }
func (c *stdioConn) RemoteAddr() net.Addr             { return sshAddr{host: c.host} }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

// IsSSHPeer reports whether a peer URL uses the SSH transport.
func IsSSHPeer(peerURL string) bool {
	return strings.HasPrefix(peerURL, sshScheme+"://")
}
