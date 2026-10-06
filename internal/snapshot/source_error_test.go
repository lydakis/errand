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

func TestChangedFilesNamesOnlyFileContentChanges(t *testing.T) {
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
	for _, err := range []error{hashErr, packErr, fmt.Errorf("preparing: %w", packErr), errors.Join(hashErr, packErr)} {
		files, ok := ChangedFiles(err)
		if !ok || len(files) == 0 {
			t.Fatalf("content change not attributed to its file: %v", err)
		}
		for _, file := range files {
			if file != name {
				t.Fatalf("changed file %q, want %q", file, name)
			}
		}
	}

	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	missingErr := Pack(new(bytes.Buffer), root, m)
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	replacedErr := Pack(new(bytes.Buffer), root, m)
	policyErr := sourceChangedf("snapshot: selection policy changed after manifest construction; retry")
	for _, err := range []error{nil, ErrSourceChanged, missingErr, replacedErr, policyErr,
		errors.Join(packErr, policyErr), errors.Join(packErr, syscall.ENOSPC)} {
		if files, ok := ChangedFiles(err); ok {
			t.Fatalf("%v reported as changed files %q; it needs full reconciliation", err, files)
		}
	}
}
