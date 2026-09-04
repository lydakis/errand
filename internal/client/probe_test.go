package client

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

func TestProbeInfoRequiresCurrentProtocolField(t *testing.T) {
	for name, body := range map[string]string{
		"missing": `{"version":"not-errand"}`,
		"wrong":   fmt.Sprintf(`{"proto":%d,"version":"future"}`, proto.ProtoVersion+1),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, body)
			}))
			t.Cleanup(server.Close)
			_, err := ProbeInfo(context.Background(), server.URL, time.Second)
			if kind, ok := ProbeKindOf(err); !ok || kind != ProbeNotErrand {
				t.Fatalf("ProbeInfo error = %v, kind = %q, want not-errand", err, kind)
			}
		})
	}
}

func TestProbeInfoRejectsIncompleteOrOversizedResponses(t *testing.T) {
	valid := []byte(fmt.Sprintf(`{"proto":%d,"version":"test"}`, proto.ProtoVersion))
	tests := map[string]http.HandlerFunc{
		"incomplete": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(valid)+10))
			w.Write(valid)
		},
		"oversized": func(w http.ResponseWriter, _ *http.Request) {
			w.Write(append(valid, bytes.Repeat([]byte(" "), 1<<20)...))
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			t.Cleanup(server.Close)
			_, err := ProbeInfo(context.Background(), server.URL, time.Second)
			if kind, ok := ProbeKindOf(err); !ok || kind != ProbeNotErrand {
				t.Fatalf("ProbeInfo error = %v, kind = %q, want not-errand", err, kind)
			}
		})
	}
}
