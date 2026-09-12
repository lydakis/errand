package config

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const maxEnvironmentFileBytes = 1 << 20

// Files are data, never shell programs. Errors identify only the file and line;
// neither malformed lines nor parsed values may reach diagnostics.
func readEnvironmentFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("environment file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("environment file %q must be a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("environment file %q: %w", path, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxEnvironmentFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading environment file %q: %w", path, err)
	}
	if len(raw) > maxEnvironmentFileBytes {
		return nil, fmt.Errorf("environment file %q exceeds 1 MiB", path)
	}
	if !utf8.Valid(raw) || strings.ContainsRune(string(raw), 0) {
		return nil, fmt.Errorf("environment file %q must be UTF-8 without NUL", path)
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimPrefix(string(raw), "\ufeff"), "\r\n", "\n"), "\n")
	values := map[string]string{}
	for i := 0; i < len(lines); i++ {
		lineNumber := i + 1
		line := strings.TrimLeft(lines[i], " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
			line = strings.TrimLeft(line[len("export"):], " \t")
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !environmentFileName(name) {
			return nil, fmt.Errorf("environment file %q line %d: expected NAME=VALUE", path, lineNumber)
		}
		value = strings.TrimLeft(value, " \t")
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			var valid bool
			value, valid = quotedEnvironmentValue(value, lines, &i)
			if !valid {
				return nil, fmt.Errorf("environment file %q line %d: invalid quoted value", path, lineNumber)
			}
		} else {
			value = strings.TrimSpace(value)
			for j := range len(value) {
				if value[j] == '#' && (j == 0 || value[j-1] == ' ' || value[j-1] == '\t') {
					value = strings.TrimSpace(value[:j])
					break
				}
			}
		}
		values[name] = value
	}
	return values, nil
}

func environmentFileName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c != '_' && !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func quotedEnvironmentValue(text string, lines []string, line *int) (string, bool) {
	quote := text[0]
	text = text[1:]
	var result strings.Builder
	for {
		for i := 0; i < len(text); i++ {
			c := text[i]
			if c == quote {
				rest := strings.TrimSpace(text[i+1:])
				return result.String(), rest == "" || strings.HasPrefix(rest, "#")
			}
			if c == '\\' && quote == '"' && i+1 < len(text) {
				i++
				switch text[i] {
				case 'n':
					result.WriteByte('\n')
				case 'r':
					result.WriteByte('\r')
				case 't':
					result.WriteByte('\t')
				case '\\', '"', '$':
					result.WriteByte(text[i])
				default:
					result.WriteByte('\\')
					result.WriteByte(text[i])
				}
			} else {
				result.WriteByte(c)
			}
		}
		(*line)++
		if *line >= len(lines) {
			return "", false
		}
		result.WriteByte('\n')
		text = lines[*line]
	}
}
