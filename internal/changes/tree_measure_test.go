package changes

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestMeasureTreeMatchesTreeUsageWithoutWideningModes(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{"a.txt": "alpha", "nested/b.txt": "bravo!", "nested/deeper/c.bin": "charlie"} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	readOnly := filepath.Join(root, "nested", "deeper")
	if err := os.Chmod(filepath.Join(readOnly, "c.bin"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })

	all, regular, err := MeasureTreeContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len("alpha") + len("bravo!") + len("charlie")); regular != want {
		t.Fatalf("regular bytes = %d, want %d", regular, want)
	}
	for rel, mode := range map[string]fs.FileMode{"nested/deeper": 0o500, "nested/deeper/c.bin": 0} {
		info, err := os.Lstat(filepath.Join(root, rel))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode changed by measurement: %v %v", rel, info.Mode(), err)
		}
	}
	wantAll, wantRegular, err := TreeUsageContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if all != wantAll || regular != wantRegular {
		t.Fatalf("read-only measurement %d/%d, widened measurement %d/%d", all, regular, wantAll, wantRegular)
	}
	transfer, err := TransferStorageBytes(root)
	if err != nil || transfer != regular {
		t.Fatalf("transfer accounting %d %v, measured %d", transfer, err, regular)
	}
}

func TestMeasureTreeReportsHiddenDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root traverses directories without owner permission")
	}
	root := t.TempDir()
	sealed := filepath.Join(root, "sealed")
	if err := os.Mkdir(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sealed, "artifact"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o700) })
	if _, _, err := MeasureTreeContext(t.Context(), root); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("hidden directory measured as %v", err)
	}
	_, regular, err := TreeUsageContext(t.Context(), root)
	if err != nil || regular != int64(len("retained")) {
		t.Fatalf("widened measurement %d %v", regular, err)
	}
	if info, err := os.Lstat(sealed); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("widened measurement left %v %v", info.Mode(), err)
	}
}
