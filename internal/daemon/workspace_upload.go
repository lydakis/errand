package daemon

import (
	"context"
	"fmt"
	"os"

	changeops "github.com/lydakis/errand/internal/changes"
)

// One source tree per workspace, including while staging or cleaning up.
// Waiting uploads hold no temporary storage and never block workspace commands.
type workspaceUpload struct {
	row  workspaceRecord
	dir  string
	done chan struct{}
	err  error // cleanup failure; protected by the store mutex
}

func (s *workspaceStore) beginUpload(ctx context.Context, row workspaceRecord) (*workspaceUpload, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		active := s.uploads[row.ID]
		if active == nil {
			dir, err := os.MkdirTemp(s.dir, ".push-upload-")
			if err != nil {
				s.mu.Unlock()
				return nil, err
			}
			upload := &workspaceUpload{row: row, dir: dir, done: make(chan struct{})}
			if s.uploads == nil {
				s.uploads = make(map[string]*workspaceUpload)
			}
			s.uploads[row.ID] = upload
			s.mu.Unlock()
			return upload, nil
		}
		err := active.err
		s.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("previous workspace upload cleanup failed: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-active.done:
		}
	}
}

func (s *workspaceStore) finishUpload(upload *workspaceUpload) error {
	err := changeops.RemoveTree(upload.dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.uploads, upload.row.ID)
	} else {
		// Preserve accounting and the storage bound until startup retries cleanup.
		upload.err = err
	}
	close(upload.done)
	return err
}
