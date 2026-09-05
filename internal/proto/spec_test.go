package proto

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestArtifactDeclarationsPersistAndAffectAdmissionIdentity(t *testing.T) {
	spec := Spec{Argv: []string{"true"}}
	original := spec.Digest()
	spec.Selection.Artifacts = []string{"reports"}
	if spec.Digest() == original {
		t.Fatal("artifact declaration did not change admission identity")
	}
	raw, err := json.Marshal(NewReceiptSpec(spec))
	if err != nil {
		t.Fatal(err)
	}
	var receipt ReceiptSpec
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(receipt.SpecWithoutEnv().Selection.Artifacts, spec.Selection.Artifacts) {
		t.Fatal("receipt lost artifact declarations")
	}
}

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
