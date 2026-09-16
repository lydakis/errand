//go:build darwin || linux

package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNativeReuseRequiresEveryStampField(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("value"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := Fingerprint(info)
	if err != nil {
		t.Fatal(err)
	}
	if !sameObservation(stamp, stamp) {
		t.Fatal("equal evidence rejected")
	}
	for i := 0; i < reflect.TypeOf(stamp).NumField(); i++ {
		t.Run(reflect.TypeOf(stamp).Field(i).Name, func(t *testing.T) {
			changed := stamp
			field := reflect.ValueOf(&changed).Elem().Field(i)
			if field.CanUint() {
				field.SetUint(field.Uint() + 1)
			} else {
				field.SetInt(field.Int() + 1)
			}
			if sameObservation(stamp, changed) {
				t.Fatal("changed native evidence reused")
			}
		})
	}
}
