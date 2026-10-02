// Package controlrepoauthoring is the v0.3.0-rc.1 milestone's
// facet-sufficiency proof for the Code facet, the Forge facet, Recommend, the
// generalized approval gate and import: it composes author -> propose ->
// approve -> apply -> read back into one end-to-end control-repo authoring
// workflow against host.Local, the way examples/inventory-onboarding proved
// the Inventory facet and the approval package sufficient for a real
// consumer.
//
// It is not a network-listening server, not a real Expansion Pack, and ships
// no OpenAPI fragment — pack-build, the tool that generates OpenAPI fragments
// from a manifest's routes, does not exist yet in Preview 0.0.1. manifest.json
// declares this package's four routes anyway, because a manifest fixture is
// part of what this example ships, but no fragment file backs operation_id
// references until that tool lands.
//
// It is author-only. It does not deploy anything and it does not write back
// to a real control repo: every byte it authors lives in host.Local's
// in-memory Code facet and is read back from there.
//
// What this proves, in two parts:
//  1. The Code facet and the approval package genuinely compose into a real
//     authoring workflow: a blank environment receives authored content, and
//     an environment settings record exists only because a second persona
//     approved a proposal for it.
//  2. The proposing and deciding call graphs are structurally separate.
//     docs/approval-pattern.md states that no code inside approval/ itself
//     can enforce this — the package hands both propose and approve/reject
//     to its caller as independent entry points. This package is that caller,
//     split into two persona types so the separation is visible in the call
//     graph rather than left to a code review's trust.
//
// Every fixture value this example and its tests choose is deliberately
// different from a Puppet default, so that no assertion can pass by
// accidentally matching one.
package controlrepoauthoring

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// CodeKind is the approval.Kind this example's two personas share: the
// code-overwrites collection every gated Code overwrite is proposed into and
// the code:approve scope required to decide one. It is a package-level var
// built from the exported code constants, so no request field, proposal body
// or persona method can choose the scope that decides the persona's own
// proposals (GOV-01).
var CodeKind = approval.Kind{Collection: code.OverwriteCollection, ApproveScope: code.OverwriteApproveScope}

// ProposerBackend models the pack worker's persona: it authors content,
// proposes replacements, and applies an already-decided proposal. It holds no
// approval token, never obtains one, and never decides — every method is
// reachable only through the Code facet and the approval package's propose
// surface.
type ProposerBackend struct {
	h *host.Host
}

// NewProposer builds a ProposerBackend over an existing *host.Host.
func NewProposer(h *host.Host) *ProposerBackend { return &ProposerBackend{h: h} }

// ApproverBackend models the operator's decision path: it presents a token
// someone else obtained and records a decision. It never proposes and never
// touches the Code or Forge facets — deciding and authoring are different
// acts performed by different actors.
type ApproverBackend struct {
	h *host.Host
}

// NewApprover builds an ApproverBackend over an existing *host.Host.
func NewApprover(h *host.Host) *ApproverBackend { return &ApproverBackend{h: h} }

// CreateEnvironment creates a blank environment named name. The facet error
// is returned unwrapped so callers can use status.Code and the local.Is*
// predicates.
func (p *ProposerBackend) CreateEnvironment(ctx context.Context, name string) (*hostv1.Environment, error) {
	return p.h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: name})
}

// AddModule writes module into env's Puppetfile. This is an ungated authoring
// write: adding a module to an environment never replaces existing content.
func (p *ProposerBackend) AddModule(ctx context.Context, env string, m *hostv1.PuppetfileModule) (*hostv1.PuppetfileModule, error) {
	return p.h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: env, Module: m})
}

// ProposeSettings freezes s as a proposal body and files it with the approval
// gate. Environment settings are authored only through this path: the body is
// built by code.OverwriteBodyForSettings and filed with approval.ProposeBody,
// and this method never builds a document write itself — approval owns the
// create-only semantics, and re-encoding them here would duplicate
// security-critical logic outside the package meant to own it.
func (p *ProposerBackend) ProposeSettings(ctx context.Context, proposalID string, s *hostv1.EnvironmentSettings) (*approval.Proposal, error) {
	body, err := code.OverwriteBodyForSettings(s)
	if err != nil {
		return nil, err
	}
	return approval.ProposeBody(ctx, p.h, CodeKind, proposalID, body)
}

