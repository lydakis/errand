package daemon

import (
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestArtifactAdmissionPolicy(t *testing.T) {
	spec := proto.Spec{Argv: []string{"true"}, ManifestRoot: (proto.Manifest{}).RootHash(), Limits: proto.DefaultLimits(), ChangeClientID: testChangeClientID}
	for _, noSnapshot := range []bool{false, true} {
		spec.NoSnapshot = noSnapshot
		spec.Selection = proto.SelectionPolicy{Artifacts: []string{"reports"}}
		if err := validateSpec(spec, proto.DefaultLimits()); err != nil {
			t.Fatalf("valid declaration: %v", err)
		}
		spec.Selection.Artifacts = []string{"../reports"}
		if err := validateSpec(spec, proto.DefaultLimits()); err == nil {
			t.Fatal("accepted invalid artifact declaration")
		}
	}
}
