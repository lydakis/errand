//go:build windows

package fsidentity

import (
	"fmt"
	"os"
	"reflect"
)

// FromInfo returns the volume serial number and file index that os.SameFile
// compares. The os package keeps them unexported, so they are read by name
// after os.SameFile has loaded them.
//
// FileInfo from File.Stat or an os.Root lookup already holds the identity
// captured from its handle. FileInfo from os.Lstat or os.Stat loads it from
// the path on first use, as os.SameFile does.
func FromInfo(info os.FileInfo) (Identity, error) {
	if info == nil || !os.SameFile(info, info) {
		return Identity{}, fmt.Errorf("filesystem identity is unavailable for %q", nameOf(info))
	}
	value := reflect.ValueOf(info)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return Identity{}, fmt.Errorf("filesystem identity is unavailable for %q", info.Name())
	}
	stat := value.Elem()
	volume, volumeOK := uint32Field(stat, "vol")
	high, highOK := uint32Field(stat, "idxhi")
	low, lowOK := uint32Field(stat, "idxlo")
	if !volumeOK || !highOK || !lowOK {
		return Identity{}, fmt.Errorf("filesystem identity is unavailable for %q", info.Name())
	}
	return Identity{Device: uint64(volume), Inode: uint64(high)<<32 | uint64(low)}, nil
}

func uint32Field(value reflect.Value, name string) (uint32, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() || field.Kind() != reflect.Uint32 {
		return 0, false
	}
	return uint32(field.Uint()), true
}

func nameOf(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return info.Name()
}
