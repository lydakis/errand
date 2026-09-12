package changes

import (
	"sort"
	"strings"
)

// childFirstPathGroups sorts paths in place and groups siblings by depth.
// Joining each group before the next lets parents be restricted safely. The
// root has depth -1, so punctuation in a child name cannot move it after root.
func childFirstPathGroups(paths []string) [][]string {
	depth := func(path string) int {
		if path == "." {
			return -1
		}
		return strings.Count(path, "/")
	}
	sort.Slice(paths, func(i, j int) bool {
		a, b := depth(paths[i]), depth(paths[j])
		if a != b {
			return a > b
		}
		return paths[i] > paths[j]
	})
	var groups [][]string
	for start := 0; start < len(paths); {
		end := start + 1
		for end < len(paths) && depth(paths[end]) == depth(paths[start]) {
			end++
		}
		groups = append(groups, paths[start:end])
		start = end
	}
	return groups
}
