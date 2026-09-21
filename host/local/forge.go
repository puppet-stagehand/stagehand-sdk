package local

// This file wires the Forge facet's two RPCs (Search, Resolve) to the
// injectable ForgeClient (forge_client.go). This plan's Task 1 lands the
// real HTTP transport and adapts forgeServer to the resulting interface,
// targeting the public registry only; Task 2 layers sealed multi-source
// selection (D-05..D-08) on top without changing this shape further. 07-03
// later replaces Resolve's body with the full recursive, cycle/conflict-
// aware advisory resolver (forge_resolver.go) — this plan wires Resolve
// through the real client with a single-node result only, since
// FORGE-04/05/06 belong to that next plan, not this one.

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// publicForgeSource is the stable name every Search/Resolve result is
// tagged with while only the public registry is wired (D-08). Task 2 adds
// private source selection; until then every request targets the public
// registry regardless of req.Source.
const publicForgeSource = "puppet-forge"

// forgeServer is the real in-memory Forge facet implementation.
type forgeServer struct {
	hostv1.UnimplementedForgeServer
	packID string
	client ForgeClient
}

// newForgeServer wires a forgeServer. A nil client defaults to the real
// HTTP adapter (D-01); tests and pack examples inject a fixture instead.
func newForgeServer(packID string, client ForgeClient) *forgeServer {
	if client == nil {
		client = DefaultForgeClient()
	}
	return &forgeServer{packID: packID, client: client}
}

func (s *forgeServer) Search(ctx context.Context, req *hostv1.SearchRequest) (*hostv1.SearchResponse, error) {
	if req == nil || req.Query == "" {
		return nil, status.Error(codes.InvalidArgument, "search query is required")
	}
	req = proto.Clone(req).(*hostv1.SearchRequest)
	if s.client == nil {
		return nil, status.Error(codes.Unavailable, "forge client is not configured")
	}

	ep := ForgeEndpoint{BaseURL: DefaultForgeBaseURL}
	results, pageInfo, err := s.client.Search(ctx, ep, publicForgeSource, req.Query, req.Page)
	if err != nil {
		return nil, err
	}
	if pageInfo == nil {
		pageInfo = &hostv1.PageInfo{}
	}
	return &hostv1.SearchResponse{Results: results, Page: pageInfo}, nil
}

func (s *forgeServer) Resolve(ctx context.Context, req *hostv1.ResolveRequest) (*hostv1.ResolveResponse, error) {
	if req == nil || req.Name == "" || req.Version == "" {
		return nil, status.Error(codes.InvalidArgument, "module name and version are required")
	}
	req = proto.Clone(req).(*hostv1.ResolveRequest)
	if s.client == nil {
		return nil, status.Error(codes.Unavailable, "forge client is not configured")
	}

	const advisoryMessage = "authoring advisory; does not emulate r10k deployment"
	ep := ForgeEndpoint{BaseURL: DefaultForgeBaseURL}
	rel, err := s.client.GetRelease(ctx, ep, req.Name, req.Version)
	if err != nil {
		if errors.Is(err, ErrForgeNotFound) {
			// A typed not-found is advisory data (D-12), never a whole-RPC
			// failure: the caller gets back a reviewable unresolved node,
			// not an error, so it can decide what to do next.
			return &hostv1.ResolveResponse{
				AdvisoryOnly:    true,
				AdvisoryMessage: advisoryMessage,
				Root: &hostv1.DependencyNode{
					Name: req.Name, Version: req.Version, Source: publicForgeSource, Unresolved: true,
				},
				Warnings: []*hostv1.ForgeAdvisoryWarning{{
					Code: "unresolved", Message: "module or release not found", Module: req.Name,
				}},
			}, nil
		}
		return nil, err
	}
	return &hostv1.ResolveResponse{
		AdvisoryOnly:    true,
		AdvisoryMessage: advisoryMessage,
		Root:            &hostv1.DependencyNode{Name: rel.Name, Version: rel.Version, Source: publicForgeSource},
	}, nil
}

// gatedForge wraps forgeServer with the forge:rw permission check every
// RPC requires.
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