// ApplySettings materializes proposalID's approved settings proposal into the
// Code facet. It passes only the proposal id: the content applied is exactly
// what the approver decided on, never anything this caller supplies.
//
// Call each Apply* immediately after Approve returns on the same path, never
// after a separate proposal read, because a read carries no authority of its
// own — two callers can both observe a pending proposal before either one
// commits a decision.
func (p *ProposerBackend) ApplySettings(ctx context.Context, proposalID string) (*hostv1.EnvironmentSettings, error) {
	return p.h.Code.ApplyEnvironmentSettings(ctx, &hostv1.ApplyEnvironmentSettingsRequest{ProposalId: proposalID})
}

// ListEnvironments returns every environment the Code facet holds, in order.
func (p *ProposerBackend) ListEnvironments(ctx context.Context) ([]*hostv1.Environment, error) {
	resp, err := p.h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Environments, nil
}

// RenderPuppetfile returns env's Puppetfile exactly as the Code facet renders
// it.
func (p *ProposerBackend) RenderPuppetfile(ctx context.Context, env string) (string, error) {
	resp, err := p.h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: env})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// ListModules returns env's Puppetfile modules.
func (p *ProposerBackend) ListModules(ctx context.Context, env string) ([]*hostv1.PuppetfileModule, error) {
	resp, err := p.h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: env})
	if err != nil {
		return nil, err
	}
	return resp.Modules, nil
}

// Settings returns env's environment settings record.
func (p *ProposerBackend) Settings(ctx context.Context, env string) (*hostv1.EnvironmentSettings, error) {
	return p.h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: env})
}

// Approve presents tokenSecret (obtained by the caller, never by this
// package) to approval.Approve. The token secret arrives from outside this
// process, held by the person deciding, and nothing in this package can
// produce it.
func (a *ApproverBackend) Approve(ctx context.Context, proposalID, tokenSecret string) (*approval.Proposal, error) {
	return approval.Approve(ctx, a.h, approval.ApproveRequest{Kind: CodeKind, ProposalID: proposalID, TokenSecret: tokenSecret})
}

// Reject presents tokenSecret (obtained by the caller, never by this
// package) to approval.Reject along with a required human-readable reason.
func (a *ApproverBackend) Reject(ctx context.Context, proposalID, tokenSecret, reason string) (*approval.Proposal, error) {
	return approval.Reject(ctx, a.h, approval.RejectRequest{Kind: CodeKind, ProposalID: proposalID, TokenSecret: tokenSecret, Reason: reason})
}

// RecommendModules turns a free-text need into ranked module suggestions. The
// host asks the named LLM provider for search queries, runs real registry
// searches, then asks the provider to rank the real results; every module fact
// in the response is a host-owned copy of a search result, so a module the
// model invents is dropped with a warning rather than returned. provider names
// an operator-configured entry and has no default.
func (p *ProposerBackend) RecommendModules(ctx context.Context, text, provider string) (*hostv1.RecommendResponse, error) {
	return p.h.Forge.Recommend(ctx, &hostv1.RecommendRequest{Text: text, LlmProvider: provider, MaxSuggestions: 3})
}

// SearchModules checks a candidate against one registry source. Searching is
// read-only: it confirms a module exists and shows its registry metadata.
func (p *ProposerBackend) SearchModules(ctx context.Context, query, source string) (*hostv1.SearchResponse, error) {
	return p.h.Forge.Search(ctx, &hostv1.SearchRequest{Query: query, Source: &hostv1.ForgeSourceSelection{Name: source}})
}

// ResolveModule returns the advisory dependency tree for one module release,
// annotated against env's current Puppetfile. name and version are required
// by the facet and are passed through unmodified. The tree is advice, never a
// preview of a real deploy, and resolving never writes a Puppetfile: each
// module still needs its own explicit AddModule.
func (p *ProposerBackend) ResolveModule(ctx context.Context, name, version, env string) (*hostv1.ResolveResponse, error) {
	return p.h.Forge.Resolve(ctx, &hostv1.ResolveRequest{Name: name, Version: version, Environment: env})
}

// scalarValue wraps v in the single-field "v" convention that Hiera data keys
// use across the host.Local and approval package boundaries. Later plans use
// it to write Hiera data values.
func scalarValue(v any) (*hostv1.Json, error) {
	s, err := structpb.NewStruct(map[string]any{"v": v})
	if err != nil {
		return nil, err
	}
	return &hostv1.Json{Value: s}, nil
}
