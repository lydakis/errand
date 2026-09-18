package changes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/snapshot"
)

// Instrumentation is inserted only into a temporary source copy by
// benchmark_apply_journal.py. Ordinary builds never call these probes.
// One operation per process gives individual observations, not b.N averages.
var applyJournalProbe struct {
	sync.Mutex
	active  bool
	metrics map[string]applyJournalMetric
}

type applyJournalMetric struct {
	Count int64
	NS    int64
	Bytes int64
}

func applyJournalRecord(name string, start time.Time, size int) {
	elapsed := time.Since(start).Nanoseconds()
	applyJournalProbe.Lock()
	defer applyJournalProbe.Unlock()
	if !applyJournalProbe.active {
		return
	}
	m := applyJournalProbe.metrics[name]
	m.Count++
	m.NS += elapsed
	m.Bytes += int64(size)
	applyJournalProbe.metrics[name] = m
}

func applyJournalSync(file *os.File, site string) error {
	start := time.Now()
	err := file.Sync()
	applyJournalRecord("barrier/"+site, start, 0)
	return err
}

func applyJournalValidate(j applyJournal) error {
	start := time.Now()
	err := validateApplyJournal(j)
	applyJournalRecord("journal/validate", start, 0)
	return err
}

func applyJournalEncode(j applyJournal) ([]byte, error) {
	start := time.Now()
	raw, err := json.MarshalIndent(j, "", "  ")
	applyJournalRecord("journal/encode", start, len(raw)+1)
	return raw, err
}

func BenchmarkApplyJournalScaling(b *testing.B) {
	for _, roots := range []int{1, 8, 32, 128, 512} {
		b.Run(fmt.Sprint(roots), func(b *testing.B) {
			if b.N != 1 {
				b.Fatal("use -benchtime=1x: retain individual apply observations")
			}
			const totalBytes = 1 << 20
			size := totalBytes / roots
			before := bytes.Repeat([]byte("a"), size)
			after := bytes.Repeat([]byte("b"), size)
			staged, dest, state := b.TempDir(), b.TempDir(), b.TempDir()
			base, remote := filepath.Join(staged, "base"), filepath.Join(staged, "remote")
			for _, dir := range []string{base, remote} {
				if err := os.Mkdir(dir, 0700); err != nil {
					b.Fatal(err)
				}
			}
			paths := make([]string, roots)
			for i := range paths {
				paths[i] = fmt.Sprintf("file-%04d", i)
				for _, dir := range []string{base, remote, dest} {
					body := before
					if dir == remote {
						body = after
					}
					f, err := os.OpenFile(filepath.Join(dir, paths[i]), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
					if err != nil {
						b.Fatal(err)
					}
					_, writeErr := f.Write(body)
					syncErr := syncStagedData(f)
					closeErr := f.Close()
					for _, err := range []error{writeErr, syncErr, closeErr} {
						if err != nil {
							b.Fatal(err)
						}
					}
				}
			}
			// Drain setup writes before starting either timing or attribution.
			for _, dir := range []string{base, remote, dest, staged, state} {
				if err := syncDirectory(dir); err != nil {
					b.Fatal(err)
				}
			}
			bm, err := snapshot.Build(base, paths)
			if err != nil {
				b.Fatal(err)
			}
			rm, err := snapshot.Build(remote, paths)
			if err != nil {
				b.Fatal(err)
			}
			bundle, err := PrepareSourceDelta(b.Context(), bm, rm, 4*totalBytes)
			if err != nil {
				b.Fatal(err)
			}
			if !reflect.DeepEqual(bundle.Paths, paths) {
				b.Fatalf("unexpected change roots: %v", bundle.Paths)
			}
			id, err := applyWorkspaceIdentity(dest)
			if err != nil {
				b.Fatal(err)
			}
			target := TransferTarget{Root: dest, RootID: id, Owner: "apply-journal-benchmark", StatePath: filepath.Join(state, "receipt.json")}
			applyJournalProbe.Lock()
			applyJournalProbe.metrics = make(map[string]applyJournalMetric)
			applyJournalProbe.active = true
			applyJournalProbe.Unlock()
			b.ResetTimer()
			result, err := target.Apply(staged, bundle, nil, ApplyOptions{})
			b.StopTimer()
			applyJournalProbe.Lock()
			applyJournalProbe.active = false
			metrics := applyJournalProbe.metrics
			applyJournalProbe.Unlock()
			if err != nil {
				b.Fatal(err)
			}
			if !reflect.DeepEqual(result.Applied, paths) || len(result.Conflicts) != 0 || len(result.States) != roots {
				b.Fatalf("incorrect apply receipt: %+v", result)
			}
			for _, name := range paths {
				got, err := os.ReadFile(filepath.Join(dest, name))
				if err != nil || !bytes.Equal(got, after) {
					b.Fatalf("incorrect installed body %s: %v", name, err)
				}
			}
			if pending, err := WorkspaceHasApplyTransactions(dest); err != nil || pending {
				b.Fatalf("pending transaction: %v %v", pending, err)
			}
			// A durable receipt must replay without overwriting a later edit.
			if err := os.WriteFile(filepath.Join(dest, paths[0]), []byte("later edit"), 0600); err != nil {
				b.Fatal(err)
			}
			retry, err := target.Apply(staged, bundle, nil, ApplyOptions{})
			if err != nil || !reflect.DeepEqual(retry, result) {
				b.Fatalf("retry differs: %+v %v", retry, err)
			}
			got, err := os.ReadFile(filepath.Join(dest, paths[0]))
			if err != nil || string(got) != "later edit" {
				b.Fatalf("retry overwrote later edit: %v", err)
			}
			if len(metrics) != 0 {
				publications := int64(3)
				if roots == 1 {
					publications = 4
				}
				if m := metrics["journal/encode"]; m.Count != publications {
					b.Fatalf("expected %d publications, got %d", publications, m.Count)
				}
				for name, m := range metrics {
					b.ReportMetric(float64(m.Count), name+"-count/op")
					b.ReportMetric(float64(m.NS), name+"-ns/op")
					if m.Bytes != 0 {
						b.ReportMetric(float64(m.Bytes), name+"-bytes/op")
					}
				}
			}
			b.ReportMetric(totalBytes, "changed-bytes/op")
		})
	}
}
