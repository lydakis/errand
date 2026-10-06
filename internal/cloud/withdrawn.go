package cloud

import "time"

// withdrawnRequests are requests withdrawn before any lease was handed to
// them, keyed by owner and request ID. A request that reaches Acquire after
// its own withdrawal, such as a lost attempt that was slow to arrive, is
// refused rather than starting a lease nobody waits for. A restart ends
// every request in flight, so memory is enough.
type withdrawnRequests map[string]time.Time

const (
	maxWithdrawnRequests = 1024
	withdrawnRequestAge  = time.Hour
)

func withdrawnKey(owner, requestID string) string { return owner + "\x00" + requestID }

// add records a withdrawal, dropping expired entries and then the oldest
// ones beyond the cap.
func (w withdrawnRequests) add(owner, requestID string, now time.Time) withdrawnRequests {
	if w == nil {
		w = withdrawnRequests{}
	}
	for k, at := range w {
		if now.Sub(at) > withdrawnRequestAge {
			delete(w, k)
		}
	}
	for len(w) >= maxWithdrawnRequests {
		oldest := ""
		for k, at := range w {
			if oldest == "" || at.Before(w[oldest]) {
				oldest = k
			}
		}
		delete(w, oldest)
	}
	w[withdrawnKey(owner, requestID)] = now
	return w
}

func (w withdrawnRequests) has(owner, requestID string, now time.Time) bool {
	at, ok := w[withdrawnKey(owner, requestID)]
	return ok && now.Sub(at) <= withdrawnRequestAge
}
