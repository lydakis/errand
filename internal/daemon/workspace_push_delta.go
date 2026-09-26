package daemon

import (
	"net/http"
	"os"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

// pushBase is the client's retained checkpoint or, before its first push, the
// creation snapshot. Either carries its identity for later requests to reuse.
func (d *Daemon) pushBase(row workspaceRecord, client string) (*changeops.SourceBase, error) {
	base, err := d.pushSession(row, client).Checkpoint().Base()
	if os.IsNotExist(err) {
		return row.creation, nil
	}
	return base, err
}

// The retained source checkpoint is independent of the running application's
// mutable files. Advertising it also negotiates delta-source upload support.
func (d *Daemon) handleWorkspacePushBase(w http.ResponseWriter, r *http.Request, id Identity) {
	client := r.URL.Query().Get("client")
	if !proto.ValidChangeClientID(client) {
		httpError(w, 400, "invalid push client")
		return
	}
	unlock, err := d.workspaces.lockWorkspaceContext(r.Context(), r.PathValue("id"))
	if err != nil {
		return
	}
	defer unlock()
	row, err := d.pushWorkspace(r, id)
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	if err := d.recoverWorkspacePushes(row); err != nil {
		workspaceHTTPError(w, err)
		return
	}
	base, err := d.pushBase(row, client)
	if err != nil {
		workspaceHTTPError(w, err)
		return
	}
	writeJSON(w, 200, base.Manifest())
}
