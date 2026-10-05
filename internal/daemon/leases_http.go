package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/lydakis/errand/internal/cloud"
	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) requireBroker(w http.ResponseWriter) bool {
	if d.broker == nil {
		httpError(w, http.StatusNotFound, "this runner has no cloud offers")
		return false
	}
	return true
}

// leaseOwner keys leases like jobs. Insecure test runners have no identity,
// and the broker refuses an empty owner, so they share one fixed owner.
func leaseOwner(id Identity) string {
	if id.Method == "insecure-test" {
		return "insecure-test"
	}
	return id.Owner()
}

func leaseError(w http.ResponseWriter, err error) {
	var e *cloud.Error
	if errors.As(err, &e) {
		httpError(w, e.Status, e.Msg)
		return
	}
	httpError(w, http.StatusInternalServerError, err.Error())
}

func (d *Daemon) handleLeaseAcquire(w http.ResponseWriter, r *http.Request, id Identity) {
	// The leased runner admits the caller for every action, so only callers
	// who may already run jobs here may lease one.
	if !id.Allowed(proto.ActionSubmit) {
		httpError(w, http.StatusForbidden, "leasing a machine needs the submit action as well as lease")
		return
	}
	if !d.requireBroker(w) {
		return
	}
	var req proto.LeaseRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid lease request: "+err.Error())
		return
	}
	// Only a tailnet login means anything to a leased runner; local callers'
	// account names do not.
	login := id.Login
	if id.Local {
		login = ""
	}
	lease, err := d.broker.Acquire(leaseOwner(id), login, req.Where)
	if err != nil {
		leaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (d *Daemon) handleLeaseList(w http.ResponseWriter, _ *http.Request, id Identity) {
	if !d.requireBroker(w) {
		return
	}
	leases := d.broker.List(leaseOwner(id))
	if leases == nil {
		leases = []proto.Lease{}
	}
	writeJSON(w, http.StatusOK, leases)
}

func (d *Daemon) handleLeaseGet(w http.ResponseWriter, r *http.Request, id Identity) {
	if !d.requireBroker(w) {
		return
	}
	lease, ok := d.broker.Get(leaseOwner(id), r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "no such lease")
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (d *Daemon) handleLeaseRelease(w http.ResponseWriter, r *http.Request, id Identity) {
	if !d.requireBroker(w) {
		return
	}
	lease, err := d.broker.Release(leaseOwner(id), r.PathValue("id"))
	if err != nil {
		leaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}
