package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/termui"
)

func TestRunControlRemainsResponsiveWithBlockedStdout(t *testing.T) {
	for _, stderrTTY := range []bool{false, true} {
		t.Run(fmt.Sprintf("stderrTTY=%v", stderrTTY), func(t *testing.T) {
			delivered := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				delivered <- r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			writer := &gatedWriter{started: make(chan struct{}), release: make(chan struct{})}
			con := termui.New(writer, io.Discard, termui.Options{ErrTTY: stderrTTY})
			view := newRunView(RunOptions{Display: RunDisplay{UI: con}})
			view.preparing(RunOptions{NoSnapshot: true})
			ctx, cancel := context.WithCancel(context.Background())
			interrupts := make(chan os.Signal, 2)
			controller := startAdmittedJobController(ctx, interrupts, newInterruptTarget(server.URL, "job", "mini/job", view.report, testInterruptNotifications()))
			writeDone := make(chan struct{})
			go func() {
				_, _ = con.Out.Write([]byte("output"))
				close(writeDone)
			}()
			t.Cleanup(func() {
				cancel()
				close(writer.release)
				<-writeDone
				<-controller.done
			})
			<-writer.started
			for _, path := range []string{"/v0/jobs/job/signal", "/v0/jobs/job/kill"} {
				interrupts <- os.Interrupt
				select {
				case got := <-delivered:
					if got != path {
						t.Fatalf("control request = %q, want %q", got, path)
					}
				case <-time.After(time.Second):
					t.Fatalf("blocked stdout prevented %s", path)
				}
			}
		})
	}
}
