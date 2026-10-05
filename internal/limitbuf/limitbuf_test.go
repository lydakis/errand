package limitbuf

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// A child that prints more than the limit is cut off at the limit even
// though exec copies its output with io.Copy.
func TestBufferBoundsChildOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	out := &Buffer{Limit: 10}
	cmd := exec.Command("/bin/sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x")
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if out.String() != strings.Repeat("x", 10) || !out.Truncated() {
		t.Fatalf("kept %d bytes, truncated %v", out.Len(), out.Truncated())
	}
}

func TestBufferKeepsShortOutput(t *testing.T) {
	b := &Buffer{Limit: 10}
	if n, err := b.Write([]byte("hello")); n != 5 || err != nil || b.String() != "hello" || b.Truncated() {
		t.Fatalf("%d %v %q %v", n, err, b.String(), b.Truncated())
	}
	if n, _ := b.Write([]byte(" world")); n != 6 || b.String() != "hello worl" || !b.Truncated() {
		t.Fatalf("%d %q %v", n, b.String(), b.Truncated())
	}
}
