package termui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Bytes renders a byte count in binary units: 39 B, 6.1 KiB, 114 MiB.
func Bytes(n int64) string {
	if n < 0 {
		return "-"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for i, label := range units {
		value /= unit
		if value < unit || i == len(units)-1 {
			if value < 10 {
				return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + " " + label
			}
			return fmt.Sprintf("%.0f %s", value, label)
		}
	}
	return fmt.Sprintf("%d B", n)
}

// Count renders an integer with thousands separators.
func Count(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Plural picks the singular or plural noun for n.
func Plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Things renders "1 file", "3,545 files".
func Things(n int, one, many string) string { return Count(n) + " " + Plural(n, one, many) }

// Duration renders an elapsed time: 3ms, 0.8s, 12.7s, 4m12s, 1h03m, 2d4h.
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", d.Seconds()), ".0") + "s"
	case d < time.Hour:
		d = d.Round(time.Second)
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	case d < 24*time.Hour:
		d = d.Round(time.Minute)
		if d%time.Hour == 0 {
			return fmt.Sprintf("%dh", int(d/time.Hour))
		}
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		days, hours := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	}
}

// Age renders how long ago t was in list form: 45s, 2m, 3h, 4d. Older than a
// month, it prints the date.
func Age(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Local().Format("Jan 2")
	}
}

// Ago renders a relative time for sentences: "just now", "6m ago".
func Ago(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	if now.Sub(t) < 10*time.Second {
		return "just now"
	}
	age := Age(t, now)
	if strings.Contains(age, " ") {
		return "on " + age
	}
	return age + " ago"
}

// Clock renders a local time of day, with the date when it isn't today.
func Clock(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	lt, ln := t.Local(), now.Local()
	if lt.Year() == ln.Year() && lt.YearDay() == ln.YearDay() {
		return lt.Format("15:04")
	}
	return lt.Format("Jan 2 15:04")
}

// Timestamp renders an absolute local time for detail views.
func Timestamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("Jan 2 15:04:05")
}
