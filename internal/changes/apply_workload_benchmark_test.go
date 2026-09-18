package changes

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/lydakis/errand/internal/snapshot"
)

// Each sample is one complete apply including receipt and cleanup. Fixture
// population and historical-retry verification are outside the timer.
func BenchmarkApplyWorkloads(b *testing.B) {
	for _, name := range []string{"tiny", "large", "flat128", "parents8", "parents128", "creation", "deletion", "new-parent", "restricted"} {
		b.Run(name, func(b *testing.B) { benchmarkApplyWorkload(b, name) })
	}
}

func benchmarkApplyWorkload(b *testing.B, name string) {
	if b.N != 1 {
		b.Fatal("use -benchtime=1x for individual observations")
	}
	count, size := 128, 4096
	if name == "tiny" || name == "large" {
		count = 1
	}
	if name == "large" {
		size = 1 << 20
	}
	staged, dest, state := b.TempDir(), b.TempDir(), b.TempDir()
	base, remote := filepath.Join(staged, "base"), filepath.Join(staged, "remote")
	for _, dir := range []string{base, remote} {
		if err := os.Mkdir(dir, 0700); err != nil {
			b.Fatal(err)
		}
	}
	before := bytes.Repeat([]byte("a"), size)
	after := append([]byte(nil), before...)
	after[size/2] = 'b'
	paths := make([]string, count)
	for i := range count {
		path := fmt.Sprintf("file-%03d", i)
		if name == "parents8" || name == "parents128" {
			parents := 8
			if name == "parents128" {
				parents = 128
			}
			path = fmt.Sprintf("parent-%03d/%s", i%parents, path)
		}
		if name == "new-parent" && i == count-1 {
			path = "new/" + path
		}
		paths[i] = path
		for _, dir := range []string{base, remote, dest} {
			if i == count-1 && ((name == "creation" || name == "new-parent") && dir != remote || name == "deletion" && dir == remote) {
				continue
			}
			file := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				b.Fatal(err)
			}
			body := before
			if dir == remote {
				body = after
			}
			f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				b.Fatal(err)
			}
			_, writeErr := f.Write(body)
			syncErr := syncStagedData(f)
			closeErr := f.Close()
			for _, err := range []error{writeErr, syncErr, closeErr} {
				if err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	selectPaths := func(root string) []string {
		var selected []string
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if err := syncDirectory(p); err != nil {
					return err
				}
			}
			if p != root {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				selected = append(selected, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		sort.Strings(selected)
		return selected
	}
	bm, err := snapshot.Build(base, selectPaths(base))
	if err != nil {
		b.Fatal(err)
	}
	rm, err := snapshot.Build(remote, selectPaths(remote))
	if err != nil {
		b.Fatal(err)
	}
	selectPaths(dest)
	for _, dir := range []string{staged, state} {
		if err := syncDirectory(dir); err != nil {
			b.Fatal(err)
		}
	}
	if name == "restricted" {
		for i := range rm.Entries {
			if rm.Entries[i].Path == paths[count-1] {
				rm.Entries[i].Mode = 0
			}
		}
		if err := os.Chmod(filepath.Join(remote, paths[count-1]), 0); err != nil {
			b.Fatal(err)
		}
	}
	bundle, err := PrepareSourceDelta(b.Context(), bm, rm, 4<<20)
	if err != nil {
		b.Fatal(err)
	}
	if len(bundle.Paths) != count {
		b.Fatalf("roots=%v", bundle.Paths)
	}
	id, err := applyWorkspaceIdentity(dest)
	if err != nil {
		b.Fatal(err)
	}
	target := TransferTarget{Root: dest, RootID: id, Owner: "apply-workload", StatePath: filepath.Join(state, "receipt.json")}
	grouped := false
	b.ResetTimer()
	result, err := target.Apply(staged, bundle, nil, ApplyOptions{groupCheckpoint: func(string, int) error { grouped = true; return nil }})
	b.StopTimer()
	if err != nil {
		b.Fatal(err)
	}
	if len(result.Applied) != count || len(result.Conflicts) != 0 {
		b.Fatalf("receipt: %+v", result)
	}
	for i, p := range paths {
		file := filepath.Join(dest, p)
		if name == "deletion" && i == count-1 {
			if _, err := os.Lstat(file); !os.IsNotExist(err) {
				b.Fatalf("deleted file: %v", err)
			}
			continue
		}
		if name == "restricted" && i == count-1 {
			info, err := os.Stat(file)
			if err != nil || info.Mode().Perm() != 0 {
				b.Fatalf("mode: %v %v", info, err)
			}
			if err := os.Chmod(file, 0600); err != nil {
				b.Fatal(err)
			}
		}
		got, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(got, after) {
			b.Fatalf("content %s: %v", p, err)
		}
	}
	if pending, err := WorkspaceHasApplyTransactions(dest); err != nil || pending {
		b.Fatalf("pending: %v %v", pending, err)
	}
	if err := os.WriteFile(filepath.Join(dest, paths[0]), []byte("later"), 0600); err != nil {
		b.Fatal(err)
	}
	replay, err := target.Apply(staged, bundle, nil, ApplyOptions{})
	if err != nil || !reflect.DeepEqual(result, replay) {
		b.Fatalf("retry: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, paths[0]))
	if err != nil || string(got) != "later" {
		b.Fatalf("later edit: %q %v", got, err)
	}
	if grouped {
		b.ReportMetric(1, "grouped/op")
	} else {
		b.ReportMetric(0, "grouped/op")
	}
	b.ReportMetric(float64(count), "roots/op")
	b.ReportMetric(float64(size), "file-bytes/op")
}
