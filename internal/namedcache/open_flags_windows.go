//go:build windows

package namedcache

// Windows has no equivalent open flags. os.Root still refuses links that
// leave the store.
const (
	openDirectory = 0
	openNoFollow  = 0
	openNonblock  = 0
)
