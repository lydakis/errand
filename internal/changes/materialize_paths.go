package changes

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

var errMaterializationParentVerification = errors.New("retained materialization parent verification failed")

// Retain at most one parent per possible copying worker. Root-level and
// one-directory paths already resolve in one step and need no retained handle.
// Source handles are revalidated before eviction/close; callers close the cache
// after workers join and before publishing any resulting tree.
type materializationPaths struct {
	root    *os.Root
	verify  bool
	mu      sync.Mutex
	parents map[string]*materializationParent
	clock   uint64
}

type materializationParent struct {
	root      *os.Root
	identity  os.FileInfo
	used      uint64
	borrowers int
}

func (p *materializationPaths) parent(name string) (*os.Root, string, *materializationParent, error) {
	slash := strings.LastIndexByte(name, '/')
	if slash < 0 || !strings.Contains(name[:slash], "/") {
		return p.root, name, nil, nil
	}
	key := name[:slash]
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clock++
	if parent := p.parents[key]; parent != nil {
		parent.used = p.clock
		parent.borrowers++
		return parent.root, name[slash+1:], parent, nil
	}
	if len(p.parents) >= stagingWorkers {
		victim, oldest := p.oldestLeaf()
		// All retained handles are borrowed. Use the original root without opening
		// another retained descriptor or waiting for a worker holding a lease.
		if oldest == nil {
			return p.root, name, nil, nil
		}
		delete(p.parents, victim)
		if err := p.closeParent(victim, oldest); err != nil {
			// A permission fallback may open new paths, but must not bypass
			// failed verification of handles that were already used.
			return nil, "", nil, errors.Join(errMaterializationParentVerification, err)
		}
	}
	// A retained ancestor also avoids restarting the walk when a new child
	// directory is first encountered.
	base, rel := p.root, key
	for slash := strings.LastIndexByte(key, '/'); slash >= 0; slash = strings.LastIndexByte(key[:slash], '/') {
		if ancestor := p.parents[key[:slash]]; ancestor != nil {
			base, rel = ancestor.root, key[slash+1:]
			break
		}
	}
	root, err := base.OpenRoot(rel)
	if err != nil {
		return nil, "", nil, err
	}
	parent := &materializationParent{root: root, used: p.clock, borrowers: 1}
	if p.verify {
		parent.identity, err = root.Lstat(".")
		if err != nil {
			return nil, "", nil, errors.Join(err, root.Close())
		}
	}
	if p.parents == nil {
		p.parents = make(map[string]*materializationParent)
	}
	p.parents[key] = parent
	return root, name[slash+1:], parent, nil
}

func (p *materializationPaths) release(parent *materializationParent) {
	if parent == nil {
		return
	}
	p.mu.Lock()
	parent.borrowers--
	p.mu.Unlock()
}

func (p *materializationPaths) closeParent(name string, parent *materializationParent) error {
	var err error
	if p.verify {
		var info os.FileInfo
		base, rel := p.root, name
		for slash := strings.LastIndexByte(name, '/'); slash >= 0; slash = strings.LastIndexByte(name[:slash], '/') {
			if ancestor := p.parents[name[:slash]]; ancestor != nil {
				base, rel = ancestor.root, name[slash+1:]
				break
			}
		}
		info, err = base.Lstat(rel)
		if err == nil && !os.SameFile(info, parent.identity) {
			err = fmt.Errorf("materialization source parent %q changed while copying", name)
		}
	}
	return errors.Join(err, parent.root.Close())
}

// Evict leaves before ancestors. This lets identity verification use a retained
// parent, whose own binding is verified later, all the way back to the caller's
// root. Closing an ancestor first would force repeated full-path verification.
func (p *materializationPaths) oldestLeaf() (string, *materializationParent) {
	var victim string
	var oldest *materializationParent
	for key, parent := range p.parents {
		if parent.borrowers != 0 {
			continue
		}
		leaf := true
		for child := range p.parents {
			if strings.HasPrefix(child, key+"/") {
				leaf = false
				break
			}
		}
		if leaf && (oldest == nil || parent.used < oldest.used) {
			victim, oldest = key, parent
		}
	}
	return victim, oldest
}

func (p *materializationPaths) close() error {
	var err error
	for len(p.parents) > 0 {
		name, parent := p.oldestLeaf()
		if parent == nil {
			return errors.Join(err, fmt.Errorf("materialization parents still borrowed"))
		}
		err = errors.Join(err, p.closeParent(name, parent))
		delete(p.parents, name)
	}
	return err
}

func (p *materializationPaths) mkdirAll(name string) error {
	parent, leaf, lease, err := p.parent(name)
	if os.IsNotExist(err) {
		// Manifests may omit implicit parents. Bootstrap that chain once, then
		// subsequent members can use the same retained-parent path as explicit dirs.
		return p.root.MkdirAll(name, 0700)
	}
	if err != nil {
		return err
	}
	defer p.release(lease)
	return parent.MkdirAll(leaf, 0700)
}

func (p *materializationPaths) open(name string) (*os.File, error) {
	// Finalization opens each directory once. Reuse a retained ancestor, but
	// don't pay for opening and evicting a new parent solely for this one open.
	p.mu.Lock()
	base, rel := p.root, name
	var lease *materializationParent
	for key := name; ; {
		if parent := p.parents[key]; parent != nil {
			base, lease = parent.root, parent
			rel = "."
			if key != name {
				rel = name[len(key)+1:]
			}
			parent.borrowers++
			break
		}
		slash := strings.LastIndexByte(key, '/')
		if slash < 0 {
			break
		}
		key = key[:slash]
	}
	p.mu.Unlock()
	defer p.release(lease)
	file, err := base.Open(rel)
	if errors.Is(err, os.ErrPermission) {
		// An explicit case-folding alias may already have removed read access.
		// Darwin search descriptors still permit synchronizing that directory.
		return openSearchMaterializedDirectory(p.root, name)
	}
	return file, err
}
