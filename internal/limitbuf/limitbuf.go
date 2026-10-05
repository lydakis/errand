// Package limitbuf keeps a bounded prefix of output whose size another
// process decides, such as a child's stdout or stderr.
package limitbuf

import "bytes"

// Buffer keeps the first Limit bytes written to it and counts the rest as
// written, so a writer is never blocked or failed by it. It wraps its
// bytes.Buffer rather than embedding it: an embedded buffer's ReadFrom would
// let io.Copy, and so exec.Cmd, write past Limit.
type Buffer struct {
	Limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *Buffer) Write(p []byte) (int, error) {
	room := b.Limit - b.buf.Len()
	if room < len(p) {
		b.truncated = true
	}
	if room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// Truncated reports whether anything was dropped.
func (b *Buffer) Truncated() bool { return b.truncated }

func (b *Buffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *Buffer) String() string { return b.buf.String() }
func (b *Buffer) Len() int       { return b.buf.Len() }
