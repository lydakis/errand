//go:build darwin || linux

package snapshotcheckpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/snapshot"
	"golang.org/x/sys/unix"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root, cache := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{".errandignore": "", "a": "before", "b": "stable"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, cache
}

func oracle(t *testing.T, root, cache string) Result {
	t.Helper()
	ctx := context.Background()
	got, err := Prepare(ctx, root, cache, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := Cold(ctx, root, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := got.State.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := want.State.RootHash(ctx)
	if err != nil || a != b {
		t.Fatalf("checkpoint differs from cold snapshot: %s %s %v", a, b, err)
	}
	return got
}

func TestRestartReusesObservationsAndRevalidatesEdits(t *testing.T) {
	root, cache := fixture(t)
	first := oracle(t, root, cache)
	if first.Reused != 0 || !first.Written {
		t.Fatalf("cold: %+v", first)
	}
	warm := oracle(t, root, cache)
	if warm.Reused != 3 || warm.Hashed != 0 || warm.Written {
		t.Fatalf("warm: %+v", warm)
	}
	name := filepath.Join(root, "a")
	info, _ := os.Stat(name)
	time.Sleep(time.Millisecond)
	if err := os.WriteFile(name, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed := oracle(t, root, cache)
	if changed.Hashed != 1 || changed.Reused != 2 {
		t.Fatalf("backdated edit: %+v", changed)
	}
	if err := os.WriteFile(name+".new", []byte("atomic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name+".new", name); err != nil {
		t.Fatal(err)
	}
	oracle(t, root, cache)
	if err := os.Chmod(name, 0700); err != nil {
		t.Fatal(err)
	}
	oracle(t, root, cache)
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../b", filepath.Join(root, "nested/link")); err != nil {
		t.Fatal(err)
	}
	oracle(t, root, cache)
}

func TestCacheFailureIdentityAndSelectionFallBack(t *testing.T) {
	root, cache := fixture(t)
	oracle(t, root, cache)
	file := filepath.Join(cache, "checkpoint")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, broken := range [][]byte{data[:len(data)/2], append([]byte("bad"), data[3:]...)} {
		if err := os.WriteFile(file, broken, 0600); err != nil {
			t.Fatal(err)
		}
		got := oracle(t, root, cache)
		if got.Reused != 0 || got.CacheStatus != "corrupt" {
			t.Fatalf("corrupt: %+v", got)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".errandignore"), []byte("b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := oracle(t, root, cache)
	if got.Reused != 0 || got.CacheStatus != "identity" {
		t.Fatalf("policy: %+v", got)
	}
	other, _ := fixture(t)
	if got := oracle(t, other, cache); got.Reused != 0 || got.CacheStatus != "identity" {
		t.Fatalf("checkout: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(cache, ".checkpoint.tmp"), []byte("interrupted write"), 0600); err != nil {
		t.Fatal(err)
	}
	oracle(t, root, cache)
	if _, err := os.Stat(filepath.Join(cache, ".checkpoint.tmp")); !os.IsNotExist(err) {
		t.Fatalf("orphan temp: %v", err)
	}
}

func TestCancellationAndCachePlacement(t *testing.T) {
	root, cache := fixture(t)
	oracle(t, root, cache)
	before, _ := os.ReadFile(filepath.Join(cache, "checkpoint"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, root, cache, snapshot.SelectOptions{}); err == nil {
		t.Fatal("accepted cancellation")
	}
	after, _ := os.ReadFile(filepath.Join(cache, "checkpoint"))
	if string(before) != string(after) {
		t.Fatal("cancelled preparation replaced checkpoint")
	}
	if _, err := Prepare(context.Background(), root, filepath.Join(root, "cache"), snapshot.SelectOptions{}); err == nil {
		t.Fatal("accepted cache inside source")
	}
}

func TestBootIdentityNonregularCacheAndConcurrentWriters(t *testing.T) {
	root, cache := fixture(t)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	oracle(t, root, cache)
	_, _, policy, err := snapshot.SelectFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkoutIdentity(root, policy, snapshot.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cp, status, _, err := readCheckpoint(context.Background(), cache, key, true)
	if err != nil {
		t.Fatal(err)
	}
	if status != "hit" {
		t.Fatal(status)
	}
	cp.Identity.Boot = "previous boot"
	if _, err := writeRawCheckpoint(context.Background(), cache, cp.checkpoint); err != nil {
		t.Fatal(err)
	}
	if got := oracle(t, root, cache); got.CacheStatus != "identity" || got.Reused != 0 {
		t.Fatalf("boot: %+v", got)
	}
	file := filepath.Join(cache, "checkpoint")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(file, 0600); err != nil {
		t.Fatal(err)
	}
	if got := oracle(t, root, cache); got.CacheStatus != "corrupt" || !got.Written {
		t.Fatalf("fifo: %+v", got)
	}
	verified := verifiedFixture(t, root)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := writeCheckpoint(context.Background(), cache, key, verified)
			if err != nil && err != unix.EWOULDBLOCK && err != unix.EAGAIN {
				t.Error(err)
			}
			if _, status, _, err := readCheckpoint(context.Background(), cache, key, true); status != "hit" || err != nil {
				t.Errorf("concurrent read: %s %v", status, err)
			}
		})
	}
	wg.Wait()
	oracle(t, root, cache)
}

func TestCacheWriteFailureDoesNotFailPreparation(t *testing.T) {
	root, cache := fixture(t)
	file := filepath.Join(cache, "not-a-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	got := oracle(t, root, file)
	if got.CacheError == "" || got.Written {
		t.Fatalf("write failure: %+v", got)
	}
}

func TestCaseAliasCannotPutCheckpointInsideSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Checkout")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(root), strings.ToLower(filepath.Base(root)))
	info, err := os.Stat(alias)
	if os.IsNotExist(err) {
		t.Skip("case-sensitive filesystem")
	}
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.Stat(root)
	if !os.SameFile(original, info) {
		t.Skip("distinct directories")
	}
	if _, err := Prepare(context.Background(), root, filepath.Join(alias, "cache"), snapshot.SelectOptions{IncludeAll: true}); err == nil {
		t.Fatal("accepted source case alias")
	}
	if _, err := os.Stat(filepath.Join(root, "cache")); !os.IsNotExist(err) {
		t.Fatalf("created cache: %v", err)
	}
}
