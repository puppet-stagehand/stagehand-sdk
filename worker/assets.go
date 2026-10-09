package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

const (
	uiManifestName = "ui.manifest.json"
	// maxUIManifestBytes is the contract's limit for ui.manifest.json.
	maxUIManifestBytes = 256 << 10
	// maxAssetPathLen is the contract's path length limit.
	maxAssetPathLen = 200
)

// assetsServer serves the files ui.manifest.json lists, plus the manifest. It
// is the worker half of the UI bundle contract (docs/ui-bundle-contract.md);
// the host separately verifies every byte against ui_digest.
type assetsServer struct {
	hostv1.UnimplementedAssetsServer
	ui     fs.FS
	listed map[string]string // listed path -> declared content type
}

// newAssetsServer reads ui.manifest.json from ui. A bundle without one is a
// pack without a UI (every Get is NotFound); one that exists but cannot be
// read is an error, so a broken image fails at start-up.
func newAssetsServer(ui fs.FS) (*assetsServer, error) {
	a := &assetsServer{ui: ui, listed: map[string]string{}}
	raw, err := fs.ReadFile(ui, uiManifestName)
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: read %s: %w", uiManifestName, err)
	}
	if len(raw) > maxUIManifestBytes {
		return nil, fmt.Errorf("worker: %s is larger than %d bytes", uiManifestName, maxUIManifestBytes)
	}
	var m struct {
		Files map[string]struct {
			ContentType string `json:"content_type"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("worker: %s is not valid JSON: %w", uiManifestName, err)
	}
	for p, f := range m.Files {
		a.listed[p] = f.ContentType
	}
	return a, nil
}

// validAssetPath applies the contract's path rule, and is stricter in one
// place: any ".." anywhere is refused, not only a ".." segment.
func validAssetPath(p string) bool {
	if p == "" || len(p) > maxAssetPathLen || p[0] == '/' {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '/' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	for i := 0; i+1 < len(p); i++ {
		if p[i] == '.' && p[i+1] == '.' {
			return false
		}
	}
	return path.Clean(p) == p && p != "."
}

// Get streams one listed file in chunks of at most MaxAssetChunk.
func (a *assetsServer) Get(req *hostv1.AssetRequest, stream grpc.ServerStreamingServer[hostv1.AssetChunk]) error {
	p := req.GetPath()
	if !validAssetPath(p) {
		return status.Error(codes.InvalidArgument, "invalid asset path")
	}
	ct := "application/json"
	if p != uiManifestName {
		declared, listed := a.listed[p]
		if !listed {
			return status.Error(codes.NotFound, "no such asset")
		}
		ct = declared
		if ct == "" {
			ct = contentTypeByExt(p)
		}
	}
	data, err := fs.ReadFile(a.ui, p)
	if errors.Is(err, fs.ErrNotExist) {
		return status.Error(codes.NotFound, "no such asset")
	}
	if err != nil {
		return status.Error(codes.Internal, "could not read asset")
	}
	if len(data) == 0 {
		return stream.Send(&hostv1.AssetChunk{ContentType: ct, Last: true})
	}
	for off := 0; off < len(data); off += MaxAssetChunk {
		end := min(off+MaxAssetChunk, len(data))
		if err := stream.Send(&hostv1.AssetChunk{Data: data[off:end], ContentType: ct, Last: end == len(data)}); err != nil {
			return err
		}
	}
	return nil
}

// contentTypeByExt is the fallback for a listed file whose manifest entry
// declares no type. The types are the ones the bundle contract allows (no
// charset parameter: the console compares them exactly).
func contentTypeByExt(p string) string {
	switch path.Ext(p) {
	case ".js":
		return "text/javascript"
	case ".json", ".map":
		return "application/json"
	case ".css":
		return "text/css"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".woff2":
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}
