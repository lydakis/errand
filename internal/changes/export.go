package changes

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// ExportRemote copies retained remote values into a new directory, preserving
// workspace-relative paths. Deletions have no exported value. Publication is
// atomic and never replaces an existing destination, even an empty directory.
func ExportRemote(stagedRoot, destination, requested string, bundle proto.ChangeBundle) error {
	if err := ValidateBundle(bundle); err != nil {
		return err
	}
	manifest, err := exportManifest(bundle, requested)
	if err != nil {
		return err
	}
	if destination == "" {
		return fmt.Errorf("export destination must not be empty")
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return fmt.Errorf("export parent directory must exist: %w", err)
	}
	guard, err := openApplyDestination(parent)
	if err != nil {
		return err
	}
	defer guard.Close()
	name := filepath.Base(destination)
	if _, err := guard.root.Lstat(name); err == nil {
		return fmt.Errorf("export destination already exists: %s", destination)
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".errand-export-")
	if err != nil {
		return err
	}
	defer RemoveTree(tmp)
	if err := guard.verifyPath(); err != nil {
		return err
	}
	tree := filepath.Join(tmp, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		return err
	}
	access, err := materializeApplySnapshot(filepath.Join(stagedRoot, "remote"), tree, tmp, manifest)
	if err != nil {
		return fmt.Errorf("preparing export: %w", err)
	}
	if err := access.restore(); err != nil {
		return err
	}
	if err := guard.verifyPath(); err != nil {
		return err
	}
	from, err := guard.root.Open(filepath.Base(tmp))
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := guard.root.Open(".")
	if err != nil {
		return err
	}
	defer to.Close()
	if err := renameNoReplace(from, "tree", to, name); err != nil {
		return fmt.Errorf("publishing export (destination must not exist): %w", err)
	}
	return errors.Join(to.Sync(), guard.verifyPath())
}

func exportManifest(bundle proto.ChangeBundle, requested string) (proto.Manifest, error) {
	if requested != "" {
		if err := validatePath(requested); err != nil {
			return proto.Manifest{}, err
		}
	}
	roots := make(map[string]bool, len(bundle.Paths))
	for _, root := range bundle.Paths {
		roots[root] = true
	}
	selectedPaths := make(map[string]bool)
	for _, entry := range bundle.RemoteManifest.Entries {
		if requested != "" && entry.Path != requested && !strings.HasPrefix(entry.Path, requested+"/") {
			continue
		}
		// Ancestors retained only to describe deleted paths have no result to
		// export. Include ancestors only when a selected change has a value.
		for current := entry.Path; current != "."; current = path.Dir(current) {
			if roots[current] {
				for selected := entry.Path; selected != "."; selected = path.Dir(selected) {
					selectedPaths[selected] = true
				}
				break
			}
		}
	}
	if len(selectedPaths) == 0 {
		if requested == "" {
			return proto.Manifest{}, fmt.Errorf("job retained only deletions; there are no remote files to export")
		}
		return proto.Manifest{}, fmt.Errorf("path %q has no retained remote value to export", requested)
	}
	var selected proto.Manifest
	for _, entry := range bundle.RemoteManifest.Entries {
		if selectedPaths[entry.Path] {
			selected.Entries = append(selected.Entries, entry)
		}
	}
	return selected, nil
}
