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

// A machine resolved for a run alone is given up exactly when no job was
// admitted there: nothing was submitted, or the runner definitely refused.
// A job that was admitted, or whose answer was lost, keeps it.
func TestRunSettlesItsClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		root   bool
		answer func(http.ResponseWriter)
		giveUp bool
	}{
		{"fails before submitting", false, nil, true}, // change state needs a workspace root
		{"refused", true, func(w http.ResponseWriter) { http.Error(w, "refused", 403) }, true},
		{"answer lost", true, func(http.ResponseWriter) { panic(http.ErrAbortHandler) }, false},
		{"admitted", true, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(proto.JobStatus{State: proto.StateQueued})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var puts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut {
					http.NotFound(w, r)
					return
				}
				puts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				tc.answer(w)
			}))
			defer server.Close()
			root := ""
			if tc.root {
				root = t.TempDir()
			}
			var givenUp atomic.Int32
			Run(RunOptions{Where: "gpu", Root: root, NoSnapshot: true, Detach: true, Argv: []string{"true"}, Stdout: io.Discard, Stderr: io.Discard, Resolve: func() ([]RunTarget, error) {
				return []RunTarget{{PeerURL: server.URL, PeerName: "lease", Claim: NewClaim(func() { givenUp.Add(1) })}}, nil
			}})
			if want := map[bool]int32{true: 1, false: 0}[tc.giveUp]; givenUp.Load() != want || (puts.Load() > 0) != tc.root {
				t.Fatalf("given up %d times, want %d (puts %d)", givenUp.Load(), want, puts.Load())
			}
		})
	}
}

// Creating a workspace on a machine resolved for it alone settles the claim
// the same way.
func TestCreateWorkspaceSettlesItsClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter, *http.Request)
		giveUp bool
	}{
		{"refused", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "refused", 403) }, true},
		{"answer lost", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }, false},
		{"server failure", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "broken", 500) }, false},
		{"created", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(proto.Workspace{ID: strings.TrimPrefix(r.URL.Path, "/v0/workspaces/"), Name: "train"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || strings.HasSuffix(r.URL.Path, "/diff") {
					http.NotFound(w, r)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				tc.answer(w, r)
			}))
			defer server.Close()
			var givenUp atomic.Int32
			CreateWorkspace(RunOptions{Where: "gpu", Root: t.TempDir(), NoSnapshot: true, Stderr: io.Discard, Resolve: func() ([]RunTarget, error) {
				return []RunTarget{{PeerURL: server.URL, PeerName: "lease", Claim: NewClaim(func() { givenUp.Add(1) })}}, nil
			}}, "train")
			if want := map[bool]int32{true: 1, false: 0}[tc.giveUp]; givenUp.Load() != want {
				t.Fatalf("given up %d times, want %d", givenUp.Load(), want)
			}
		})
	}
}
