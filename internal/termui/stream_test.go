package termui

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockedStreamWriter struct {
	started, release chan struct{}
}

func (w *blockedStreamWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}

func TestIndependentStreamWritesDoNotBlockEachOther(t *testing.T) {
	for _, stdoutBlocked := range []bool{false, true} {
		for _, print := range []bool{false, true} {
			writer := &blockedStreamWriter{started: make(chan struct{}), release: make(chan struct{})}
			var buf bytes.Buffer
			con := Plain(writer, &buf)
			blocked, available := con.Out, con.Err
			if !stdoutBlocked {
				con = Plain(&buf, writer)
				blocked, available = con.Err, con.Out
			}
			writeDone := make(chan struct{})
			go func() {
				if print {
					blocked.Print("blocked")
				} else {
					_, _ = blocked.Write([]byte("blocked"))
				}
				close(writeDone)
			}()
			<-writer.started
			otherDone := make(chan struct{})
			go func() { available.Print("still responsive"); close(otherDone) }()
			responsive := false
			select {
			case <-otherDone:
				responsive = true
			case <-time.After(time.Second):
			}
			close(writer.release)
			<-writeDone
			<-otherDone
			if !responsive || buf.String() != "still responsive\n" {
				t.Fatalf("stdoutBlocked=%v print=%v responsive=%v output=%q", stdoutBlocked, print, responsive, buf.String())
			}
		}
	}
}

func TestSharedStreamWriterSerializesConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	con := Plain(&buf, &buf)
	var wg sync.WaitGroup
	for _, stream := range []*Stream{con.Out, con.Err} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				stream.Print("line")
				_, _ = stream.Write([]byte("raw\n"))
			}
		}()
	}
	wg.Wait()
	if strings.Count(buf.String(), "line\n") != 200 || strings.Count(buf.String(), "raw\n") != 200 {
		t.Fatalf("concurrent writes were corrupted: %q", buf.String())
	}
}

type sliceStreamWriter []byte

func (w sliceStreamWriter) Write(p []byte) (int, error) { return len(p), nil }

type wrappedStreamWriter struct{ io.Writer }

func TestStreamAcceptsNonComparableWriter(t *testing.T) {
	for _, writer := range []io.Writer{sliceStreamWriter{}, wrappedStreamWriter{sliceStreamWriter{}}} {
		con := Plain(writer, writer)
		if n, err := con.Out.Write([]byte("raw")); n != 3 || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
}
