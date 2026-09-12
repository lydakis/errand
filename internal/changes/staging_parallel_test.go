package changes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/snapshot"
)

func TestStagingJoinsBoundedFileWorkBeforeParentsAndPublication(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "!dir"), 0700); err != nil {
				t.Fatal(err)
			}
			var paths []string
			for i := range stagingWorkers*2 + 1 {
				name := fmt.Sprintf("!dir/%02d", i)
				writeTransferFile(t, root, name, "body")
				paths = append(paths, name)
			}
			m, err := snapshot.Build(root, paths)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range paths {
				if err := os.Chmod(filepath.Join(root, name), 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(filepath.Join(root, "!dir"), 0); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				os.Chmod(root, 0700)
				os.Chmod(filepath.Join(root, "!dir"), 0700)
			})
			started := make(chan struct{}, len(paths))
			release := make(chan struct{})
			var completed, parents, published atomic.Int32
			injected := errors.New("member sync failed")
			done := make(chan error, 1)
			go func() {
				done <- syncTransferSource(root, m, func(f *os.File) error {
					info, err := f.Stat()
					if err != nil {
						return err
					}
					if info.Mode().Perm() != 0 {
						t.Error("synced before restoring mode")
					}
					if info.IsDir() {
						if int(completed.Load()) != len(paths) {
							t.Error("parent synced before children finished")
						}
						parents.Add(1)
						return nil
					}
					started <- struct{}{}
					<-release
					completed.Add(1)
					if fail && filepath.Base(f.Name()) == "00" {
						return injected
					}
					return nil
				}, func(*os.File) error {
					if int(completed.Load()) != len(paths) || parents.Load() != 2 {
						t.Error("published before joining all members")
					}
					published.Add(1)
					return nil
				})
			}()
			// All workers must be able to start concurrently, but no more than the
			// shared bound can enter file synchronization before one is released.
			for range stagingWorkers {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					close(release)
					<-done
					t.Fatal("staging did not start bounded concurrent work")
				}
			}
			select {
			case <-started:
				t.Error("exceeded staging worker bound")
			default:
			}
			if parents.Load() != 0 || published.Load() != 0 {
				t.Error("advanced before joining members")
			}
			close(release)
			err = <-done
			if fail && !errors.Is(err, injected) || !fail && err != nil {
				t.Fatalf("staging result: %v", err)
			}
			if int(completed.Load()) != len(paths) || parents.Load() != 2 {
				t.Fatal("failure left members unrestored")
			}
			if fail && published.Load() != 0 || !fail && published.Load() != 1 {
				t.Fatal("incorrect publication after member synchronization")
			}
		})
	}
}

func TestStagingCancellationPreservesFirstFailure(t *testing.T) {
	injected := errors.New("storage sync failed")
	started := make(chan struct{})
	err := runStagingTasksContext(context.Background(), 2, func(ctx context.Context, i int) error {
		if i == 0 {
			<-started
			return injected
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, injected) || errors.Is(err, context.Canceled) {
		t.Fatalf("worker cancellation hid the original storage failure: %v", err)
	}
}

func TestCapturedDirectoriesJoinSiblingsBeforeRestrictingParent(t *testing.T) {
	root := t.TempDir()
	paths := []string{"parent", "parent/a", "parent/b"}
	var directories []proto.ManifestEntry
	for _, path := range paths {
		if err := os.Mkdir(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
		directories = append(directories, proto.ManifestEntry{Path: path, Type: proto.EntryDir, Mode: 0})
	}
	t.Cleanup(func() {
		for _, path := range paths {
			os.Chmod(filepath.Join(root, path), 0700)
		}
	})
	started, release := make(chan struct{}, 2), make(chan struct{})
	var children atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- finalizeCapturedDirectories(root, directories, func(file *os.File) error {
			info, err := file.Stat()
			if err != nil {
				return err
			}
			if info.Mode().Perm() != 0 {
				t.Error("directory synced before final permissions")
			}
			if filepath.Base(file.Name()) == "parent" {
				if children.Load() != 2 {
					t.Error("parent restricted before children joined")
				}
				return nil
			}
			started <- struct{}{}
			<-release
			children.Add(1)
			return nil
		})
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			close(release)
			<-done
			t.Fatal("sibling directory syncs did not start concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
