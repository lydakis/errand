package logio

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

// writeFrames writes one frame per chunk, alternating streams by a "!" prefix
// (stderr), with t_unix_ms equal to its index times 1000.
func writeFrames(t *testing.T, chunks ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "io.log")
	var b strings.Builder
	for i, chunk := range chunks {
		stream := "stdout"
		if strings.HasPrefix(chunk, "!") {
			stream, chunk = "stderr", chunk[1:]
		}
		line, _ := json.Marshal(proto.LogFrame{
			Seq: int64(i + 1), Stream: stream,
			DataB64: base64.StdEncoding.EncodeToString([]byte(chunk)), TUnixMS: int64(i) * 1000,
		})
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// replay returns what a client sees from w: frames Start..Last with the cut
// applied to the first.
func replay(t *testing.T, chunks []string, w Window) string {
	t.Helper()
	var out strings.Builder
	for seq := w.Start; seq <= w.Last; seq++ {
		chunk := strings.TrimPrefix(chunks[seq-1], "!")
		if seq == w.Start {
			chunk = chunk[w.Cut:]
		}
		out.WriteString(chunk)
	}
	return out.String()
}

func TestFindWindowTailsLinesAcrossFramesAndStreams(t *testing.T) {
	chunks := []string{"one\ntwo\n", "thr", "ee\n", "!oops\n", "four\nfive"}
	path := writeFrames(t, chunks...)
	for _, tc := range []struct {
		tail  int
		since int64
		want  string
	}{
		{tail: -1, want: "one\ntwo\nthree\noops\nfour\nfive"},
		{tail: 0, want: ""},
		{tail: 1, want: "five"},
		{tail: 2, want: "four\nfive"},
		{tail: 3, want: "oops\nfour\nfive"},
		{tail: 4, want: "three\noops\nfour\nfive"},
		{tail: 5, want: "two\nthree\noops\nfour\nfive"},
		{tail: 100, want: "one\ntwo\nthree\noops\nfour\nfive"},
		{tail: -1, since: 3000, want: "oops\nfour\nfive"},
		{tail: 1, since: 3000, want: "five"},
		{tail: -1, since: 9000, want: ""},
	} {
		w, err := FindWindow(path, tc.since, tc.tail)
		if err != nil {
			t.Fatal(err)
		}
		if w.Last != 5 {
			t.Fatalf("tail %d since %d: last = %d, want 5", tc.tail, tc.since, w.Last)
		}
		if got := replay(t, chunks, w); got != tc.want {
			t.Errorf("tail %d since %d replays %q, want %q", tc.tail, tc.since, got, tc.want)
		}
	}
}

func TestFindWindowCountsAFinalNewlineAsTheEndOfALine(t *testing.T) {
	chunks := []string{"a\n", "b\nc\n"}
	path := writeFrames(t, chunks...)
	for tail, want := range map[int]string{1: "c\n", 2: "b\nc\n", 3: "a\nb\nc\n"} {
		w, err := FindWindow(path, 0, tail)
		if err != nil {
			t.Fatal(err)
		}
		if got := replay(t, chunks, w); got != want {
			t.Errorf("tail %d replays %q, want %q", tail, got, want)
		}
	}
}

func TestFindWindowLeavesOutARecordStillBeingWritten(t *testing.T) {
	path := writeFrames(t, "done\n")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":2,"stream":"stdout","data_b64":"`)
	f.Close()
	w, err := FindWindow(path, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if w.Start != 1 || w.Cut != 0 || w.Last != 1 {
		t.Fatalf("window = %+v, want frame 1 only", w)
	}
}

func TestTrimFrameDropsLeadingBytes(t *testing.T) {
	frame := proto.LogFrame{Seq: 4, DataB64: base64.StdEncoding.EncodeToString([]byte("abc\ndef"))}
	got, err := TrimFrame(frame, 4)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(got.DataB64)
	if string(data) != "def" || got.Seq != 4 {
		t.Fatalf("trimmed frame = %+v (%q)", got, data)
	}
	if _, err := TrimFrame(frame, 8); !IsIntegrityError(err) {
		t.Fatalf("over-long cut error = %v, want an integrity error", err)
	}
}
