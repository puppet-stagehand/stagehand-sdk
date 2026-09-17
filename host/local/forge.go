package local

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// ForgeClient is the transport-independent registry seam. The HTTP adapter
// is supplied by the Forge client plan; fixture clients implement this
// interface directly for deterministic host.Local tests.
type ForgeClient interface {
	Search(context.Context, *hostv1.SearchRequest) (*hostv1.SearchResponse, error)
	Resolve(context.Context, *hostv1.ResolveRequest) (*hostv1.ResolveResponse, error)
}

type forgeServer struct {
	hostv1.UnimplementedForgeServer
	client ForgeClient
}

func newForgeServer(client ForgeClient) *forgeServer {
	return &forgeServer{client: client}
}

func (s *forgeServer) Search(ctx context.Context, req *hostv1.SearchRequest) (*hostv1.SearchResponse, error) {
	if req == nil || req.Query == "" {
		return nil, status.Error(codes.InvalidArgument, "search query is required")
	}
	if s.client == nil {
		return nil, status.Error(codes.Unavailable, "forge client is not configured")
	}
	resp, err := s.client.Search(ctx, proto.Clone(req).(*hostv1.SearchRequest))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, status.Error(codes.Internal, "forge client returned an empty search response")
	}
	return proto.Clone(resp).(*hostv1.SearchResponse), nil
}

func (s *forgeServer) Resolve(ctx context.Context, req *hostv1.ResolveRequest) (*hostv1.ResolveResponse, error) {
	if req == nil || req.Name == "" || req.Version == "" {
		return nil, status.Error(codes.InvalidArgument, "module name and version are required")
	}
	if s.client == nil {
		return nil, status.Error(codes.Unavailable, "forge client is not configured")
	}
	resp, err := s.client.Resolve(ctx, proto.Clone(req).(*hostv1.ResolveRequest))
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, status.Error(codes.Internal, "forge client returned an empty resolve response")
	}
	return proto.Clone(resp).(*hostv1.ResolveResponse), nil
}

type gatedForge struct {
	hostv1.UnimplementedForgeServer
	perms  map[string]bool
	packID string
	inner  *forgeServer
}

func (s *gatedForge) Search(ctx context.Context, req *hostv1.SearchRequest) (*hostv1.SearchResponse, error) {
	if !s.perms["forge:rw"] {
		return nil, ErrPermissionDenied("forge:rw")
	}
	return s.inner.Search(ctx, req)
}

func (s *gatedForge) Resolve(ctx context.Context, req *hostv1.ResolveRequest) (*hostv1.ResolveResponse, error) {
	if !s.perms["forge:rw"] {
		return nil, ErrPermissionDenied("forge:rw")
	}
	return s.inner.Resolve(ctx, req)
}
