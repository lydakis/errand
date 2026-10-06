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

// retainsResults reports whether res, j's result, retained workspace changes
// for a client to fetch that end with the machine. A job on a persistent
// workspace changed the workspace, which outlives the job, so its changes
// are not counted: persistent workspaces end with a lease without holding
// it.
func retainsResults(j *Job, res *proto.Result) bool {
	return j.Spec.WorkspaceID == "" && res != nil && res.Changes != nil && res.Changes.PathCount > 0
}

// fetchRecorded reports whether j's receipt directory holds the record a
// download of its retained changes leaves.
func fetchRecorded(j *Job) bool {
	_, err := os.Lstat(filepath.Join(j.Dir, fetchedReceipt))
	return err == nil
}

// noteResultsLocked counts j as a finished job whose retained changes no
// client has fetched, unless it has none, they were fetched, or j is no
// longer the runner's. A runner with unfetched results is not idle: they end
// with its machine. It is called in the critical section that publishes res,
// so nothing sees the result before it is counted. d.mu must be held.
func (d *Daemon) noteResultsLocked(j *Job, res *proto.Result) {
	if j.fetched || d.jobs[j.ID] != j || !retainsResults(j, res) {
		return
	}
	d.unfetched[j] = j.Admission.Time
}

// forgetResultsLocked stops counting j's results, which are fetched or gone;
// it does nothing if they are not counted. d.mu must be held.
func (d *Daemon) forgetResultsLocked(j *Job) {
	delete(d.unfetched, j)
}

// latestUnfetchedLocked is when the runner admitted the most recent job whose
// results are counted, or zero when none are. A cloud peer compares it with
// the runner's latest admission when its lease became ready, so only jobs
// admitted during the lease hold it. d.mu must be held.
func (d *Daemon) latestUnfetchedLocked() time.Time {
	var latest time.Time
	for _, admitted := range d.unfetched {
		if admitted.After(latest) {
			latest = admitted
		}
	}
	return latest
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
