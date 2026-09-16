package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSourceMutationClassification(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "value")
	if err := os.WriteFile(name, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Build(root, []string{"value"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("changed contents"), 0600); err != nil {
		t.Fatal(err)
	}
	_, hashErr := hashFileSized(name, 7, 0600)
	packErr := Pack(new(bytes.Buffer), root, m)
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	missingErr := Pack(new(bytes.Buffer), root, m)
	for _, err := range []error{hashErr, packErr, missingErr, errors.Join(hashErr, packErr)} {
		if !IsSourceChanged(err) {
			t.Fatalf("mutation not classified: %v", err)
		}
	}
	for _, cause := range []error{os.ErrPermission, syscall.ENOSPC, syscall.EIO, errors.New("unknown failure")} {
		if IsSourceChanged(sourceReadError(cause)) || IsSourceChanged(errors.Join(hashErr, fmt.Errorf("copying: %w", cause))) {
			t.Fatalf("storage/unknown failure hidden as mutation: %v", cause)
		}
	}
}
