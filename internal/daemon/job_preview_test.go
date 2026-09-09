package daemon

import (
	"strconv"
	"strings"
	"testing"
)

func TestBoundedCommandPreservesQuotedPrefix(t *testing.T) {
	for _, argv := range [][]string{
		nil, {""}, {"", ""}, {"tool", "two words", "next"},
		{"tool", strings.Repeat("\u0000\"\\界", 1<<16), "later"},
		{strings.Repeat("a", 510) + "界"},
		{"\xff\xfe\x80\xc3", "tail"},
	} {
		quoted := make([]string, len(argv))
		for i, arg := range argv {
			quoted[i] = strconv.Quote(arg)
		}
		full := strings.Join(quoted, " ")
		for _, limit := range []int{0, 1, 2, 3, 8, 20, 512} {
			want, truncated := full, len(full) > limit
			if limit <= 0 {
				want, truncated = "", len(argv) > 0
			} else if truncated {
				want = markCommandTruncated(full, limit)
			}
			got, gotTruncated := boundedCommand(argv, limit)
			if got != want || gotTruncated != truncated {
				t.Fatalf("limit %d: %q (%t), want %q (%t)", limit, got, gotTruncated, want, truncated)
			}
		}
	}
}
