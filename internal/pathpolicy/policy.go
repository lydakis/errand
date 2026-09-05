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
	prefix   string
	rules    []rule
	caseFold bool
}

func Compile(policy proto.SelectionPolicy) (*Matcher, error) {
	if err := ValidateCaches(policy.Caches); err != nil {
		return nil, err
	}
	if err := ValidateArtifacts(policy.Artifacts); err != nil {
		return nil, err
	}
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
	return &Matcher{prefix: policy.Prefix, rules: rules, caseFold: policy.CaseFold}, nil
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
	compiled.components = splitPattern(pattern)
	return compiled, true, nil
}

func splitPattern(pattern string) []string {
	var components []string
	start := 0
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '/' {
			continue
		}
		component := pattern[start:i]
		backslashes := 0
		for j := len(component) - 1; j >= 0 && component[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			component = component[:len(component)-1]
		}
		components = append(components, component)
		start = i + 1
	}
	return append(components, pattern[start:])
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
			if (!rule.directoryOnly || currentIsDirectory) && rule.matchesParts(parts[:i+1], m.caseFold) {
				matchedIgnored = !rule.negated
			}
		}
		ignored = ignored || matchedIgnored
	}
	return ignored
}

func (r rule) matchesParts(parts []string, caseFold bool) bool {
	if !r.anchored {
		parts = parts[len(parts)-1:]
	}
	return matchComponents(r.components, parts, caseFold)
}

func matchComponents(pattern, name []string, caseFold bool) bool {
	hasDoubleStar := false
	for _, component := range pattern {
		if isDoubleStar(component) {
			hasDoubleStar = true
			break
		}
	}
	if !hasDoubleStar {
		if len(pattern) != len(name) {
			return false
		}
		for i := range pattern {
			if !matchSegment(pattern[i], name[i], caseFold) {
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
		case isDoubleStar(pattern[patternIndex]):
			if patternIndex == len(pattern)-1 {
				matched = nameIndex < len(name)
			} else {
				matched = match(patternIndex+1, nameIndex) ||
					(nameIndex < len(name) && match(patternIndex, nameIndex+1))
			}
		case nameIndex < len(name):
			matched = matchSegment(pattern[patternIndex], name[nameIndex], caseFold) &&
				match(patternIndex+1, nameIndex+1)
		}
		memo[key] = matched
		return matched
	}
	return match(0, 0)
}

func isDoubleStar(component string) bool {
	return len(component) >= 2 && strings.Trim(component, "*") == ""
}

func matchSegment(pattern, name string, caseFold bool) bool {
	patternIndex, nameIndex := 0, 0
	starPattern, starName := -1, -1
	for nameIndex < len(name) {
		if patternIndex < len(pattern) {
			switch pattern[patternIndex] {
			case '*':
				for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
					patternIndex++
				}
				starPattern, starName = patternIndex, nameIndex
				continue
			case '?':
				patternIndex++
				nameIndex++
				continue
			case '\\':
				if patternIndex+1 < len(pattern) &&
					equalByte(pattern[patternIndex+1], name[nameIndex], caseFold) {
					patternIndex += 2
					nameIndex++
					continue
				}
			case '[':
				classMatched, next, valid := matchClass(pattern, patternIndex, name[nameIndex], caseFold)
				if valid && classMatched {
					patternIndex = next
					nameIndex++
					continue
				}
			default:
				if equalByte(pattern[patternIndex], name[nameIndex], caseFold) {
					patternIndex++
					nameIndex++
					continue
				}
			}
		}
		if starPattern < 0 {
			return false
		}
		starName++
		nameIndex = starName
		patternIndex = starPattern
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

func matchClass(pattern string, start int, value byte, caseFold bool) (bool, int, bool) {
	i := start + 1
	if i == len(pattern) {
		return false, 0, false
	}
	negated := pattern[i] == '!' || pattern[i] == '^'
	if negated {
		i++
	}
	matched := false
	var previous byte
	if i < len(pattern) && pattern[i] == ']' {
		matched = equalByte(']', value, caseFold)
		previous = ']'
		i++
	}
	for i < len(pattern) && pattern[i] != ']' {
		current := pattern[i]
		switch {
		case current == '\\':
			i++
			if i == len(pattern) {
				return false, 0, false
			}
			current = pattern[i]
			matched = matched || equalByte(current, value, caseFold)
			previous = current
		case current == '-' && previous != 0 && i+1 < len(pattern) && pattern[i+1] != ']':
			i++
			end := pattern[i]
			if end == '\\' {
				i++
				if i == len(pattern) {
					return false, 0, false
				}
				end = pattern[i]
			}
			matched = matched || inRange(value, previous, end, caseFold)
			previous = 0
		case current == '[' && i+1 < len(pattern) && pattern[i+1] == ':':
			end := strings.Index(pattern[i+2:], ":]")
			if end < 0 {
				matched = matched || equalByte('[', value, caseFold)
				previous = '['
				break
			}
			end += i + 2
			classMatched, valid := matchPOSIXClass(pattern[i+2:end], value, caseFold)
			if !valid {
				return false, 0, false
			}
			matched = matched || classMatched
			i = end + 1
			previous = 0
		default:
			matched = matched || equalByte(current, value, caseFold)
			previous = current
		}
		i++
	}
	if i == len(pattern) {
		return false, 0, false
	}
	if negated {
		matched = !matched
	}
	return matched, i + 1, true
}

func equalByte(left, right byte, caseFold bool) bool {
	return foldASCII(left, caseFold) == foldASCII(right, caseFold)
}

func foldASCII(value byte, enabled bool) byte {
	if enabled && value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}

func inRange(value, start, end byte, caseFold bool) bool {
	foldedValue := foldASCII(value, caseFold)
	foldedStart := foldASCII(start, caseFold)
	foldedEnd := foldASCII(end, caseFold)
	if foldedValue >= foldedStart && foldedValue <= foldedEnd {
		return true
	}
	return caseFold && value >= 'a' && value <= 'z' && value-('a'-'A') >= start && value-('a'-'A') <= end
}

func matchPOSIXClass(name string, value byte, caseFold bool) (bool, bool) {
	switch name {
	case "alnum":
		return isASCIIAlpha(value) || isASCIIDigit(value), true
	case "alpha":
		return isASCIIAlpha(value), true
	case "blank":
		return value == ' ' || value == '\t', true
	case "cntrl":
		return value <= 0x1f || value == 0x7f, true
	case "digit":
		return isASCIIDigit(value), true
	case "graph":
		return value > 0x20 && value <= 0x7e, true
	case "lower":
		return value >= 'a' && value <= 'z' || caseFold && value >= 'A' && value <= 'Z', true
	case "print":
		return value >= 0x20 && value <= 0x7e, true
	case "punct":
		return value > 0x20 && value <= 0x7e && !isASCIIAlpha(value) && !isASCIIDigit(value), true
	case "space":
		return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\f' || value == '\v', true
	case "upper":
		return value >= 'A' && value <= 'Z' || caseFold && value >= 'a' && value <= 'z', true
	case "xdigit":
		return isASCIIDigit(value) || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F', true
	default:
		return false, false
	}
}

func isASCIIAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isASCIIDigit(value byte) bool {
	return value >= '0' && value <= '9'
}
