package snapshot

import (
	"errors"
	"fmt"
	"os"
)

// ErrSourceChanged identifies a source that changed while it was being read.
// Use IsSourceChanged when joined errors may also contain a storage failure.
var ErrSourceChanged = errors.New("snapshot source changed")

type sourceChangedError struct{ error }

func (e *sourceChangedError) Unwrap() error        { return e.error }
func (e *sourceChangedError) Is(target error) bool { return target == ErrSourceChanged }

func sourceChangedf(format string, args ...any) error {
	return &sourceChangedError{fmt.Errorf(format, args...)}
}

// A previously selected or observed source path disappearing is a mutation.
// Permission, storage, and other read failures retain their original category.
func sourceReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return &sourceChangedError{err}
	}
	return err
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
