package snapshot

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

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

// Test hooks around capture's first Git queries: before they run, and after
// them but before capture records the sources they read.
var testHookBeforeGitQueries, testHookBeforeRecordingGitSources func()

// captureGitSelection records Git's selection inputs and returns the
// directories its walk reached (slash-separated, relative to root): true for
// unignored directories to stamp, false for excluded ones it did not enter.
// It returns nil evidence, not an error, when the repository layout is outside
// what the proof covers; the caller then keeps using full selection.
//
// Like directory stamps, sources are recorded before the work they justify.
// The first queries only name the sources and list candidate excluded
// directories. Once the sources are recorded, Git confirms the candidates and
// the config listing is repeated, so a change before a source's record shows
// in those results, and a change after it fails verification.
func captureGitSelection(root string, opts SelectOptions) (*gitSelectionEvidence, map[string]bool, error) {
	if testHookBeforeGitQueries != nil {
		testHookBeforeGitQueries()
	}
	// The Git queries are independent; run them together so full cycles pay
	// roughly one process round trip for capture.
	var (
		paths, origins        []byte
		global                string
		globalSet             bool
		standard              []string
		standardOK            bool
		ignored               map[string]bool
		e                     = &gitSelectionEvidence{contents: map[string][]byte{}}
		pathsErr, digestErr   error
		globalErr, originsErr error
		ignoredErr            error
		reads                 sync.WaitGroup
	)
	reads.Go(func() {
		paths, pathsErr = exec.Command("git", "-C", root, "rev-parse", "--show-toplevel",
			"--git-path", "index", "--git-path", "info/exclude", "--git-path", "config.worktree").Output()
		if pathsErr != nil {
			return
		}
		// Stamp the index before hashing the tracked set it describes.
		index := strings.SplitN(string(paths), "\n", 3)
		if len(index) == 3 {
			e.index = index[1]
			if !filepath.IsAbs(e.index) {
				e.index = filepath.Join(root, e.index)
			}
			e.index = filepath.Clean(e.index)
			e.indexInfo, _ = os.Lstat(e.index)
		}
		e.indexDigest, digestErr = trackedDigest(root)
	})
	reads.Go(func() {
		// Look up the excludes file after the listing, so the listing
		// repeated below also covers it.
		if origins, originsErr = gitConfigOrigins(root); originsErr == nil {
			global, globalSet, globalErr = gitConfigPath(root, "core.excludesFile")
		}
	})
	reads.Go(func() { standard, standardOK = standardGitConfigPaths(root) })
	reads.Go(func() { ignored, ignoredErr = ignoredDirectories(root) })
	reads.Wait()
	if testHookBeforeRecordingGitSources != nil {
		testHookBeforeRecordingGitSources()
	}
	if errors.Join(pathsErr, digestErr, globalErr, originsErr, ignoredErr) != nil || !standardOK {
		return nil, nil, nil
	}
	lines := strings.Split(strings.TrimSuffix(string(paths), "\n"), "\n")
	if len(lines) != 4 || e.index == "" {
		return nil, nil, nil
	}
	resolve := func(name string) string {
		if !filepath.IsAbs(name) {
			name = filepath.Join(root, name)
		}
		return filepath.Clean(name)
	}
	worktree := filepath.Clean(lines[0])
	if resolved, err := filepath.EvalSymlinks(worktree); err == nil {
		worktree = resolved
	}
	// Git reads config.worktree only with extensions.worktreeConfig, which is
	// itself recorded in the repository config; recording it always is simpler.
	sources := []string{resolve(lines[2]), resolve(lines[3])}
	if !globalSet {
		var err error
		if global, err = defaultGitExcludesPath(); err != nil {
			return nil, nil, nil
		}
	} else if !filepath.IsAbs(global) {
		global = filepath.Join(worktree, global)
	}
	sources = append(sources, global)
	sources = append(sources, standard...)
	// Records alternate between an origin and a "key\nvalue" entry. Git runs
	// from the top of the worktree, so relative origins are relative to it.
	records := strings.Split(string(origins), "\x00")
	for i := 0; i+1 < len(records); i += 2 {
		origin, ok := strings.CutPrefix(records[i], "file:")
		if ok && origin != "" {
			if !filepath.IsAbs(origin) {
				origin = filepath.Join(worktree, origin)
			}
			origin = filepath.Clean(origin)
			sources = append(sources, origin)
		} else {
			origin = ""
		}
		// Git reports an included file only once it exists, so record every
		// declared target, whether or not its condition holds now.
		key, value, _ := strings.Cut(records[i+1], "\n")
		condition, conditional := strings.CutPrefix(key, "includeif.")
		if key != "include.path" && !(conditional && strings.HasSuffix(condition, ".path")) {
			continue
		}
		if strings.HasPrefix(condition, "onbranch:") {
			return nil, nil, nil // depends on HEAD, which is not recorded
		}
		target, ok := includeTarget(value, origin)
		if !ok {
			return nil, nil, nil
		}
		sources = append(sources, target)
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

	// Repeat the config listing during the walk; it differs if the config
	// changed between the first listing and the record above.
	var (
		relisted  []byte
		relistErr error
		relist    sync.WaitGroup
	)
	relist.Go(func() { relisted, relistErr = gitConfigOrigins(root) })
	directories, err := walkGitSelection(root, opts, ignored, e.contents)
	relist.Wait()
	if err != nil || relistErr != nil || !bytes.Equal(relisted, origins) {
		// A concurrent change; the next preparation proves selection again.
		return nil, nil, nil
	}
	return e, directories, nil
}

// walkGitSelection walks the directories whose .gitignore files Git reads,
// recording each in contents, and returns the directories it reached as
// captureGitSelection does. It stops at the directories Git listed as ignored.
// Whether one is excluded depends only on rules outside it, all recorded once
// the walk ends, so Git decides then; the rest are walked afterwards.
func walkGitSelection(root string, opts SelectOptions, ignored map[string]bool, contents map[string][]byte) (map[string]bool, error) {
	directories := map[string]bool{}
	var paused []string
	walk := func(start string, pause map[string]bool) error {
		return filepath.WalkDir(filepath.Join(root, filepath.FromSlash(start)), func(p string, d fs.DirEntry, err error) error {
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
					contents[p] = data
				}
				return nil
			}
			if rel != "." && (pathContainsGitMetadata(rel) || isLocalChangeTransactionPath(rel) ||
				pathpolicy.InCache(rel, opts.Caches)) {
				directories[rel] = false
				return filepath.SkipDir
			}
			if rel != "." && pause[rel] {
				directories[rel] = false
				paused = append(paused, rel)
				return filepath.SkipDir
			}
			directories[rel] = true
			return nil
		})
	}
	if err := walk(".", ignored); err != nil {
		return nil, err
	}
	excluded, err := excludedDirectories(root, paused)
	if err != nil {
		return nil, err
	}
	for _, rel := range paused {
		if !excluded[rel] {
			if err := walk(rel, nil); err != nil {
				return nil, err
			}
		}
	}
	return directories, nil
}

