package client

import (
	"io"
	"sync/atomic"
	"time"
)

// TransferStats describes this invocation, not a saved transfer receipt.
// ChangedPaths counts change roots, which may be files or whole directories.
// TransferredBytes counts multipart transfer bodies, including archive framing
// and metadata, but excludes HTTP headers and control requests. Reused staging
// contributes zero bytes. ElapsedMillis includes local preparation and apply.
type TransferStats struct {
	ChangedPaths     int   `json:"changed_paths"`
	TransferredBytes int64 `json:"transferred_bytes"`
	ElapsedMillis    int64 `json:"elapsed_ms"`
}

type transferMeter struct {
	stats   *TransferStats
	started time.Time
	bytes   atomic.Int64
}

func startTransfer(stats *TransferStats) *transferMeter {
	if stats == nil {
		return nil
	}
	*stats = TransferStats{}
	return &transferMeter{stats: stats, started: time.Now()}
}

func (m *transferMeter) finish() {
	if m != nil && m.stats != nil {
		m.stats.TransferredBytes = m.bytes.Load()
		m.stats.ElapsedMillis = time.Since(m.started).Milliseconds()
	}
}

func (m *transferMeter) paths(paths []string, selected map[string]bool) {
	if m == nil || m.stats == nil {
		return
	}
	m.stats.ChangedPaths = 0
	for _, path := range paths {
		if selected == nil || selected[path] {
			m.stats.ChangedPaths++
		}
	}
}

func (m *transferMeter) reader(r io.Reader) io.Reader {
	if m == nil {
		return r
	}
	return transferReader{Reader: r, bytes: &m.bytes}
}

func (m *transferMeter) readCloser(r io.ReadCloser) io.ReadCloser {
	if m == nil {
		return r
	}
	return struct {
		io.Reader
		io.Closer
	}{m.reader(r), r}
}

type transferReader struct {
	io.Reader
	bytes *atomic.Int64
}

func (r transferReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}
