package snapshot

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/pathpolicy"
)

// gitSelectionEvidence binds a Git-driven selection to everything Git reads
// to produce it: the tracked set (index), ignore sources and configuration.
// Together with the unignored directory stamps held by selectionEvidence, an
// unchanged proof means tracked ∪ untracked-unignored is unchanged, so a watch
// can refresh hinted files without asking Git to enumerate the tree again.
type gitSelectionEvidence struct {
	index       string
	indexInfo   fs.FileInfo // nil when absent
	indexDigest [sha256.Size]byte
	// Ignore and configuration sources are small and can be edited in place
	// without changing any directory stamp, so their contents are compared.
	// A nil value records an absent file.
	contents map[string][]byte
}

// captureGitSelection records Git's selection inputs and returns the
// unignored directories to stamp (slash-separated, relative to root). It
// returns nil evidence, not an error, when the repository layout is outside
// what the proof covers; the caller then keeps using full selection.
func captureGitSelection(root string, opts SelectOptions) (*gitSelectionEvidence, map[string]bool, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel",
		"--git-path", "index", "--git-path", "info/exclude").Output()
	if err != nil {
		return nil, nil, nil
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != 3 {
		return nil, nil, nil
	}
	resolve := func(name string) string {
		if !filepath.IsAbs(name) {
			name = filepath.Join(root, name)
		}
		return filepath.Clean(name)
	}
	worktree, index, exclude := filepath.Clean(lines[0]), resolve(lines[1]), resolve(lines[2])
	if resolved, e := filepath.EvalSymlinks(worktree); e == nil {
		worktree = resolved
	}
	e := &gitSelectionEvidence{index: index, contents: map[string][]byte{}}
	e.indexInfo, _ = os.Lstat(index)
	if e.indexDigest, err = trackedDigest(root); err != nil {
		return nil, nil, nil
	}
	sources := []string{exclude}
	global, ok, err := gitConfigPath(root, "core.excludesFile")
	if err != nil {
		return nil, nil, nil
	}
	if !ok {
		if global, err = defaultGitExcludesPath(); err != nil {
			return nil, nil, nil
		}
	} else if !filepath.IsAbs(global) {
		global = filepath.Join(worktree, global)
	}
	sources = append(sources, global)
	// Every configuration file Git consulted, including included files.
	origins, err := exec.Command("git", "-C", root, "config", "--list", "--show-origin", "-z").Output()
	if err != nil {
		return nil, nil, nil
	}
	for _, record := range strings.Split(string(origins), "\x00") {
		if name, ok := strings.CutPrefix(record, "file:"); ok && name != "" {
			sources = append(sources, resolve(name))
		}
	}
	// Ignore files above the snapshot root still apply inside it.
	for dir := filepath.Dir(root); strings.HasPrefix(root, worktree) && len(dir) >= len(worktree); dir = filepath.Dir(dir) {
		sources = append(sources, filepath.Join(dir, ".gitignore"))
		if dir == filepath.Dir(dir) {
			break
		}
	}
	for _, name := range sources {
		if _, seen := e.contents[name]; seen {
			continue
		}
		data, err := optionalContents(name)
		if err != nil {
			return nil, nil, nil
		}
		e.contents[name] = data
	}

	excluded, err := excludedDirectories(root)
	if err != nil {
		return nil, nil, nil
	}
	directories := map[string]bool{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !d.IsDir() {
			if d.Name() == ".gitignore" {
				data, err := optionalContents(p)
				if err != nil {
					return err
				}
				e.contents[p] = data
			}
			return nil
		}
		if rel != "." && (excluded[rel] || pathContainsGitMetadata(rel) || isLocalChangeTransactionPath(rel) ||
			pathpolicy.InCache(rel, opts.Caches)) {
			return filepath.SkipDir
		}
		directories[rel] = true
		return nil
	})
	if err != nil {
		// A concurrent change; the next preparation proves selection again.
		return nil, nil, nil
	}
	return e, directories, nil
}

// excludedDirectories returns directories that an ignore pattern excludes as
// directories. Git does not descend into them, so nested rules cannot reopen
// their contents and they need no stamps. Tracked files inside them are covered
// by the index digest; the caller stamps their ancestors from the manifest.
//
// Git's --directory listing alone is not enough: it also collapses directories
// whose files happen to be ignored individually (logs/ under *.log), where a
// new nested .gitignore can reopen a file.
func excludedDirectories(root string) (map[string]bool, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--others", "--ignored",
		"--exclude-standard", "--directory").Output()
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, name := range strings.Split(string(out), "\x00") {
		if strings.HasSuffix(name, "/") {
			candidates = append(candidates, name)
		}
	}
	excluded := map[string]bool{}
	if len(candidates) == 0 {
		return excluded, nil
	}
	check := exec.Command("git", "-C", root, "check-ignore", "-z", "--stdin", "--no-index")
	check.Stdin = strings.NewReader(strings.Join(candidates, "\x00") + "\x00")
	out, err = check.Output()
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) { // 1: none ignored
		return nil, err
	}
	for _, name := range strings.Split(string(out), "\x00") {
		if dir, ok := strings.CutSuffix(name, "/"); ok {
			excluded[dir] = true
		}
	}
	return excluded, nil
}

func (e *gitSelectionEvidence) verify(root string) error {
	for name, want := range e.contents {
		data, err := optionalContents(name)
		if err != nil {
			return sourceReadError(err)
		}
		if (data == nil) != (want == nil) || !bytes.Equal(data, want) {
			return sourceChangedf("snapshot: Git selection policy changed after manifest construction; retry")
		}
	}
	info, _ := os.Lstat(e.index)
	if sameFileEvidence(e.indexInfo, info) {
		return nil
	}
	// Git rewrites the index to refresh cached stat data (for example an
	// editor's background status). Only the tracked set matters here.
	digest, err := trackedDigest(root)
	if err != nil {
		return sourceReadError(err)
	}
	if digest != e.indexDigest {
		return sourceChangedf("snapshot: Git index changed after manifest construction; retry")
	}
	return nil
}

// trackedDigest hashes tracked paths with their modes and stages, excluding
// object ids and cached stat data, which do not affect selection.
func trackedDigest(root string) ([sha256.Size]byte, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--stage").Output()
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	h := sha256.New()
	for _, record := range strings.Split(string(out), "\x00") {
		metadata, name, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(metadata)
		if len(fields) != 3 {
			continue
		}
		h.Write([]byte(fields[0] + " " + fields[2] + "\t" + name + "\x00"))
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

func optionalContents(name string) ([]byte, error) {
	data, err := os.ReadFile(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

func sameFileEvidence(old, now fs.FileInfo) bool {
	if old == nil || now == nil {
		return old == nil && now == nil
	}
	if !os.SameFile(old, now) || old.Mode() != now.Mode() || old.Size() != now.Size() || !old.ModTime().Equal(now.ModTime()) {
		return false
	}
	a, b, ok := changeStamp(old)
	c, d, supported := changeStamp(now)
	return ok && supported && a == c && b == d
}
