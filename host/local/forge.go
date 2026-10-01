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
// Resolve is wired onto forge_resolver.go's pure recursive walk and stamps
// each node's already_in_puppetfile status by reading (never writing)
// env's stored Puppetfile through the same code-puppetfiles Documents
// collection the Code facet owns (D-15) — Forge never calls
// PutPuppetfileModule or any other Code mutation (D-14/FORGE-06).

import (
	"context"
	"encoding/json"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/puppet-stagehand/stagehand-sdk/code"
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
	llm     LLMClient
}

// newForgeServer wires a forgeServer. A nil client defaults to the real
// HTTP adapter (D-01); tests and pack examples inject a fixture instead.
// llm is stored as given: a nil LLMClient makes Recommend fail with
// FailedPrecondition until a default client lands.
func newForgeServer(packID string, docs *documentsServer, secrets *secretsServer, client ForgeClient, llm LLMClient) *forgeServer {
	if client == nil {
		client = DefaultForgeClient()
	}
	return &forgeServer{packID: packID, client: client, docs: docs, secrets: secrets, llm: llm}
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

	// resolveDependencyTree (forge_resolver.go) handles a typed not-found
	// anywhere in the tree — root included — as an unresolved advisory
	// node, never a whole-RPC failure (D-12); only a real transport or
	// malformed-payload error returns here as err (D-03).
	root, err := resolveDependencyTree(ctx, s.client, ep, source, req.Name, req.Version)
	if err != nil {
		return nil, err
	}

	if req.Environment != "" {
		present := s.currentPuppetfileModuleNames(req.Environment)
		attachAlreadyInPuppetfile(root, present)
	}

	return &hostv1.ResolveResponse{
		Root:            root,
		Warnings:        collectTreeWarnings(root),
		AdvisoryOnly:    true,
		AdvisoryMessage: "authoring advisory; does not emulate r10k deployment",
	}, nil
}

// currentPuppetfileModuleNames reads env's stored Puppetfile text (if any)
// — through the SAME code-puppetfiles Documents collection and docs.mu
// lock discipline codeServer uses (D-15) — and returns the set of module
// names it currently contains, normalized (D-08/D-15's "already there"
// check must not be fooled by a Puppetfile author having written the
// ns-name hyphen form where this facet's own results use ns/name). A
// missing or unparseable Puppetfile reports an empty set rather than an
// error: Resolve's job is describing current reality, not validating the
// environment on Forge's behalf — Code already does that on its own write
// paths. This function only ever reads; it never calls Put/RemovePuppetfileModule
// or any other Code mutation (D-14/FORGE-06).
func (s *forgeServer) currentPuppetfileModuleNames(env string) map[string]bool {
	s.docs.mu.Lock()
	doc, ok := s.docs.getLocked(puppetfileCollection, env)
	var text string
	if ok && doc.Body != nil && doc.Body.Value != nil {
		text, _ = doc.Body.Value.AsMap()["text"].(string)
	}
	s.docs.mu.Unlock()

	pf, err := code.ParsePuppetfile(text)
	if err != nil {
		return nil
	}
	names := make(map[string]bool, len(pf.Modules))
	for _, m := range pf.Modules {
		names[normalizeModuleName(m.GetName())] = true
	}
	return names
}

// normalizeModuleName converts a module identity to its ns/name form. A
// Puppetfile module name may be written in either the ns/name or ns-name
// slug form (code/puppetfile.go's forgeSlugOK accepts both); this facet's
// own Search/Resolve results always use ns/name (forge_client.go). Only
// the first separator is replaced, mirroring forge_client.go's
// forgeModuleSlug — the module-name segment itself never contains a
// hyphen (Forge's slug grammar restricts it to [a-z][a-z0-9_]*).
func normalizeModuleName(name string) string {
	if strings.Contains(name, "/") {
		return name
	}
	return strings.Replace(name, "-", "/", 1)
}

// attachAlreadyInPuppetfile walks tree, setting AlreadyInPuppetfile on
// every node (root included) from present — a read-only annotation pass
// that never touches Documents/Code (D-15).
func attachAlreadyInPuppetfile(node *hostv1.DependencyNode, present map[string]bool) {
	if node == nil {
		return
	}
	node.AlreadyInPuppetfile = present[normalizeModuleName(node.Name)]
	for _, child := range node.Dependencies {
		attachAlreadyInPuppetfile(child, present)
	}
}

// collectTreeWarnings flattens every node's own Warnings (D-13: each node
// keeps its warnings so a caller can see *why* it is flagged) into the
// response-level Warnings list, a convenience rollup for a caller that
// wants "everything advisory about this tree" without walking it itself.
func collectTreeWarnings(node *hostv1.DependencyNode) []*hostv1.ForgeAdvisoryWarning {
	if node == nil {
		return nil
	}
	warnings := append([]*hostv1.ForgeAdvisoryWarning{}, node.Warnings...)
	for _, child := range node.Dependencies {
		warnings = append(warnings, collectTreeWarnings(child)...)
	}
	return warnings
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

// Recommend requires forge:recommend only. It is orthogonal to forge:rw: the
// gate runs before any provider resolution, secret reveal or LLM call.
func (s *gatedForge) Recommend(ctx context.Context, req *hostv1.RecommendRequest) (*hostv1.RecommendResponse, error) {
	if !s.perms["forge:recommend"] {
		return nil, ErrPermissionDenied("forge:recommend")
	}
	return s.inner.Recommend(ctx, req)
}
