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
}

// logJob queues a lifecycle event for Config.JobLog without waiting on it.
// A consumer that stalls (a backpressured stderr pipe) must never delay a
// job's start, its runtime limit, or its result.
func (d *Daemon) logJob(kind JobLogKind, j *Job, res *proto.Result) {
	if d.jobLog == nil {
		return
	}
	j.mu.Lock()
	event := JobLogEvent{
		Kind: kind, Time: time.Now(), ID: j.ID, Owner: admissionOwner(j.Admission),
		Project: j.Admission.Project, Argv: append([]string(nil), j.Spec.Argv...), Result: res,
	}
	j.mu.Unlock()
	if kind == JobLogQueued {
		if ahead := d.queueAhead(j); ahead != nil {
			event.QueueAhead = *ahead
		}
	}
	d.jobLog.push(event)
}

// jobLogCloseWait bounds how long Close waits on a stalled JobLog.
const jobLogCloseWait = 2 * time.Second

// jobLogQueue delivers events to one consumer, in order, from its own
// goroutine. It never drops an event and never makes the caller wait; while
// the consumer is stalled, events wait in memory. Each job adds at most
// three, so job throughput bounds the growth.
type jobLogQueue struct {
	mu      sync.Mutex
	pending []JobLogEvent
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
	q.mu.Lock()
	q.pending = append(q.pending, event)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *jobLogQueue) take() []JobLogEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	batch := q.pending
	q.pending = nil
	return batch
}

func (q *jobLogQueue) run(deliver func(JobLogEvent)) {
	defer close(q.done)
	for {
		batch := q.take()
		for _, event := range batch {
			deliver(event)
		}
		if len(batch) > 0 {
			continue
		}
		select {
		case <-q.wake:
		case <-q.closing:
			for _, event := range q.take() {
				deliver(event)
			}
			return
		}
	}
}

// close delivers every event pushed before it, waiting at most wait for a
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
