package client

import (
	"context"
	"strings"

	"github.com/lydakis/errand/internal/archive"
	"github.com/lydakis/errand/internal/proto"
)

func pushBase(peer, workspace, client string) (proto.Manifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), controlRequestTimeout)
	defer cancel()
	var base proto.Manifest
	err := getJSONContext(ctx, strings.TrimSuffix(peer, "/")+"/v0/workspaces/"+workspace+"/push/base?client="+client, maxWorkspaceResponseBytes, "push checkpoint", &base)
	if err != nil {
		return base, err
	}
	if err := archive.Validate(base); err != nil {
		return base, err
	}
	return base, nil
}

func pushSourceManifest(request proto.PushRequest) proto.Manifest {
	return request.Delta.RemoteManifest
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
