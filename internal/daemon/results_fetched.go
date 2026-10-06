package daemon

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// fetchedReceipt, written once in a job's receipt directory, records that a
// client downloaded the job's retained workspace changes whole.
const fetchedReceipt = "fetched.json"

type fetchedRecord struct {
	FetchedAt time.Time `json:"fetched_at"`
}

// hasResults reports whether res retained workspace changes for a client to
// fetch.
func hasResults(res *proto.Result) bool {
	return res != nil && res.Changes != nil && res.Changes.PathCount > 0
}

// fetchRecorded reports whether j's receipt directory holds the record a
// download of its retained changes leaves.
func fetchRecorded(j *Job) bool {
	_, err := os.Lstat(filepath.Join(j.Dir, fetchedReceipt))
	return err == nil
}

// noteResultsLocked counts j as a finished job whose retained changes no
// client has fetched, unless it has none or they were fetched. A runner with
// unfetched results is not idle: they end with its machine. d.mu must be
// held.
func (d *Daemon) noteResultsLocked(j *Job, res *proto.Result) {
	if j.unfetched || j.fetched || !hasResults(res) {
		return
	}
	j.unfetched = true
	d.unfetched++
}

// forgetResultsLocked stops counting j's results, which are fetched or gone.
// d.mu must be held.
func (d *Daemon) forgetResultsLocked(j *Job) {
	if j.unfetched {
		j.unfetched = false
		d.unfetched--
	}
}

// resultsFetched records that a client downloaded j's retained changes
// whole, and stops counting them. It records the download whether or not the
// job is counted yet: a download can finish after the job publishes its
// result but before it is counted, and the count then sees the record. The
// record outlives a restart; if it cannot be written, the results count again
// after one.
func (d *Daemon) resultsFetched(j *Job) {
	d.mu.Lock()
	first := !j.fetched
	j.fetched = true
	d.forgetResultsLocked(j)
	d.mu.Unlock()
	if !first {
		return
	}
	if err := j.writeJSON(fetchedReceipt, fetchedRecord{FetchedAt: time.Now().UTC()}); err != nil {
		log.Printf("job %s: recording fetched results: %v", j.ID, err)
	}
}
