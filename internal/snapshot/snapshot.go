// Package snapshot selects, hashes, and packs a workspace for shipment.
// A snapshot is consistent or refused: files that change between hashing
// and packing abort the pack rather than shipping a mixture of moments.
package snapshot

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"

	"github.com/lydakis/errand/internal/proto"
)

type GitInfo struct {
	Repository bool
	Commit     string
	Dirty      bool
}

type SelectOptions struct {
	// IncludeAll explicitly permits a recursive snapshot when root is neither a
	// Git worktree nor governed by .errandignore. It also acknowledges the risk
	// of using the user's home directory. Filesystem roots remain forbidden.
	IncludeAll bool
}

// SelectFiles returns the relative (slash-separated) paths to snapshot.
// Precedence: .errandignore if present, else git's view of tracked plus
// untracked-unignored files. A recursive non-Git snapshot requires an explicit
// policy or override. .git is never shipped.
func SelectFiles(root string) ([]string, GitInfo, error) {
	return SelectFilesWithOptions(root, SelectOptions{})
}

func SelectFilesWithOptions(root string, opts SelectOptions) ([]string, GitInfo, error) {
	if err := validateSnapshotRoot(root, opts); err != nil {
		return nil, GitInfo{}, err
	}
	if _, err := os.Lstat(filepath.Join(root, ".errandignore")); err == nil {
		// The explicit snapshot policy takes precedence even if repository
		// metadata is incomplete. Git information remains best-effort metadata.
		data, err := os.ReadFile(filepath.Join(root, ".errandignore"))
		if err != nil {
			return nil, GitInfo{}, fmt.Errorf("snapshot: reading .errandignore: %w", err)
		}
		gi, _ := gitInfo(root)
		matcher := ignore.CompileIgnoreLines(strings.Split(string(data), "\n")...)
		paths, err := walk(root, matcher)
		return paths, gi, err
	} else if !os.IsNotExist(err) {
		return nil, GitInfo{}, fmt.Errorf("snapshot: inspecting .errandignore: %w", err)
	}
	gi, err := gitInfo(root)
	if err != nil {
		return nil, gi, err
	}
	return selectFilesWithOptions(root, gi, gitListFiles, opts)
}

func selectFiles(root string, gi GitInfo, listGitFiles func(string) ([]string, error)) ([]string, GitInfo, error) {
	return selectFilesWithOptions(root, gi, listGitFiles, SelectOptions{})
}

func selectFilesWithOptions(root string, gi GitInfo, listGitFiles func(string) ([]string, error), opts SelectOptions) ([]string, GitInfo, error) {
	if gi.Repository {
		paths, err := listGitFiles(root)
		if err != nil {
			return nil, gi, fmt.Errorf("snapshot: listing git files: %w", err)
		}
		return paths, gi, nil
	}
	if !opts.IncludeAll {
		return nil, gi, fmt.Errorf("snapshot: %q is not a Git worktree and has no .errandignore; add an explicit policy or pass --include-all", root)
	}
	paths, err := walkWithIgnore(root)
	return paths, gi, err
}

func validateSnapshotRoot(root string, opts SelectOptions) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("snapshot: resolving root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("snapshot: inspecting root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("snapshot: root %q is not a directory", abs)
	}
	if filepath.Dir(abs) == abs {
		return fmt.Errorf("snapshot: refusing filesystem root %q", abs)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("snapshot: locating home directory: %w", err)
	}
	homeAbs, err := filepath.Abs(home)
	if err != nil {
		return fmt.Errorf("snapshot: resolving home directory: %w", err)
	}
	sameHome := filepath.Clean(abs) == filepath.Clean(homeAbs)
	if !sameHome {
		if homeInfo, statErr := os.Stat(homeAbs); statErr == nil {
			sameHome = os.SameFile(info, homeInfo)
		}
	}
	if sameHome && !opts.IncludeAll {
		return fmt.Errorf("snapshot: refusing home directory %q without --include-all", abs)
	}
	return nil
}

func gitInfo(root string) (GitInfo, error) {
	inside, err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		if hasGitMarker(root) {
			return GitInfo{}, fmt.Errorf("snapshot: detecting git repository: %w", err)
		}
		return GitInfo{}, nil
	}
	if strings.TrimSpace(string(inside)) != "true" {
		return GitInfo{}, nil
	}
	gi := GitInfo{Repository: true}
	out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", "HEAD").Output()
	if err == nil {
		gi.Commit = strings.TrimSpace(string(out))
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		return gi, fmt.Errorf("snapshot: resolving git HEAD: %w", err)
	}
	st, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return gi, fmt.Errorf("snapshot: reading git status: %w", err)
	}
	if len(strings.TrimSpace(string(st))) > 0 {
		gi.Dirty = true
	}
	return gi, nil
}

