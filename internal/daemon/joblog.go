package daemon

import (
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

func (d *Daemon) logJob(kind JobLogKind, j *Job, res *proto.Result) {
	if d.cfg.JobLog == nil {
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
	d.cfg.JobLog(event)
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
