package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
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
	// Leasing spends money, so a web page the caller happens to visit must
	// not be able to ask for it: browsers send Origin with cross-site
	// requests, and cannot send a JSON content type without a CORS preflight
	// this runner never approves.
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" || r.Header.Get("Origin") != "" {
		httpError(w, http.StatusUnsupportedMediaType, "lease requests must be application/json and not come from a browser")
		return
	}
	var req proto.LeaseRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid lease request: "+err.Error())
		return
	}
	if !proto.ValidULID(req.RequestID) {
		httpError(w, http.StatusBadRequest, "lease request needs a request_id ULID")
		return
	}
	// Only a tailnet login means anything to a leased runner; local callers'
	// account names do not.
	login := id.Login
	if id.Local {
		login = ""
	}
	lease, err := d.broker.Acquire(leaseOwner(id), login, req.Where, req.SSHKey, req.RequestID)
	if err != nil {
		leaseError(w, err)
		return
	}
	writeLease(w, id, lease)
}

// handleLeaseAdmit lets another of the caller's devices into a leased
// machine by its SSH key. Like a lease request it hands out access to the
// machine, so it has the same requirements.
func (d *Daemon) handleLeaseAdmit(w http.ResponseWriter, r *http.Request, id Identity) {
	if !id.Allowed(proto.ActionSubmit) {
		httpError(w, http.StatusForbidden, "using a leased machine needs the submit action as well as lease")
		return
	}
	if !d.requireBroker(w) {
		return
	}
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" || r.Header.Get("Origin") != "" {
		httpError(w, http.StatusUnsupportedMediaType, "lease requests must be application/json and not come from a browser")
		return
	}
	var req proto.LeaseRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid key request: "+err.Error())
		return
	}
	login := id.Login
	if id.Local {
		login = ""
	}
	lease, err := d.broker.Admit(leaseOwner(id), login, r.PathValue("id"), req.SSHKey)
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
	leases := leaseView(id, d.broker.List(leaseOwner(id)))
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
	writeLease(w, id, lease)
}

// A leased machine admits its owner for every action, so holding a lease
// needs the submit action here. An owner who has lost it loses its leases
// at its next request to this runner, and is never told where they are.
func (d *Daemon) checkLeaseAccess(id Identity) {
	if !id.Allowed(proto.ActionSubmit) {
		_ = d.broker.EndAll(leaseOwner(id), "its owner may no longer submit jobs on this runner")
	}
}

// writeLease answers with one lease, as leaseView shows it to this caller.
func writeLease(w http.ResponseWriter, id Identity, lease proto.Lease) {
	writeJSON(w, http.StatusOK, leaseView(id, []proto.Lease{lease})[0])
}

func leaseView(id Identity, leases []proto.Lease) []proto.Lease {
	if id.Allowed(proto.ActionSubmit) {
		return leases
	}
	for i := range leases {
		leases[i].Target = nil
	}
	return leases
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
	writeLease(w, id, lease)
}

func (d *Daemon) handleLeaseWithdraw(w http.ResponseWriter, r *http.Request, id Identity) {
	if !d.requireBroker(w) {
		return
	}
	lease, err := d.broker.Withdraw(leaseOwner(id), r.PathValue("id"))
	if err != nil {
		leaseError(w, err)
		return
	}
	writeLease(w, id, lease)
}
