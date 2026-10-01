package termui

import (
	"io"
	"testing"
)

type streamDiscard struct{}

func (*streamDiscard) Write(p []byte) (int, error) { return len(p), nil }

func BenchmarkIndependentStreamWrite(b *testing.B) {
	con := Plain(&streamDiscard{}, io.Discard)
	data := make([]byte, 32<<10)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = con.Out.Write(data)
	}
}
