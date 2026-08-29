package logio

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestFramingAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.log")
	w, err := NewWriter(path, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := w.StreamWriter("stdout")
	errw := w.StreamWriter("stderr")
	out.Write([]byte("one"))
	errw.Write([]byte("two"))
	out.Write([]byte("three"))
	w.Close()

	var seqs []int64
	if err := Follow(context.Background(), path, 1, nil, func(f proto.LogFrame) error {
		seqs = append(seqs, f.Seq)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 2 || seqs[0] != 2 || seqs[1] != 3 {
		t.Fatalf("resume from seq 1 gave %v, want [2 3]", seqs)
	}
}

func TestLiveFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.log")
	w, err := NewWriter(path, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var got []string
	go func() {
		defer wg.Done()
		Follow(context.Background(), path, 0, w, func(f proto.LogFrame) error {
			got = append(got, f.Stream)
			return nil
		})
	}()
	out := w.StreamWriter("stdout")
	out.Write([]byte("a"))
	time.Sleep(50 * time.Millisecond)
	out.Write([]byte("b"))
	w.Close()
	wg.Wait()
	if len(got) != 2 {
		t.Fatalf("live follower saw %d frames, want 2", len(got))
	}
}

func TestLiveFollowStopsWhenContextIsCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.log")
	w, err := NewWriter(path, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Follow(ctx, path, 0, w, func(proto.LogFrame) error { return nil })
	}()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("follow cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("follow did not stop after context cancellation")
	}
}

func TestTerminalFollowRejectsPartialFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.log")
	if err := os.WriteFile(path, []byte(`{"seq":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Follow(context.Background(), path, 0, nil, func(proto.LogFrame) error { return nil })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial terminal frame error = %v, want unexpected EOF", err)
	}
	if !IsIntegrityError(err) {
		t.Fatalf("partial terminal frame was not classified as an integrity error: %v", err)
	}
}

func TestFollowRejectsSequenceGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.log")
	if err := os.WriteFile(path, []byte("{\"seq\":2,\"stream\":\"stdout\",\"data_b64\":\"eA==\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Follow(context.Background(), path, 0, nil, func(proto.LogFrame) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "expected 1") {
		t.Fatalf("sequence gap error = %v", err)
	}
	if !IsIntegrityError(err) {
		t.Fatalf("sequence gap was not classified as an integrity error: %v", err)
	}
}

func TestLogCapTerminates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.log")
	hit := make(chan struct{})
	var once sync.Once
	w, err := NewWriter(path, 10, func() { once.Do(func() { close(hit) }) })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	out := w.StreamWriter("stdout")
	out.Write([]byte("0123456789ABCDEF")) // over the 10-byte cap
	select {
	case <-hit:
	case <-time.After(time.Second):
		t.Fatal("log cap did not trigger the limit callback")
	}
	if !w.LimitHit() {
		t.Fatal("LimitHit not recorded")
	}
	if w.Complete() {
		t.Fatal("writer that hit the log cap reported complete logs")
	}
}

func TestLogWriteFailureTriggersFailureCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "io.log")
	hit := make(chan struct{})
	var once sync.Once
	w, err := NewWriter(path, 1<<20, func() { once.Do(func() { close(hit) }) })
	if err != nil {
		t.Fatal(err)
	}
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}
	w.StreamWriter("stdout").Write([]byte("lost"))
	select {
	case <-hit:
	case <-time.After(time.Second):
		t.Fatal("log write failure did not trigger failure callback")
	}
}
