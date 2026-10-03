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
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	// callTimeout bounds each LLM provider call; llmCallTimeout in production.
	// A field rather than a bare constant so a test can shrink it.
	callTimeout time.Duration
	// searchTimeout bounds Recommend's whole search fan-out;
	// recommendSearchTimeout in production. A field so a test can shrink it.
	searchTimeout time.Duration
}

// newForgeServer wires a forgeServer. A nil client defaults to the real
// HTTP adapter (D-01); tests and pack examples inject a fixture instead.
// A nil llm defaults to the real LLM adapter (DefaultLLMClient), the same way
// a nil client defaults to the real Forge adapter; tests and pack examples
// inject a fixture with WithLLMClient instead.
func newForgeServer(packID string, docs *documentsServer, secrets *secretsServer, client ForgeClient, llm LLMClient) *forgeServer {
	if client == nil {
		client = DefaultForgeClient()
	}
	if llm == nil {
		llm = DefaultLLMClient()
	}
	return &forgeServer{packID: packID, client: client, docs: docs, secrets: secrets, llm: llm, callTimeout: llmCallTimeout, searchTimeout: recommendSearchTimeout}
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
	// Environment is optional, but when given it must be a name the Code
	// facet could have created. Rejecting it here (before any Secrets or
	// network work) turns a typo into InvalidArgument instead of a silent
	// all-false already_in_puppetfile (AR-07-01).
	if req.Environment != "" {
		if err := validateEnvName(req.Environment); err != nil {
			return nil, err
		}
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

// forgeModuleKey is the comparison key for a Forge-sourced Puppetfile entry or
// resolved node. It is source-qualified so it can never collide with a
// gitModuleKey, and it delegates to code.CanonicalModuleName so Resolve and
// the Code overwrite gate share one definition of module identity.
func forgeModuleKey(name string) string {
	return "forge\x00" + code.CanonicalModuleName(name)
}

// gitModuleKey is the comparison key for a Git-sourced Puppetfile entry. The
// name is used raw: a Git module's name is a bare module name where a hyphen
// is part of the name, never a namespace separator.
func gitModuleKey(name string) string {
	return "git\x00" + name
}

// currentPuppetfileModuleNames reads env's stored Puppetfile text (if any)
// — through the SAME code-puppetfiles Documents collection and docs.mu
// lock discipline codeServer uses (D-15) — and returns the set of module
// keys it currently contains (see forgeModuleKey and gitModuleKey), so the
// "already there" check (D-08/D-15) is not fooled by a Puppetfile author
// having written the ns-name hyphen form or a different owner case where this
// facet's own results use ns/name. A
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
		// A Git module's name is the bare module name (see
		// PuppetfileModule.name), never a Forge ns-name slug, so a hyphen in
		// it is part of the name and must not be rewritten into a namespace
		// separator. Source qualification, not a different spelling, is what
		// stops a hyphenated Git name from being mistaken for a Forge slug:
		// the two live under different key prefixes and cannot collide.
		if m.GetGit() != nil {
			names[gitModuleKey(m.GetName())] = true
			continue
		}
		names[forgeModuleKey(m.GetName())] = true
	}
	return names
}

// normalizeModuleName converts a module name to its ns/name display and wire
// form, with case preserved. This is NOT the identity key: Search and Resolve
// results (forge_resolver.go's DependencyNode.Name) and the Recommend prompt
// text (recommend_prompt.go) use it so the spelling a pack sees never moves.
// Identity comparisons must use code.CanonicalModuleName instead (through
// forgeModuleKey here); the two call-site groups are deliberately separate, so
// do not unify them. Only the first separator is replaced, mirroring
// forge_client.go's forgeModuleSlug — the module-name segment itself never
// contains a hyphen (Forge's slug grammar restricts it to [a-z][a-z0-9_]*).
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
	node.AlreadyInPuppetfile = present[forgeModuleKey(node.Name)]
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
