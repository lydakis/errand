package proto

import (
	"testing"
	"time"
)

func TestULIDTimestampDecodesGeneratedTime(t *testing.T) {
	before := time.Now().Add(-time.Millisecond)
	id := NewULID()
	after := time.Now().Add(time.Millisecond)
	got, ok := ULIDTimestamp(id)
	if !ok || got.Before(before) || got.After(after) {
		t.Fatalf("ULIDTimestamp(%q) = %v, %v; want time between %v and %v", id, got, ok, before, after)
	}
}

func TestULIDTimestampRejectsOverflowEncoding(t *testing.T) {
	if _, ok := ULIDTimestamp("Z0000000000000000000000000"); ok {
		t.Fatal("ULIDTimestamp accepted a non-canonical 50-bit timestamp")
	}
}
