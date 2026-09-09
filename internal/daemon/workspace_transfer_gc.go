package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

func (d *Daemon) handleTransferGC(w http.ResponseWriter, r *http.Request, id Identity) {
	var request proto.TransferGCRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.OlderThanSeconds < 1 || request.OlderThanSeconds > int64((1<<63-1)/int64(time.Second)) {
		httpError(w, 400, "positive bounded older_than_seconds is required")
		return
	}
	cutoff := time.Now().Add(-time.Duration(request.OlderThanSeconds) * time.Second)
	d.workspaces.mu.Lock()
	rows, err := d.workspaces.records()
	d.workspaces.mu.Unlock()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	var total changeops.TransferGCResult
	for _, row := range rows {
		if row.Owner != d.workspaceOwner(id) {
			continue
		}
		unlock := d.workspaces.lockWorkspace(row.ID)
		err := func() error {
			d.workspaces.mu.Lock()
			current, err := d.workspaces.lookup(row.Owner, row.ID)
			d.workspaces.mu.Unlock()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(filepath.Join(d.workspaces.dir, row.ID, "push"))
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			for _, e := range entries {
				if !proto.ValidChangeClientID(e.Name()) {
					return fmt.Errorf("invalid workspace transfer directory")
				}
				result, err := d.pushSession(current, e.Name()).GC(r.Context(), cutoff, request.DryRun, []proto.Manifest{current.Manifest})
				if err != nil {
					return err
				}
				total.Removed += result.Removed
				total.Protected += result.Protected
				total.FreedBytes += result.FreedBytes
			}
			return nil
		}()
		unlock()
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, total)
}
