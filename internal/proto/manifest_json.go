package proto

import (
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

// writeManifestJSON streams exactly the bytes json.Marshal(m) produces, so
// RootHash keeps its wire identity without reflection or a whole-manifest
// buffer. Strings that need any escaping are delegated to json.Marshal.
func writeManifestJSON(w io.Writer, m Manifest) {
	if m.Entries == nil {
		w.Write([]byte(`{"entries":null}`))
		return
	}
	buf := make([]byte, 0, 32<<10)
	buf = append(buf, `{"entries":[`...)
	for i, e := range m.Entries {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendManifestEntryJSON(buf, e)
		if len(buf) >= 16<<10 {
			w.Write(buf)
			buf = buf[:0]
		}
	}
	buf = append(buf, "]}"...)
	w.Write(buf)
}

// AppendManifestJSON appends exactly the bytes json.Marshal(m) produces.
func AppendManifestJSON(buf []byte, m Manifest) []byte {
	if m.Entries == nil {
		return append(buf, `{"entries":null}`...)
	}
	buf = append(buf, `{"entries":[`...)
	for i, e := range m.Entries {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendManifestEntryJSON(buf, e)
	}
	return append(buf, "]}"...)
}

// ManifestJSONSize returns the length of AppendManifestJSON's output when no
// string needs escaping, and less than it otherwise, so callers can size a
// buffer once.
func ManifestJSONSize(m Manifest) int {
	if m.Entries == nil {
		return len(`{"entries":null}`)
	}
	n := len(`{"entries":[]}`)
	for i, e := range m.Entries {
		if i > 0 {
			n++ // ,
		}
		n += len(`{"path":"","type":"","mode":}`) + len(e.Path) + len(e.Type) + decimalLen(int64(e.Mode))
		if e.Size != 0 {
			n += len(`,"size":`) + decimalLen(e.Size)
		}
		if e.SHA256 != "" {
			n += len(`,"sha256":""`) + len(e.SHA256)
		}
		if e.Target != "" {
			n += len(`,"target":""`) + len(e.Target)
		}
	}
	return n
}

// decimalLen is the length of strconv.FormatInt(v, 10).
func decimalLen(v int64) int {
	n, u := 1, uint64(v)
	if v < 0 {
		n, u = 2, -u
	}
	for ; u >= 10; u /= 10 {
		n++
	}
	return n
}

func appendManifestEntryJSON(buf []byte, e ManifestEntry) []byte {
	buf = append(buf, `{"path":`...)
	buf = AppendJSONString(buf, e.Path)
	buf = append(buf, `,"type":`...)
	buf = AppendJSONString(buf, e.Type)
	buf = append(buf, `,"mode":`...)
	buf = strconv.AppendUint(buf, uint64(e.Mode), 10)
	if e.Size != 0 {
		buf = append(buf, `,"size":`...)
		buf = strconv.AppendInt(buf, e.Size, 10)
	}
	if e.SHA256 != "" {
		buf = append(buf, `,"sha256":`...)
		buf = AppendJSONString(buf, e.SHA256)
	}
	if e.Target != "" {
		buf = append(buf, `,"target":`...)
		buf = AppendJSONString(buf, e.Target)
	}
	return append(buf, '}')
}

// jsonPlain marks the bytes json.Marshal copies into a string unescaped:
// printable ASCII other than the quote, the backslash and the HTML characters.
var jsonPlain = func() (plain [256]bool) {
	for c := 0x20; c < utf8.RuneSelf; c++ {
		plain[c] = c != '"' && c != '\\' && c != '<' && c != '>' && c != '&'
	}
	return plain
}()

// AppendJSONString appends exactly the bytes json.Marshal(s) produces.
func AppendJSONString(buf []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if !jsonPlain[s[i]] {
			encoded, err := json.Marshal(s)
			if err != nil {
				panic(err) // strings always marshal
			}
			return append(buf, encoded...)
		}
	}
	buf = append(buf, '"')
	buf = append(buf, s...)
	return append(buf, '"')
}
