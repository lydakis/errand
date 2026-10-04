//go:build !windows

package changes

// Unix removes a directory while a descriptor to it is open, so the verified
// root stays open until the tree is gone.
func releaseTreeForRemoval(*treeAccess, string) error { return nil }