func hasGitMarker(root string) bool {
	dir, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func gitListFiles(root string) ([]string, error) {
	staged, err := exec.Command("git", "-C", root, "ls-files", "-z", "--stage").Output()
	if err != nil {
		return nil, err
	}
	for _, record := range strings.Split(string(staged), "\x00") {
		metadata, name, ok := strings.Cut(record, "\t")
		if ok && strings.HasPrefix(metadata, "160000 ") {
			return nil, fmt.Errorf("git submodule %q is not supported; use .errandignore to define an explicit recursive snapshot", name)
		}
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "-co", "--exclude-standard").Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		// git can list files that no longer exist (staged deletes)
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err == nil {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func walkWithIgnore(root string) ([]string, error) {
	var matcher *ignore.GitIgnore
	if data, err := os.ReadFile(filepath.Join(root, ".errandignore")); err == nil {
		matcher = ignore.CompileIgnoreLines(strings.Split(string(data), "\n")...)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("snapshot: reading .errandignore: %w", err)
	}
	return walk(root, matcher)
}

func walk(root string, matcher *ignore.GitIgnore) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if pathBase := filepath.Base(p); pathBase == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if matcher != nil && matcher.MatchesPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

// Build lstats and hashes every selected path into a manifest.
func Build(root string, paths []string) (proto.Manifest, error) {
	selected := make(map[string]struct{}, len(paths))
	for _, rel := range paths {
		selected[rel] = struct{}{}
		for parent := path.Dir(rel); parent != "." && parent != "/"; parent = path.Dir(parent) {
			selected[parent] = struct{}{}
		}
	}
	paths = make([]string, 0, len(selected))
	for rel := range selected {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	var m proto.Manifest
	for _, rel := range paths {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Lstat(abs)
		if err != nil {
			return m, err
		}
		e := proto.ManifestEntry{Path: rel, Mode: uint32(fi.Mode().Perm())}
		switch {
		case fi.Mode().IsDir():
			e.Type = proto.EntryDir
		case fi.Mode()&fs.ModeSymlink != 0:
			e.Type = proto.EntrySymlink
			target, err := os.Readlink(abs)
			if err != nil {
				return m, err
			}
			e.Target = target
		case fi.Mode().IsRegular():
			e.Type = proto.EntryFile
			e.Size = fi.Size()
			sum, err := hashFile(abs)
			if err != nil {
				return m, err
			}
			e.SHA256 = sum
		default:
			return m, fmt.Errorf("snapshot: %s: unsupported file type %v", rel, fi.Mode())
		}
		m.Entries = append(m.Entries, e)
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m, nil
}

// Pack writes the manifest's entries as a tar stream, verifying each file
// still hashes to its manifest value. Any change aborts the pack.
func Pack(w io.Writer, root string, m proto.Manifest) error {
	return PackPartial(w, root, m, nil)
}

// PackPartial revalidates every entry but writes only selected files.
// Directories and symlinks are always written.
func PackPartial(w io.Writer, root string, m proto.Manifest, shipFile func(proto.ManifestEntry) bool) error {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	tw := tar.NewWriter(w)
	for _, e := range m.Entries {
		hdr := &tar.Header{Name: e.Path, Mode: int64(e.Mode)}
		switch e.Type {
		case proto.EntryDir:
			fi, err := rootFS.Lstat(e.Path)
			if err != nil || !fi.IsDir() || uint32(fi.Mode().Perm()) != e.Mode {
				return fmt.Errorf("snapshot: %s changed during pack; retry", e.Path)
			}
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case proto.EntrySymlink:
			fi, err := rootFS.Lstat(e.Path)
			if err != nil || fi.Mode()&fs.ModeSymlink == 0 || uint32(fi.Mode().Perm()) != e.Mode {
				return fmt.Errorf("snapshot: %s changed during pack; retry", e.Path)
			}
			target, err := rootFS.Readlink(e.Path)
			if err != nil || target != e.Target {
				return fmt.Errorf("snapshot: %s changed during pack; retry", e.Path)
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.Target
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case proto.EntryFile:
			fi, err := rootFS.Lstat(e.Path)
			if err != nil {
				return fmt.Errorf("snapshot: %s vanished during pack: %w", e.Path, err)
			}
			if !fi.Mode().IsRegular() || fi.Size() != e.Size || uint32(fi.Mode().Perm()) != e.Mode {
				return fmt.Errorf("snapshot: %s changed during pack; retry", e.Path)
			}
			f, err := rootFS.Open(e.Path)
			if err != nil {
				return fmt.Errorf("snapshot: %s changed during pack; retry: %w", e.Path, err)
			}
			opened, err := f.Stat()
			if err != nil || !opened.Mode().IsRegular() || opened.Size() != e.Size || uint32(opened.Mode().Perm()) != e.Mode {
				f.Close()
				return fmt.Errorf("snapshot: %s changed during pack; retry", e.Path)
			}
			h := sha256.New()
			var dest io.Writer = h
			if shipFile == nil || shipFile(e) {
				hdr.Typeflag = tar.TypeReg
				hdr.Size = e.Size
				if err := tw.WriteHeader(hdr); err != nil {
					f.Close()
					return err
				}
				dest = io.MultiWriter(tw, h)
			}
			n, err := io.Copy(dest, io.LimitReader(f, e.Size))
			if err != nil {
				f.Close()
				return err
			}
			var extra [1]byte
			extraN, extraErr := f.Read(extra[:])
			closed, statErr := f.Stat()
			closeErr := f.Close()
			if extraErr != nil && extraErr != io.EOF {
				return extraErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 ||
				extraN != 0 || extraErr != io.EOF || statErr != nil || !closed.Mode().IsRegular() ||
				closed.Size() != e.Size || uint32(closed.Mode().Perm()) != e.Mode {
				return fmt.Errorf("snapshot: %s changed during pack; retry", e.Path)
			}
		}
	}
	return tw.Close()
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
