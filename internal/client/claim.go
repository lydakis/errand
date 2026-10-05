package client

import "sync"

// admission is what became of a run's request on one runner.
type admission int

const (
	notAdmitted   admission = iota // nothing reached the runner, or it definitely refused
	admitted                       // the runner accepted the request
	maybeAdmitted                  // the request was sent, but its answer was lost
)

// A Claim is a run's hold on a machine resolved for it alone, such as a
// lease. The run settles it exactly once, with what became of its request
// there. A request that was not admitted gives the machine up at once. One
// that was admitted, or may have been, keeps the hold: only the runner
// knows whether a job is there, so from then on the machine's own idle rule
// decides when it ends.
type Claim struct {
	giveUp func()
	once   sync.Once
}

// NewClaim returns a claim that calls giveUp if its run admits nothing.
func NewClaim(giveUp func()) *Claim { return &Claim{giveUp: giveUp} }

func (c *Claim) settle(a admission) {
	if c == nil {
		return
	}
	c.once.Do(func() {
		if a == notAdmitted {
			c.giveUp()
		}
	})
}
