package cloud

import "time"

// withdrawnRequests are requests withdrawn before any lease was handed to
// them, by owner and then request ID. A request that reaches Acquire after
// its own withdrawal, such as a lost attempt that was slow to arrive, is
// refused rather than starting a lease nobody waits for. A restart ends
// every request in flight, so memory is enough.
//
// Each owner's withdrawals are bounded on their own, so one owner's flood
// only ever evicts that owner's entries. The number of owners is bounded
// too; owners are callers authorized to lease here.
type withdrawnRequests map[string]map[string]time.Time

const (
	maxWithdrawnPerOwner = 128
	maxWithdrawnOwners   = 1024
	withdrawnRequestAge  = time.Hour
)

// add records a withdrawal. It drops the owner's expired entries, then its
// oldest ones beyond the cap. A new owner beyond the owner cap replaces
// the owner whose last withdrawal is oldest.
func (w withdrawnRequests) add(owner, requestID string, now time.Time) withdrawnRequests {
	if w == nil {
		w = withdrawnRequests{}
	}
	requests := w[owner]
	if requests == nil {
		if len(w) >= maxWithdrawnOwners {
			stalest, stalestAt := "", time.Time{}
			for o, rs := range w {
				if at := latest(rs); stalest == "" || at.Before(stalestAt) {
					stalest, stalestAt = o, at
				}
			}
			delete(w, stalest)
		}
		requests = map[string]time.Time{}
		w[owner] = requests
	}
	for id, at := range requests {
		if now.Sub(at) > withdrawnRequestAge {
			delete(requests, id)
		}
	}
	for len(requests) >= maxWithdrawnPerOwner {
		oldest := ""
		for id, at := range requests {
			if oldest == "" || at.Before(requests[oldest]) {
				oldest = id
			}
		}
		delete(requests, oldest)
	}
	requests[requestID] = now
	return w
}

func (w withdrawnRequests) has(owner, requestID string, now time.Time) bool {
	at, ok := w[owner][requestID]
	return ok && now.Sub(at) <= withdrawnRequestAge
}

func latest(requests map[string]time.Time) time.Time {
	var last time.Time
	for _, at := range requests {
		if at.After(last) {
			last = at
		}
	}
	return last
}
