package workspace

import (
	"fmt"
	"strconv"
	"strings"
)

// Session describes attachment preferences, never durable job state.
// A nil Forward list inherits; an explicit empty list clears defaults.
type Session struct {
	Forward []string `toml:"forward"`
}

func (s *Session) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("session must be a table")
	}
	*s = Session{}
	for key, raw := range table {
		if key != "forward" {
			return fmt.Errorf("unsupported session setting %q", key)
		}
		values, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("session.forward must be an array of port mappings")
		}
		s.Forward = make([]string, 0, len(values))
		for _, raw := range values {
			value, ok := raw.(string)
			if !ok {
				return fmt.Errorf("session.forward must contain only strings")
			}
			s.Forward = append(s.Forward, value)
		}
	}
	return ValidatePortForwards(s.Forward)
}

// ParsePortForward accepts only [LOCAL:]REMOTE TCP port numbers.
func ParsePortForward(value string) (local, remote uint16, err error) {
	localText, remoteText, hasLocal := strings.Cut(value, ":")
	if !hasLocal {
		remoteText = localText
	}
	if localText == "" || remoteText == "" || strings.Contains(remoteText, ":") {
		return 0, 0, fmt.Errorf("forward wants [LOCAL:]REMOTE, got %q", value)
	}
	parse := func(label, text string) (uint16, error) {
		port, err := strconv.ParseUint(text, 10, 16)
		if err != nil || port == 0 {
			return 0, fmt.Errorf("%s port %q must be between 1 and 65535", label, text)
		}
		return uint16(port), nil
	}
	local, err = parse("local", localText)
	if err != nil {
		return 0, 0, err
	}
	remote, err = parse("remote", remoteText)
	return local, remote, err
}

func ValidatePortForwards(values []string) error {
	seen := map[uint16]bool{}
	for _, value := range values {
		local, _, err := ParsePortForward(value)
		if err != nil {
			return err
		}
		if seen[local] {
			return fmt.Errorf("local port %d is forwarded more than once", local)
		}
		seen[local] = true
	}
	return nil
}
