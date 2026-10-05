package daemon

import (
	"reflect"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestParseNVIDIA(t *testing.T) {
	out := []byte("NVIDIA H100 80GB HBM3, 81559\nNVIDIA GB10, [N/A]\n\n, 12\n")
	want := []proto.GPU{{Name: "NVIDIA H100 80GB HBM3", MemoryMiB: 81559}, {Name: "NVIDIA GB10"}}
	if got := parseNVIDIA(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}
