// Package relpath answers path.Clean and path.Dir questions about
// slash-separated paths without building cleaned copies. Manifest validation
// asks them for every entry, so they run in a single pass over the bytes.
package relpath

import "strings"

// IsClean reports whether path.Clean(p) == p.
func IsClean(p string) bool {
	switch p {
	case "":
		return false
	case ".", "/":
		return true
	}
	start := 0
	if p[0] == '/' {
		start = 1
	}
	// Clean keeps ".." only as a leading run of a relative path.
	leading := start == 0
	for i := start; i <= len(p); i++ {
		if i < len(p) && p[i] != '/' {
			continue
		}
		switch element := p[start:i]; element {
		case "", ".":
			return false // empty elements include a trailing slash
		case "..":
			if !leading {
				return false
			}
		default:
			leading = false
		}
		start = i + 1
	}
	return true
}

// Dir returns path.Dir(p) for a p that IsClean accepts.
func Dir(p string) string {
	switch i := strings.LastIndexByte(p, '/'); i {
	case -1:
		return "."
	case 0:
		return "/"
	default:
		return p[:i]
	}
}
