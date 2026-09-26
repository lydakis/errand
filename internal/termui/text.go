package termui

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ShortIDLength is how many ULID characters identify a job on screen. The
// first ten encode the admission millisecond, so twelve are unique in
// practice and still resolve as a prefix.
const ShortIDLength = 12

// ShortID shortens a job ULID for display.
func ShortID(id string) string {
	if len(id) > ShortIDLength {
		return id[:ShortIDLength]
	}
	return id
}

// CellWidth is the number of terminal cells s occupies, ignoring SGR escapes.
func CellWidth(s string) int {
	width := 0
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		width += RuneWidth(r)
		i += size
	}
	return width
}

// RuneWidth follows East Asian Width closely enough for errand's output:
// wide and fullwidth ranges and pictographs take two cells, combining marks
// none, everything else one.
func RuneWidth(r rune) int {
	switch {
	case r == 0, unicode.IsControl(r), unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), r == '‍', r == '️':
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115f, r >= 0x2e80 && r <= 0x303e, r >= 0x3041 && r <= 0x33ff,
		r >= 0x3400 && r <= 0x4dbf, r >= 0x4e00 && r <= 0x9fff, r >= 0xa000 && r <= 0xa4cf,
		r >= 0xac00 && r <= 0xd7a3, r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f, r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	default:
		return 1
	}
}

// escapeLen returns the byte length of an SGR/CSI escape at the start of s.
func escapeLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b || s[1] != '[' {
		return 0
	}
	for i := 2; i < len(s); i++ {
		if c := s[i]; c >= 0x40 && c <= 0x7e {
			return i + 1
		}
	}
	return 0
}

// StripANSI removes SGR escapes.
func StripANSI(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TruncateANSI cuts s to at most width cells, ending with "…" when cut and
// resetting any open style.
func TruncateANSI(s string, width int) string {
	if width <= 0 || CellWidth(s) <= width {
		return s
	}
	var b strings.Builder
	used := 0
	styled := false
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			b.WriteString(s[i : i+n])
			styled = s[i:i+n] != "\x1b[0m"
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w := RuneWidth(r)
		if used+w > width-1 {
			break
		}
		b.WriteRune(r)
		used += w
		i += size
	}
	b.WriteString("…")
	if styled {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// Truncate cuts plain text to width cells with a trailing "…".
func Truncate(s string, width int) string { return TruncateANSI(s, width) }

// ShellQuote renders argv the way a person would type it in a shell.
func ShellQuote(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = shellWord(arg)
	}
	return strings.Join(parts, " ")
}

func shellWord(arg string) string {
	if arg == "" {
		return "''"
	}
	safe := true
	for _, r := range arg {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return arg
	}
	if strings.IndexFunc(arg, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
		return strconv.Quote(arg)
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// ParseQuotedArgv reverses the runner's job-list command rendering, a series
// of Go-quoted strings. It returns false when the text isn't in that form.
func ParseQuotedArgv(s string) ([]string, bool) {
	var argv []string
	rest := strings.TrimSpace(s)
	for rest != "" {
		prefix, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return nil, false
		}
		arg, err := strconv.Unquote(prefix)
		if err != nil {
			return nil, false
		}
		argv = append(argv, arg)
		rest = strings.TrimLeft(rest[len(prefix):], " ")
	}
	return argv, len(argv) > 0
}

// Suggest returns the candidate closest to input when it is a likely typo.
func Suggest(input string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := editDistance(strings.ToLower(input), strings.ToLower(c)); d < bestDist {
			best, bestDist = c, d
		}
	}
	if best != "" && bestDist >= len(input) {
		return ""
	}
	return best
}

// editDistance is the optimal-string-alignment distance: insertions,
// deletions, substitutions and adjacent transpositions each cost one.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	d := make([][]int, len(ra)+1)
	for i := range d {
		d[i] = make([]int, len(rb)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(ra)][len(rb)]
}