// standardGitConfigPaths returns the config files Git reads by location: the
// system file, unless GIT_CONFIG_NOSYSTEM disables it, and the global files.
// Git reads them only if they exist, and --show-origin lists only files with
// entries, so they are recorded either way. It reports false when a path
// cannot be resolved.
func standardGitConfigPaths(root string) ([]string, bool) {
	return resolveGitConfigPaths(func() ([]byte, error) {
		// The default system path depends on how Git was built; Git 2.42
		// and later report it.
		return exec.Command("git", "-C", root, "var", "GIT_CONFIG_SYSTEM").Output()
	})
}

// resolveGitConfigPaths is standardGitConfigPaths with the system path query
// injected.
func resolveGitConfigPaths(systemConfig func() ([]byte, error)) ([]string, bool) {
	var names []string
	if !gitEnvTrue("GIT_CONFIG_NOSYSTEM") {
		// Older Git fails, and Git exits 1 for a GIT_CONFIG_NOSYSTEM value
		// it reads as true and gitEnvTrue does not; both keep full selection.
		out, err := systemConfig()
		system, ok := strings.CutSuffix(string(out), "\n")
		if err != nil || !ok {
			return nil, false
		}
		names = append(names, system)
	}
	if global, ok := os.LookupEnv("GIT_CONFIG_GLOBAL"); ok {
		names = append(names, global) // replaces both files below
	} else {
		home, xdg := os.Getenv("HOME"), os.Getenv("XDG_CONFIG_HOME")
		if home == "" {
			return nil, false
		}
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		names = append(names, filepath.Join(home, ".gitconfig"), filepath.Join(xdg, "git", "config"))
	}
	for i, name := range names {
		if !filepath.IsAbs(name) {
			return nil, false
		}
		names[i] = filepath.Clean(name)
	}
	return names, true
}

// gitEnvTrue reports whether Git certainly reads the environment variable as
// true. Git also accepts forms this does not, so false is not conclusive.
func gitEnvTrue(key string) bool {
	value := os.Getenv(key)
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return err == nil && n != 0
}

// includeTarget resolves a declared include path as Git does: "~" is HOME, and
// a relative path is relative to the directory of the file declaring it. It
// reports false for forms it does not resolve.
func includeTarget(value, origin string) (string, bool) {
	if rest, ok := strings.CutPrefix(value, "~"); ok {
		home := os.Getenv("HOME")
		if home == "" || (rest != "" && rest[0] != '/') { // ~user
			return "", false
		}
		value = home + rest
	}
	if value == "" || strings.HasPrefix(value, "%(prefix)/") {
		return "", false
	}
	if !filepath.IsAbs(value) {
		if origin == "" {
			return "", false
		}
		value = filepath.Join(filepath.Dir(origin), value)
	}
	return filepath.Clean(value), true
}

// gitConfigOrigins lists every configuration entry with the file it came
// from, including included files.
func gitConfigOrigins(root string) ([]byte, error) {
	return exec.Command("git", "-C", root, "config", "--list", "--show-origin", "-z").Output()
}

// ignoredDirectories returns the untracked directories Git collapses as
// ignored. Git's --directory listing alone is not enough to skip them: it
// also collapses directories whose files happen to be ignored individually
// (logs/ under *.log), where a new nested .gitignore can reopen a file.
// excludedDirectories tells the two apart.
func ignoredDirectories(root string) (map[string]bool, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--others", "--ignored",
		"--exclude-standard", "--directory").Output()
	if err != nil {
		return nil, err
	}
	ignored := map[string]bool{}
	for _, name := range strings.Split(string(out), "\x00") {
		if dir, ok := strings.CutSuffix(name, "/"); ok {
			ignored[dir] = true
		}
	}
	return ignored, nil
}

// excludedDirectories returns which of dirs an ignore pattern excludes as
// directories. Git does not descend into them, so nested rules cannot reopen
// their contents and they need no stamps. Tracked files inside them are covered
// by the index digest; the caller stamps their ancestors from the manifest.
func excludedDirectories(root string, dirs []string) (map[string]bool, error) {
	excluded := map[string]bool{}
	if len(dirs) == 0 {
		return excluded, nil
	}
	check := exec.Command("git", "-C", root, "check-ignore", "-z", "--stdin", "--no-index")
	check.Stdin = strings.NewReader(strings.Join(dirs, "/\x00") + "/\x00")
	out, err := check.Output()
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
