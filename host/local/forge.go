package local

// This file wires the Forge facet's two RPCs (Search, Resolve) to the
// injectable ForgeClient (forge_client.go) and to this plan's sealed
// multi-source configuration (D-05..D-08). Forge declares no RPC of its
// own for configuring a private source: a pack configures one by calling
// the existing Secrets and Documents facets directly (Secrets.Store to
// seal {base_url, auth}, then Documents.Put into forgeSourceCollection
// with the resulting ref) — matching this project's CLAUDE.md rule that
// state lives in Documents and secrets go through Secrets, and avoiding a
// second write path for the same data. Forge's own job is reading that
// convention back at Search/Resolve time.
//
// 07-03 replaces Resolve's body with the full recursive, cycle/conflict-
// aware advisory resolver (forge_resolver.go); this plan wires Resolve
// through the real client and source selection with a single-node result
// only — FORGE-04/05/06 are this phase's next plan's requirements, not
// this one's.

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// forgeSourceCollection is the Documents collection holding the name-only
// forge-source index (D-07): one document per configured private source,
// keyed by source name, body {name, label, secret_ref}. It never carries a
// base URL or auth value — those live only in the sealed Secret the
// secret_ref points at.
const forgeSourceCollection = "forge-sources"

// publicForgeSource is the stable name every Search/Resolve result tags
// public-registry data with (D-08), and the name a ForgeSourceSelection
// may use to explicitly request the public registry. An unset selection
// also means the public registry. Selecting it never touches
// Documents/Secrets.
const publicForgeSource = "puppet-forge"

// forgeSourceSecret is the sealed JSON shape a forge-sources index entry's
// secret_ref resolves to via Secrets.Reveal: the private base URL and the
// exact Authorization header value to send for that source (D-06). This
// shape is an internal host.Local convention a pack must follow when
// configuring a source — it is not part of the public wire contract.
type forgeSourceSecret struct {
	BaseURL string `json:"base_url"`
	Auth    string `json:"auth"`
}

// forgeServer is the real in-memory Forge facet implementation. Like
// codeServer, it carries no mutex of its own for Documents state: docs is
// the SAME documentsServer instance host.Host.Documents holds, so
// docs.mu is the one lock that orders every forge-sources read. secrets is
// likewise the same secretsServer instance backing host.Host.Secrets;
// forgeServer reads it directly (not through the secrets:rw gate) because
// revealing a source's own previously-configured secret is part of the
// forge:rw-gated Search/Resolve operation itself, not a separate elevation
// — the pack already needed secrets:rw to configure that source in the
// first place.
type forgeServer struct {
	hostv1.UnimplementedForgeServer
	packID  string
	client  ForgeClient
	docs    *documentsServer
	secrets *secretsServer
}

// newForgeServer wires a forgeServer. A nil client defaults to the real
// HTTP adapter (D-01); tests and pack examples inject a fixture instead.
func newForgeServer(packID string, docs *documentsServer, secrets *secretsServer, client ForgeClient) *forgeServer {
	if client == nil {
		client = DefaultForgeClient()
	}
	return &forgeServer{packID: packID, client: client, docs: docs, secrets: secrets}
}

// resolveSource turns a ForgeSourceSelection into the endpoint a request
// should target and the source name every result/node gets tagged with
// (D-08). An unset selection, or the literal public source name, always
// resolves to the public registry without touching Documents/Secrets —
// only a private source name triggers the index lookup + reveal (D-07).
// Searching one private source reveals only that source's own secret: a
// caller selecting "internal-a" never causes "internal-b"'s config to be
// read, sealed or otherwise.
func (s *forgeServer) resolveSource(ctx context.Context, sel *hostv1.ForgeSourceSelection) (ForgeEndpoint, string, error) {
	name := publicForgeSource
	if sel != nil && sel.Name != "" {
		name = sel.Name
	}
	if name == publicForgeSource {
		return ForgeEndpoint{BaseURL: DefaultForgeBaseURL}, publicForgeSource, nil
	}

	s.docs.mu.Lock()
	doc, ok := s.docs.getLocked(forgeSourceCollection, name)
	var secretRef string
	if ok && doc.Body != nil && doc.Body.Value != nil {
		secretRef, _ = doc.Body.Value.AsMap()["secret_ref"].(string)
	}
	s.docs.mu.Unlock()
	if !ok {
		return ForgeEndpoint{}, "", status.Errorf(codes.NotFound, "forge source %q is not configured", name)
	}
	if secretRef == "" {
		return ForgeEndpoint{}, "", status.Errorf(codes.Internal, "forge source %q index entry has no secret_ref", name)
	}

	revealed, err := s.secrets.Reveal(ctx, &hostv1.SecretRef{Ref: secretRef})
	if err != nil {
		return ForgeEndpoint{}, "", status.Errorf(codes.Internal, "forge source %q secret could not be revealed", name)
	}
	var sealed forgeSourceSecret
	if err := json.Unmarshal(revealed.Plaintext, &sealed); err != nil {
		return ForgeEndpoint{}, "", status.Errorf(codes.Internal, "forge source %q secret is malformed", name)
	}
	if _, verr := validateForgeBaseURL(sealed.BaseURL); verr != nil {
		return ForgeEndpoint{}, "", status.Errorf(codes.FailedPrecondition, "forge source %q has an invalid base URL: %v", name, verr)
	}
	return ForgeEndpoint{BaseURL: sealed.BaseURL, Auth: sealed.Auth}, name, nil
}

func (s *forgeServer) Search(ctx context.Context, req *hostv1.SearchRequest) (*hostv1.SearchResponse, error) {
	if req == nil || req.Query == "" {
		return nil, status.Error(codes.InvalidArgument, "search query is required")
	}
	req = proto.Clone(req).(*hostv1.SearchRequest)
	if s.client == nil {
		return nil, status.Error(codes.Unavailable, "forge client is not configured")
	}

	ep, source, err := s.resolveSource(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	results, pageInfo, err := s.client.Search(ctx, ep, source, req.Query, req.Page)
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

	ep, source, err := s.resolveSource(ctx, req.Source)
	if err != nil {
		return nil, err
	}

	const advisoryMessage = "authoring advisory; does not emulate r10k deployment"
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
					Name: req.Name, Version: req.Version, Source: source, Unresolved: true,
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
		Root:            &hostv1.DependencyNode{Name: rel.Name, Version: rel.Version, Source: source},
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
