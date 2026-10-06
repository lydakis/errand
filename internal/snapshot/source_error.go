package snapshot

import (
	"errors"
	"fmt"
	"os"
)

// ErrSourceChanged identifies a source that changed while it was being read.
// Use IsSourceChanged when joined errors may also contain a storage failure.
var ErrSourceChanged = errors.New("snapshot source changed")

// file names a regular file whose bytes, size or mode changed while it was
// read, when that is the whole change. Structural changes leave it empty.
type sourceChangedError struct {
	error
	file string
}

func (e *sourceChangedError) Unwrap() error        { return e.error }
func (e *sourceChangedError) Is(target error) bool { return target == ErrSourceChanged }

func sourceChangedf(format string, args ...any) error {
	return &sourceChangedError{error: fmt.Errorf(format, args...)}
}

// FileChanged reports that the regular file at the native path changed while
// it was read; err describes the observation.
func FileChanged(path string, err error) error {
	return &sourceChangedError{error: err, file: path}
}

func fileChangedf(path, format string, args ...any) error {
	return FileChanged(path, fmt.Errorf(format, args...))
}

// A previously selected or observed source path disappearing is a mutation.
// Permission, storage, and other read failures retain their original category.
func sourceReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return &sourceChangedError{error: err}
	}
	return err
}

// ChangedFiles returns the native paths of the files whose contents changed
// when every cause of err is such a change. Rereading those files is then
// enough; any other mutation, or none, needs full source reconciliation.
func ChangedFiles(err error) ([]string, bool) {
	if err == nil {
		return nil, false
	}
	switch e := err.(type) {
	case *sourceChangedError:
		return []string{e.file}, e.file != ""
	case interface{ Unwrap() []error }:
		causes := e.Unwrap()
		var files []string
		for _, cause := range causes {
			changed, ok := ChangedFiles(cause)
			if !ok {
				return nil, false
			}
			files = append(files, changed...)
		}
		return files, len(files) != 0
	case interface{ Unwrap() error }:
		return ChangedFiles(e.Unwrap())
	}
	return nil, false
}

// IsSourceChanged requires every cause to describe source mutation. Copying a
// source can join a mutation with a destination failure; the latter must not
// disappear into an unlimited resampling loop.
func IsSourceChanged(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrSourceChanged {
		return true
	}
	switch e := err.(type) {
	case *sourceChangedError:
		return true
	case interface{ Unwrap() []error }:
		causes := e.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !IsSourceChanged(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return IsSourceChanged(e.Unwrap())
	}
	return false
}
