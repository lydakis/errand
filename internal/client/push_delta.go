package client

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
)

func pushBase(peer, workspace, client string) (*proto.Manifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	var base proto.Manifest
	err := getJSONContext(ctx, strings.TrimSuffix(peer, "/")+"/v0/workspaces/"+workspace+"/push/base?client="+client, maxWorkspaceResponseBytes, "push checkpoint", &base)
	var response *controlHTTPError
	if errors.As(err, &response) && response.statusCode == http.StatusNotFound {
		return nil, nil
	} // old daemon
	if err != nil {
		return nil, err
	}
	if err := archive.Validate(base); err != nil {
		return nil, err
	}
	return &base, nil
}

func pushSourceManifest(request proto.PushRequest) proto.Manifest {
	if request.Delta != nil {
		return request.Delta.RemoteManifest
	}
	return request.Manifest
}

func smallPushDelta(manifest proto.Manifest) bool {
	var bytes int64
	for _, e := range manifest.Entries {
		if e.Type == proto.EntryFile {
			if e.Size < 0 || e.Size > 64<<10-bytes {
				return false
			}
			bytes += e.Size
		}
	}
	return true
}
