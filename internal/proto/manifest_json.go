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
	buf := make([]byte, 0, 32<<10)
	flush := func() {
		w.Write(buf)
		buf = buf[:0]
	}
	if m.Entries == nil {
		w.Write([]byte(`{"entries":null}`))
		return
	}
	buf = append(buf, `{"entries":[`...)
	for i, e := range m.Entries {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, `{"path":`...)
		buf = appendJSONString(buf, e.Path)
		buf = append(buf, `,"type":`...)
		buf = appendJSONString(buf, e.Type)
		buf = append(buf, `,"mode":`...)
		buf = strconv.AppendUint(buf, uint64(e.Mode), 10)
		if e.Size != 0 {
			buf = append(buf, `,"size":`...)
			buf = strconv.AppendInt(buf, e.Size, 10)
		}
		if e.SHA256 != "" {
			buf = append(buf, `,"sha256":`...)
			buf = appendJSONString(buf, e.SHA256)
		}
		if e.Target != "" {
			buf = append(buf, `,"target":`...)
			buf = appendJSONString(buf, e.Target)
		}
		buf = append(buf, '}')
		if len(buf) >= 16<<10 {
			flush()
		}
	}
	buf = append(buf, "]}"...)
	flush()
}

func appendJSONString(buf []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' || c >= utf8.RuneSelf {
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
