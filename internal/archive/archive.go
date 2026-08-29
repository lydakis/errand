// Package archive extracts a workspace tar stream, trusting nothing: every
// entry must match the manifest, stay inside the destination, and hash to
// its declared value. A strange archive must not write outside the
// workspace.
package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// Validate checks manifest-level path safety before any byte is written:
// clean relative paths only, no duplicates, no entry routed through a
// symlink, and symlink targets that resolve inside the workspace.
func Validate(m proto.Manifest) error {
	seen := make(map[string]string, len(m.Entries)) // path -> type
	for _, e := range m.Entries {
		if err := checkRelPath(e.Path); err != nil {
			return err
		}
		if err := validateEntry(e); err != nil {
			return err
		}
		if _, dup := seen[e.Path]; dup {
			return fmt.Errorf("archive: duplicate path %q", e.Path)
		}
		seen[e.Path] = e.Type
	}
	for _, e := range m.Entries {
		for dir := path.Dir(e.Path); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if parentType, explicit := seen[dir]; explicit && parentType != proto.EntryDir {
				return fmt.Errorf("archive: %q passes through non-directory %q", e.Path, dir)
			}
		}
		if e.Type == proto.EntrySymlink {
			if err := checkSymlinkTarget(e.Path, e.Target); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEntry(e proto.ManifestEntry) error {
	switch e.Type {
	case proto.EntryFile:
		if e.Size < 0 {
			return fmt.Errorf("archive: file %q has negative size", e.Path)
		}
		if len(e.SHA256) != sha256.Size*2 {
			return fmt.Errorf("archive: file %q has invalid sha256", e.Path)
		}
		if _, err := hex.DecodeString(e.SHA256); err != nil {
			return fmt.Errorf("archive: file %q has invalid sha256", e.Path)
		}
		if e.Target != "" {
			return fmt.Errorf("archive: file %q has a symlink target", e.Path)
		}
	case proto.EntryDir:
		if e.Size != 0 || e.SHA256 != "" || e.Target != "" {
			return fmt.Errorf("archive: directory %q has file or symlink fields", e.Path)
		}
	case proto.EntrySymlink:
		if e.Size != 0 || e.SHA256 != "" {
			return fmt.Errorf("archive: symlink %q has file fields", e.Path)
		}
	case "":
		return fmt.Errorf("archive: entry %q has no type", e.Path)
	default:
		return fmt.Errorf("archive: entry %q has unsupported type %q", e.Path, e.Type)
	}
	return nil
}

func checkRelPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p ||
		p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "\x00") {
		return fmt.Errorf("archive: unsafe path %q", p)
	}
	return nil
}

func checkSymlinkTarget(link, target string) error {
	if target == "" || strings.HasPrefix(target, "/") {
		return fmt.Errorf("archive: symlink %q has unsafe target %q", link, target)
	}
	resolved := path.Clean(path.Join(path.Dir(link), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("archive: symlink %q escapes workspace (target %q)", link, target)
	}
	return nil
}

// Extract writes the stream into dest, verifying each entry against the
// manifest and enforcing the total-size limit. dest must be a fresh,
// errand-owned directory. Symlinks are created after all other entries.
func Extract(r io.Reader, dest string, m proto.Manifest, maxBytes int64) error {
	if err := Validate(m); err != nil {
		return err
	}
	byPath := make(map[string]proto.ManifestEntry, len(m.Entries))
	for _, e := range m.Entries {
		byPath[e.Path] = e
	}

	tr := tar.NewReader(r)
	written := make(map[string]bool)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		e, ok := byPath[name]
		if !ok {
			return fmt.Errorf("archive: entry %q not in manifest", hdr.Name)
		}
		if written[name] {
			return fmt.Errorf("archive: duplicate stream entry %q", name)
		}
		written[name] = true
		abs := filepath.Join(dest, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if e.Type != proto.EntryDir {
				return typeMismatch(name)
			}
			if err := os.MkdirAll(abs, os.FileMode(e.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if e.Type != proto.EntryFile {
				return typeMismatch(name)
			}
			if maxBytes < 0 || e.Size > maxBytes-total {
				return fmt.Errorf("archive: workspace exceeds %d bytes", maxBytes)
			}
			total += e.Size
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(e.Mode))
			if err != nil {
				return err
			}
			h := sha256.New()
			n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(tr, e.Size+1))
			if err != nil {
				f.Close()
				return err
			}
			if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
				f.Close()
				return fmt.Errorf("archive: %q does not match manifest hash", name)
			}
			if err := f.Chmod(os.FileMode(e.Mode)); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if e.Type != proto.EntrySymlink || hdr.Linkname != e.Target {
				return typeMismatch(name)
			}
			// created after the loop
		default:
			return fmt.Errorf("archive: %q has unsupported tar type %d", name, hdr.Typeflag)
		}
	}
	for _, e := range m.Entries {
		if !written[e.Path] {
			return fmt.Errorf("archive: manifest entry %q missing from stream", e.Path)
		}
	}
	// Symlinks last: no file write can ever traverse one of our links.
	for _, e := range m.Entries {
		if e.Type != proto.EntrySymlink {
			continue
		}
		abs := filepath.Join(dest, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(e.Target, abs); err != nil {
			return err
		}
	}
	// Directories stay traversable while children and symlinks are created.
	// Restore their declared modes deepest-first only after extraction is complete.
	var dirs []proto.ManifestEntry
	for _, e := range m.Entries {
		if e.Type == proto.EntryDir {
			dirs = append(dirs, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i].Path, "/") > strings.Count(dirs[j].Path, "/")
	})
	for _, e := range dirs {
		abs := filepath.Join(dest, filepath.FromSlash(e.Path))
		if err := os.Chmod(abs, os.FileMode(e.Mode)); err != nil {
			return err
		}
	}
	return nil
}

func typeMismatch(name string) error {
	return fmt.Errorf("archive: %q disagrees with manifest about type or target", name)
}
