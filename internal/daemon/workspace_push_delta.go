package daemon

import (
	"net/http"
	"os"

	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) pushBase(row workspaceRecord, client string) (proto.Manifest, error) {
	v, err := d.pushSession(row, client).Checkpoint().Read()
	if os.IsNotExist(err) {
		return row.Manifest, nil
	}
	return v.Manifest, err
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
	writeJSON(w, 200, base)
}
