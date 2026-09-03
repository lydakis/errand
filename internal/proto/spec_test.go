package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestAndReceiptDoNotEmbedProtocolVersion(t *testing.T) {
	for name, value := range map[string]any{
		"request": Spec{Argv: []string{"true"}},
		"receipt": NewReceiptSpec(Spec{Argv: []string{"true"}}),
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"v":`) {
			t.Fatalf("%s embeds redundant protocol version: %s", name, raw)
		}
	}
}
