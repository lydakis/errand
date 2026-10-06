package cloud

import "time"

// withdrawnRequests are requests withdrawn before any lease was handed to
// them, by owner and then request ID. A request that reaches Acquire after
// its own withdrawal, such as a lost attempt that was slow to arrive, is
// refused rather than starting a lease nobody waits for. A restart ends
// every request in flight, so memory is enough.
//
// Owners include whoever the tailnet policy grants the lease action, which
// this runner does not bound, so an entry is never dropped before it expires: a
// withdrawal that finds no room, for its owner or in all, is refused
// instead, and the caller is not told the request is settled.
type withdrawnRequests map[string]map[string]time.Time

const (
	maxWithdrawnPerOwner = 128
	maxWithdrawn         = 4096
	withdrawnRequestAge  = time.Hour
)

// add records a withdrawal after dropping expired entries, and reports
// whether there was room for it.
func (w *withdrawnRequests) add(owner, requestID string, now time.Time) bool {
	if *w == nil {
		*w = withdrawnRequests{}
	}
	total := 0
	for o, requests := range *w {
		for id, at := range requests {
			if now.Sub(at) > withdrawnRequestAge {
				delete(requests, id)
			}
		}
		if len(requests) == 0 {
			delete(*w, o)
		}
		total += len(requests)
	}
	requests := (*w)[owner]
	if _, ok := requests[requestID]; !ok && (len(requests) >= maxWithdrawnPerOwner || total >= maxWithdrawn) {
		return false
	}
	if requests == nil {
		requests = map[string]time.Time{}
		(*w)[owner] = requests
	}
	requests[requestID] = now
	return true
}

func (w withdrawnRequests) has(owner, requestID string, now time.Time) bool {
	at, ok := w[owner][requestID]
	return ok && now.Sub(at) <= withdrawnRequestAge
}
