package worker

import (
	"io/fs"
	"path"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

type assetsServer struct {
	hostv1.UnimplementedAssetsServer
	ui fs.FS
}

func newAssetsServer(ui fs.FS) (*assetsServer, error) { return &assetsServer{ui: ui}, nil }

func (a *assetsServer) Get(req *hostv1.AssetRequest, stream grpc.ServerStreamingServer[hostv1.AssetChunk]) error {
	data, err := fs.ReadFile(a.ui, req.GetPath())
	if err != nil {
		return status.Error(codes.NotFound, "no such asset")
	}
	return stream.Send(&hostv1.AssetChunk{Data: data, ContentType: contentTypeByExt(req.GetPath()), Last: true})
}

func contentTypeByExt(p string) string {
	switch path.Ext(p) {
	case ".js":
		return "text/javascript"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}
