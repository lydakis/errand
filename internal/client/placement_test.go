package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestWhereRetryOnlyOnCertainPreAdmissionRejection(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		status               int
		loseFirst, wantRetry bool
	}{
		{"capacity", 429, false, true}, {"requirements", 412, false, true},
		{"permission", 403, false, false}, {"server failure", 503, false, false},
		{"ambiguous then capacity", 429, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var puts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					_, _ = io.Copy(io.Discard, r.Body)
					if puts.Add(1) == 1 && tc.loseFirst {
						panic(http.ErrAbortHandler)
					}
					http.Error(w, "refused", tc.status)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			var fallback atomic.Int32
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					fallback.Add(1)
				}
				http.Error(w, "refused", 403)
			}))
			defer second.Close()
			code := Run(RunOptions{PeerURL: server.URL, Root: t.TempDir(), NoSnapshot: true, Where: "*", Argv: []string{"true"}, Stdout: io.Discard, Stderr: io.Discard, Candidates: []RunTarget{{PeerURL: server.URL}, {PeerURL: second.URL}}})
			if puts.Load() == 0 || code == 0 || (fallback.Load() > 0) != tc.wantRetry {
				t.Fatalf("code=%d puts=%d retry=%v", code, puts.Load(), fallback.Load() > 0)
			}
		})
	}
}

func TestWhereInterruptDuringRejectedSubmissionStopsFallback(t *testing.T) {
	signals := make(chan os.Signal, 1)
	forwarded := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/signal") {
			once.Do(func() { close(forwarded) })
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		signals <- os.Interrupt
		select {
		case <-forwarded:
		case <-r.Context().Done():
			return
		}
		http.Error(w, "capacity filled", 429)
	}))
	defer server.Close()
	selected := 0
	opts := RunOptions{Where: "*", Root: t.TempDir(), NoSnapshot: true, Argv: []string{"true"}, Stdout: io.Discard, Stderr: io.Discard, Candidates: []RunTarget{{PeerURL: server.URL}, {PeerURL: server.URL}}, OnSelected: func(RunTarget) { selected++ }}
	code := runWithDetachNotifications(opts, signals, newInterruptNotifications(func() {}, func() {}), context.Background().Done())
	if code != 130 || selected != 1 {
		t.Fatalf("cancelled submission retried: code=%d selected=%d", code, selected)
	}
}

func TestWhereFallbackReusesSnapshotAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name               string
		create, noSnapshot bool
	}{{"run snapshot", false, false}, {"workspace snapshot", true, false}, {"run environment", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			create := tc.create
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, ".errandignore"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "input"), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("WHERE_TEST_VALUE", "original")
			manifests := make(chan proto.Manifest, 2)
			envs := make(chan string, 2)
			server := func(status int) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
						http.NotFound(w, r)
						return
					}
					mr, err := r.MultipartReader()
					if err != nil {
						t.Error(err)
						http.Error(w, "multipart", 400)
						return
					}
					part, err := mr.NextPart()
					if err != nil {
						t.Error(err)
						return
					}
					if !create {
						var spec proto.Spec
						if err := json.NewDecoder(part).Decode(&spec); err != nil {
							t.Error(err)
						}
						envs <- spec.Env["WHERE_TEST_VALUE"]
					}
					part, err = mr.NextPart()
					if err != nil {
						t.Error(err)
						return
					}
					var manifest proto.Manifest
					if err := json.NewDecoder(part).Decode(&manifest); err != nil {
						t.Error(err)
					}
					manifests <- manifest
					_, _ = io.Copy(io.Discard, r.Body)
					http.Error(w, "refused", status)
				}))
			}
			first, second := server(412), server(403)
			defer first.Close()
			defer second.Close()
			var diagnostics bytes.Buffer
			selected := 0
			opts := RunOptions{Where: "*", NoSnapshot: tc.noSnapshot, Root: root, Argv: []string{"true"}, PassEnv: []string{"WHERE_TEST_VALUE"}, Stdout: io.Discard, Stderr: &diagnostics, Candidates: []RunTarget{{PeerURL: first.URL}, {PeerURL: second.URL}}, OnSelected: func(target RunTarget) {
				selected++
				if target.PeerURL == second.URL {
					if err := os.WriteFile(filepath.Join(root, "later-file"), []byte("not part of this request"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Setenv("WHERE_TEST_VALUE", "later-value"); err != nil {
						t.Fatal(err)
					}
				}
			}}
			if create {
				if _, err := CreateWorkspace(opts, "experiment"); err == nil {
					t.Fatal("expected refusal")
				} else {
					diagnostics.WriteString(err.Error())
				}
			} else if code := Run(opts); code != ExitTransaction {
				t.Fatalf("code=%d", code)
			}
			if selected != 2 {
				t.Fatalf("selected %d candidates", selected)
			}
			if tc.noSnapshot {
				if len(manifests) != 2 || len(envs) != 2 {
					t.Fatalf("attempts=%d envs=%d", len(manifests), len(envs))
				}
				if <-envs != "original" || <-envs != "original" {
					t.Fatal("fallback reread ambient environment")
				}
			} else {
				if len(manifests) != 1 || !strings.Contains(diagnostics.String(), "selection policy changed") {
					t.Fatalf("fallback rebuilt a changed snapshot: uploads=%d diagnostics=%s", len(manifests), &diagnostics)
				}
			}

		})
	}
}

func TestWorkspacePlacementDoesNotRetryUncertainOrOtherRejections(t *testing.T) {
	for _, status := range []int{403, 429, 503, 0} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if status == 0 {
					panic(http.ErrAbortHandler)
				}
				http.Error(w, "refused", status)
			}))
			defer first.Close()
			var attempts int
			_, err := CreateWorkspace(RunOptions{Root: t.TempDir(), NoSnapshot: true, Where: "*", Candidates: []RunTarget{{PeerURL: first.URL}, {PeerURL: first.URL}}, OnSelected: func(RunTarget) { attempts++ }}, "experiment")
			if err == nil || attempts != 1 {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
}
