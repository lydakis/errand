package pathpolicy

import (
	"fmt"
	"path"
	"strings"
)

// ValidateArtifacts accepts bounded, exact workspace-relative paths. These
// declarations affect retention only; they never widen input selection.
func ValidateArtifacts(paths []string) error {
	if len(paths) > MaxPatterns {
		return fmt.Errorf("artifact list exceeds %d paths", MaxPatterns)
	}
	total := 0
	seen := make(map[string]bool, len(paths))
	for _, name := range paths {
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00\r\n*?[]") {
			return fmt.Errorf("artifact path %q must be an exact workspace-relative file or directory", name)
		}
		for i, part := range strings.Split(name, "/") {
			if strings.EqualFold(part, ".git") || (i == 0 && strings.HasPrefix(part, ".errand-change-")) {
				return fmt.Errorf("artifact path %q uses reserved metadata", name)
			}
		}
		if len(name) > MaxPatternBytes || len(name) > MaxPolicyBytes-total {
			return fmt.Errorf("artifact paths exceed size limit")
		}
		total += len(name)
		if seen[name] {
			return fmt.Errorf("artifact path %q is declared more than once", name)
		}
		seen[name] = true
	}
	return nil
}
