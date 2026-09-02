// Package pathpolicy compiles the immutable ignore rules shared by snapshot
// preparation and remote workspace-change retention.
package pathpolicy

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

const (
	MaxPatterns     = 10_000
	MaxPatternBytes = 8 << 10
	MaxPolicyBytes  = 256 << 10
)

type rule struct {
	components    []string
	negated       bool
	directoryOnly bool
	anchored      bool
}

type Matcher struct {
	prefix string
	rules  []rule
}

func Compile(policy proto.SelectionPolicy) (*Matcher, error) {
	if err := validatePrefix(policy.Prefix); err != nil {
		return nil, err
	}
	if len(policy.Ignore) > MaxPatterns {
		return nil, fmt.Errorf("selection policy exceeds %d ignore patterns", MaxPatterns)
	}
	total := 0
	rules := make([]rule, 0, len(policy.Ignore))
	for _, pattern := range policy.Ignore {
		if len(pattern) > MaxPatternBytes {
			return nil, fmt.Errorf("selection policy pattern exceeds %d bytes", MaxPatternBytes)
		}
		if strings.ContainsAny(pattern, "\x00\r\n") {
			return nil, fmt.Errorf("selection policy pattern contains a line break or NUL")
		}
		if len(pattern) > MaxPolicyBytes-total {
			return nil, fmt.Errorf("selection policy exceeds %d bytes", MaxPolicyBytes)
		}
		total += len(pattern)
		compiled, ok, err := compileRule(pattern)
		if err != nil {
			return nil, err
		}
		if ok {
			rules = append(rules, compiled)
		}
	}
	return &Matcher{prefix: policy.Prefix, rules: rules}, nil
}

func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if path.IsAbs(prefix) || path.Clean(prefix) != prefix || prefix == "." || prefix == ".." ||
		strings.HasPrefix(prefix, "../") || strings.ContainsRune(prefix, '\x00') {
		return fmt.Errorf("selection policy has unsafe workspace prefix %q", prefix)
	}
	return nil
}

func compileRule(pattern string) (rule, bool, error) {
	pattern = trimUnescapedTrailingSpaces(pattern)
	if pattern == "" || strings.HasPrefix(pattern, "#") {
		return rule{}, false, nil
	}
	compiled := rule{}
	if strings.HasPrefix(pattern, "!") {
		compiled.negated = true
		pattern = strings.TrimPrefix(pattern, "!")
	}
	if pattern == "" {
		return rule{}, false, nil
	}
	if strings.HasSuffix(pattern, "/") {
		compiled.directoryOnly = true
		pattern = strings.TrimSuffix(pattern, "/")
	}
	compiled.anchored = strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	if pattern == "" {
		return rule{}, false, nil
	}
	compiled.components = strings.Split(pattern, "/")
	for i, component := range compiled.components {
		if component == "**" {
			continue
		}
		component = normalizeClassNegation(component)
		component = normalizePOSIXClasses(component)
		if _, err := filepath.Match(component, ""); err != nil {
			return rule{}, false, fmt.Errorf("invalid selection policy pattern %q: %w", pattern, err)
		}
		compiled.components[i] = component
	}
	return compiled, true, nil
}

func normalizePOSIXClasses(pattern string) string {
	classes := map[string]string{
		"alnum":  "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
		"alpha":  "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
		"blank":  " \t",
		"cntrl":  asciiRange(1, 31) + string(rune(127)),
		"digit":  "0123456789",
		"graph":  asciiRange(33, 126),
		"lower":  "abcdefghijklmnopqrstuvwxyz",
		"print":  asciiRange(32, 126),
		"punct":  `!"#$%&'()*+,-./:;<=>?@[\]^_` + "`" + `{|}~`,
		"space":  " \t\v\f\r\n",
		"upper":  "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"xdigit": "0123456789ABCDEFabcdef",
	}
	for name, characters := range classes {
		pattern = strings.ReplaceAll(pattern, "[:"+name+":]", escapeClassCharacters(characters))
	}
	return pattern
}

func asciiRange(first, last byte) string {
	result := make([]byte, 0, int(last-first)+1)
	for char := first; char <= last; char++ {
		result = append(result, char)
	}
	return string(result)
}

func escapeClassCharacters(characters string) string {
	var result strings.Builder
	for _, char := range characters {
		if char == '\\' || char == ']' || char == '-' || char == '^' {
			result.WriteByte('\\')
		}
		result.WriteRune(char)
	}
	return result.String()
}

func trimUnescapedTrailingSpaces(pattern string) string {
	for strings.HasSuffix(pattern, " ") {
		backslashes := 0
		for i := len(pattern) - 2; i >= 0 && pattern[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		pattern = strings.TrimSuffix(pattern, " ")
	}
	return pattern
}

func normalizeClassNegation(pattern string) string {
	var result strings.Builder
	escaped := false
	for i := 0; i < len(pattern); i++ {
		char := pattern[i]
		if escaped {
			result.WriteByte(char)
			escaped = false
			continue
		}
		if char == '\\' {
			result.WriteByte(char)
			escaped = true
			continue
		}
		result.WriteByte(char)
		if char == '[' && i+1 < len(pattern) && pattern[i+1] == '!' {
			result.WriteByte('^')
			i++
		}
	}
	return result.String()
}

func (m *Matcher) Ignored(name string, directory bool) bool {
	if m == nil {
		return false
	}
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	if m.prefix != "" {
		name = path.Join(m.prefix, name)
	}
	parts := strings.Split(name, "/")
	ignored := false
	for i := range parts {
		currentIsDirectory := i < len(parts)-1 || directory
		matchedIgnored := false
		for _, rule := range m.rules {
			if (!rule.directoryOnly || currentIsDirectory) && rule.matchesParts(parts[:i+1]) {
				matchedIgnored = !rule.negated
			}
		}
		ignored = ignored || matchedIgnored
	}
	return ignored
}

func (r rule) matchesParts(parts []string) bool {
	if !r.anchored {
		parts = parts[len(parts)-1:]
	}
	return matchComponents(r.components, parts)
}

func matchComponents(pattern, name []string) bool {
	hasDoubleStar := false
	for _, component := range pattern {
		if component == "**" {
			hasDoubleStar = true
			break
		}
	}
	if !hasDoubleStar {
		if len(pattern) != len(name) {
			return false
		}
		for i := range pattern {
			matched, err := filepath.Match(pattern[i], name[i])
			if err != nil || !matched {
				return false
			}
		}
		return true
	}

	type state struct {
		pattern int
		name    int
	}
	memo := make(map[state]bool)
	var match func(int, int) bool
	match = func(patternIndex, nameIndex int) bool {
		key := state{pattern: patternIndex, name: nameIndex}
		if matched, ok := memo[key]; ok {
			return matched
		}

		matched := false
		switch {
		case patternIndex == len(pattern):
			matched = nameIndex == len(name)
		case pattern[patternIndex] == "**":
			if patternIndex == len(pattern)-1 {
				matched = nameIndex < len(name)
			} else {
				matched = match(patternIndex+1, nameIndex) ||
					(nameIndex < len(name) && match(patternIndex, nameIndex+1))
			}
		case nameIndex < len(name):
			componentMatched, err := filepath.Match(pattern[patternIndex], name[nameIndex])
			matched = err == nil && componentMatched && match(patternIndex+1, nameIndex+1)
		}
		memo[key] = matched
		return matched
	}
	return match(0, 0)
}
