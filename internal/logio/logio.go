// Package logio owns io.log: framed stdout/stderr records in
// daemon-observed order, append-only on disk, followable by readers that
// can resume from any sequence number.
package logio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

const maxFrameData = 32 << 10

// Writer serializes interleaved stream writes into sequenced frames.
type Writer struct {
	mu        sync.Mutex
	cond      *sync.Cond
	f         *os.File
	seq       int64
	total     int64
	max       int64
	limitHit  bool
	writeErr  error
	closed    bool
	onFailure func()
}

type IntegrityError struct{ err error }

func (e *IntegrityError) Error() string { return e.err.Error() }
func (e *IntegrityError) Unwrap() error { return e.err }

func IsIntegrityError(err error) bool {
	var integrityErr *IntegrityError
	return errors.As(err, &integrityErr)
}

func integrityError(err error) error { return &IntegrityError{err: err} }

func NewWriter(path string, maxBytes int64, onFailure func()) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, max: maxBytes, onFailure: onFailure}
	w.cond = sync.NewCond(&w.mu)
	return w, nil
}

// StreamWriter returns an io.Writer that frames everything written to it
// under the given stream name. Safe for concurrent use across streams.
func (w *Writer) StreamWriter(stream string) io.Writer {
	return &streamWriter{w: w, stream: stream}
}

type streamWriter struct {
	w      *Writer
	stream string
}

func (s *streamWriter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += maxFrameData {
		end := min(off+maxFrameData, len(p))
		if err := s.w.append(s.stream, p[off:end]); err != nil {
			// The process's pipe must keep draining even past the cap, so
			// report success; the cap handler terminates the job.
			return len(p), nil
		}
	}
	return len(p), nil
}

func (w *Writer) append(stream string, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.limitHit || w.writeErr != nil {
		return io.ErrClosedPipe
	}
	if w.total+int64(len(data)) > w.max {
		w.limitHit = true
		if w.onFailure != nil {
			go w.onFailure()
		}
		return io.ErrClosedPipe
	}
	next := w.seq + 1
	frame := proto.LogFrame{
		Seq:     next,
		Stream:  stream,
		DataB64: base64.StdEncoding.EncodeToString(data),
		TUnixMS: time.Now().UnixMilli(),
	}
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		w.writeErr = err
		if w.onFailure != nil {
			go w.onFailure()
		}
		return err
	}
	w.seq = next
	w.total += int64(len(data))
	w.cond.Broadcast()
	return nil
}

// LimitHit reports whether the log cap terminated collection.
func (w *Writer) LimitHit() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.limitHit
}

// Complete reports whether every accepted log byte was durably handed to the
// log file without hitting the configured cap.
func (w *Writer) Complete() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.limitHit && w.writeErr == nil
}

func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeErr
}

// Close ends the stream; followers drain and stop.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.cond.Broadcast()
	err := w.f.Close()
	if err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	return err
}

// waitChange blocks until seq advances past cur or the writer closes.
func (w *Writer) waitChange(ctx context.Context, cur int64) (closed bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			w.mu.Lock()
			w.cond.Broadcast()
			w.mu.Unlock()
		case <-stopped:
		}
	}()
	for w.seq <= cur && !w.closed {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		w.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return w.closed && w.seq <= cur, nil
}

// Follow reads frames with Seq > from and passes them to fn. When live is
// non-nil it keeps following until the writer closes; otherwise it stops
// at EOF. fn returning an error stops the follow.
func Follow(ctx context.Context, path string, from int64, live *Writer, fn func(proto.LogFrame) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	last := from
	expected := int64(1)
	var buf []byte
	chunk := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := f.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				nl := bytes.IndexByte(buf, '\n')
				if nl < 0 {
					break // partial line stays buffered for the next read
				}
				line := buf[:nl]
				buf = buf[nl+1:]
				var frame proto.LogFrame
				if jerr := json.Unmarshal(line, &frame); jerr != nil {
					return integrityError(jerr)
				}
				if frame.Seq != expected {
					return integrityError(fmt.Errorf("log sequence %d, expected %d", frame.Seq, expected))
				}
				expected++
				if frame.Seq > last {
					last = frame.Seq
					if ferr := fn(frame); ferr != nil {
						return ferr
					}
				}
			}
		}
		if err == io.EOF {
			if live == nil {
				if len(buf) != 0 {
					return integrityError(io.ErrUnexpectedEOF)
				}
				return nil
			}
			closed, err := live.waitChange(ctx, last)
			if err != nil {
				return err
			}
			if closed {
				if len(buf) != 0 {
					return integrityError(io.ErrUnexpectedEOF)
				}
				return nil
			}
			continue // writer appended more; keep reading
		}
		if err != nil {
			return err
		}
	}
}
