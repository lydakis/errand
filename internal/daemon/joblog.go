package daemon

import (
	"sync"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// JobLogKind names a job lifecycle moment worth one line in the runner log.
type JobLogKind string

const (
	JobLogQueued   JobLogKind = "queued"
	JobLogStarted  JobLogKind = "started"
	JobLogFinished JobLogKind = "finished"
	JobLogDropped  JobLogKind = "dropped"
)

// JobLogEvent is what `errand serve` prints for each job lifecycle moment.
// It carries no environment values or workspace contents.
type JobLogEvent struct {
	Kind       JobLogKind
	Time       time.Time
	ID         string
	Owner      string
	Project    string
	Argv       []string
	QueueAhead int
	Result     *proto.Result
	Dropped    uint64 // aggregated lifecycle events discarded while output stalled
}

// logJob queues a lifecycle event for Config.JobLog without waiting on it.
// A consumer that stalls (a backpressured stderr pipe) must never delay a
// job's start, its runtime limit, or its result.
func (d *Daemon) logJob(kind JobLogKind, j *Job, res *proto.Result) {
	if d.jobLog == nil {
		return
	}
	d.jobLog.push(newJobLogEvent(kind, j, res))
}

// logQueuedLocked records that j must wait for a slot. It runs under d.mu,
// so the event is queued before the drain worker can start j and log that.
func (d *Daemon) logQueuedLocked(j *Job, ahead int) {
	if d.jobLog == nil {
		return
	}
	event := newJobLogEvent(JobLogQueued, j, nil)
	event.QueueAhead = ahead
	d.jobLog.push(event)
}

func newJobLogEvent(kind JobLogKind, j *Job, res *proto.Result) JobLogEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	event := JobLogEvent{
		Kind: kind, Time: time.Now(), ID: j.ID, Owner: admissionOwner(j.Admission),
		Project: j.Admission.Project, Result: res,
	}
	if kind == JobLogStarted {
		event.Argv = append([]string(nil), j.Spec.Argv...)
	}
	return event
}

// jobLogCloseWait bounds how long Close waits on a stalled JobLog.
const jobLogCloseWait = 2 * time.Second

const (
	maxPendingJobLogEvents = 1024
	maxPendingJobLogBytes  = 4 << 20
)

// jobLogQueue delivers events to one consumer, in order, from its own
// goroutine without making jobs wait for output. Pending events are bounded
// by count and retained payload size; overflow is reported when output resumes.
// The consumer holds at most one bounded batch in addition to the pending batch.
type jobLogQueue struct {
	mu      sync.Mutex
	pending []JobLogEvent
	bytes   int64
	dropped uint64
	wake    chan struct{}
	closing chan struct{}
	done    chan struct{}
}

func newJobLogQueue(deliver func(JobLogEvent)) *jobLogQueue {
	q := &jobLogQueue{wake: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{})}
	go q.run(deliver)
	return q
}

func (q *jobLogQueue) push(event JobLogEvent) {
	size := jobLogEventBytes(event)
	q.mu.Lock()
	if len(q.pending) >= maxPendingJobLogEvents || size > maxPendingJobLogBytes-q.bytes {
		q.dropped++
	} else {
		q.pending = append(q.pending, event)
		q.bytes += size
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Include string/slice payloads and fixed allowances for event/result metadata.
// Counting bytes as well as events prevents large argv from filling a large queue.
func jobLogEventBytes(e JobLogEvent) int64 {
	size := int64(256 + len(e.ID) + len(e.Owner) + len(e.Project) + 16*len(e.Argv))
	for _, arg := range e.Argv {
		size += int64(len(arg))
	}
	if r := e.Result; r != nil {
		size += int64(512 + len(r.State) + len(r.Signal) + len(r.StartError) + len(r.TransactionError) + len(r.LimitExceeded))
		if c := r.Changes; c != nil {
			size += int64(128 + len(c.BundleRoot) + 16*len(c.Paths))
			for _, path := range c.Paths {
				size += int64(len(path))
			}
		}
	}
	return size
}

func (q *jobLogQueue) take() ([]JobLogEvent, uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	batch := q.pending
	dropped := q.dropped
	q.pending = nil
	q.bytes, q.dropped = 0, 0
	return batch, dropped
}

func (q *jobLogQueue) run(deliver func(JobLogEvent)) {
	defer close(q.done)
	deliverBatch := func(batch []JobLogEvent, dropped uint64) {
		for i, event := range batch {
			deliver(event)
			batch[i] = JobLogEvent{} // release payloads as each event finishes
		}
		if dropped != 0 {
			deliver(JobLogEvent{Kind: JobLogDropped, Time: time.Now(), Dropped: dropped})
		}
	}
	for {
		batch, dropped := q.take()
		deliverBatch(batch, dropped)
		if len(batch) > 0 || dropped != 0 {
			continue
		}
		select {
		case <-q.wake:
		case <-q.closing:
			batch, dropped := q.take()
			deliverBatch(batch, dropped)
			return
		}
	}
}

// close delivers retained events and overflow counts, waiting at most wait for a
// consumer that has stalled so shutdown can't hang on it.
func (q *jobLogQueue) close(wait time.Duration) {
	close(q.closing)
	select {
	case <-q.done:
	case <-time.After(wait):
	}
}

// queueAhead counts queue entries ahead of j while j is queued.
func (d *Daemon) queueAhead(j *Job) *int {
	d.mu.Lock()
	defer d.mu.Unlock()
	j.mu.Lock()
	queued := j.state == proto.StateQueued
	j.mu.Unlock()
	if !queued {
		return nil
	}
	for i, queuedJob := range d.queue {
		if queuedJob == j {
			ahead := i
			return &ahead
		}
	}
	return nil
}

// statusWithQueue is a job's status plus its queue position, for callers
// that are waiting on it.
func (d *Daemon) statusWithQueue(j *Job) proto.JobStatus {
	status := j.Status()
	status.QueueAhead = d.queueAhead(j)
	return status
}
