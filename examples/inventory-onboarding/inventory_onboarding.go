// Package inventoryonboarding is the v0.2.0-rc.1 milestone's
// facet-sufficiency proof for the Inventory facet plus the approval/
// governance package: it composes discover -> group -> attach facts ->
// propose -> approve -> onboard -> query into one end-to-end workflow
// against host.Local, the way examples/opentofu-lite proved the first four
// facets sufficient for a real consumer.
//
// It is not a network-listening server, not a real Expansion Pack, and
// ships no OpenAPI fragment — pack-build, the tool that generates OpenAPI
// fragments from a manifest's routes, does not exist yet in Preview 0.0.1
// (CLAUDE.md's own roadmap note lists it as a later phase). manifest.json
// declares this package's three routes anyway, because a manifest fixture
// is part of what this example ships (ROADMAP criterion 3), but no
// fragment file backs operation_id references until that tool lands.
//
// What this proves, in two parts:
//  1. The Inventory facet and the approval package genuinely compose into
//     a real onboarding workflow — not a toy CRUD example — ending with a
//     node present, ONBOARDED, still grouped, and returned by a fact query.
//  2. The proposing and deciding call graphs are structurally separate.
//     docs/approval-pattern.md states that no code inside approval/ itself
//     can enforce this — the package hands both Propose and Approve/Reject
//     to its caller as independent entry points, and it is entirely up to
//     the caller not to build a bridge between them. This package is that
//     caller, split into two persona types precisely so that separation is
//     visible in the call graph rather than left to a code review's trust.
//
// This example deliberately uses its own Discover candidate set
// ("web-07.example.test", group "production-web" with classes
// "profile::base" and "role::web_server") rather than host.Local's
// built-in defaults ("db-01.example.test" / "web-01.example.test", group
// "webservers") — mirroring opentofu-lite's stateVersionRetention choice —
// so that no assertion in this package's tests can pass by accidentally
// matching a default that may change later.
package inventoryonboarding

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// OnboardingKind is the approval.Kind this example's two personas share:
// the "inventory-proposals" collection host/local's OnboardNode reads and
// the "inventory:approve" scope required to decide a proposal in it. It is
// a code-defined package-level var, deliberately not derived from any
// request field, proposal body, or method parameter reachable by the
// proposing persona, and deliberately not built by a method or constructor
// on either persona — the proposing persona must not be able to choose the
// scope required to decide its own proposals (GOV-01, D-06/D-07).
// OnboardNode keeps its name, request/response shape and wire contract;
// only the internal approval call sites carry this value (D-08).
//
// The Collection literal must match host/local's unexported
// proposalCollection (host/local/inventory.go): OnboardNode resolves a
// proposal_id through that collection, so drift between the two would leave
// every approval invisible to it.
var OnboardingKind = approval.Kind{Collection: "inventory-proposals", ApproveScope: "inventory:approve"}

// ProposerBackend models the pack worker's persona: it discovers
// candidates, groups them, attaches facts, proposes onboarding, and
// materializes an already-decided proposal. It holds no approval token,
// never obtains one, and reaches no function that obtains one — every
// method on this type is reachable only through the Inventory facet and
// the approval package's Propose/Get surface.
type ProposerBackend struct {
	h *host.Host
}

// NewProposer builds a ProposerBackend over an existing *host.Host.
func NewProposer(h *host.Host) *ProposerBackend { return &ProposerBackend{h: h} }

// ApproverBackend models the operator's decision path: it presents a token
// someone else obtained (never one it minted itself) and records a
// decision. It never creates a proposal and never touches the Inventory
// facet directly — deciding and materializing an onboarding are different
// acts performed by different actors.
type ApproverBackend struct {
	h *host.Host
}

// NewApprover builds an ApproverBackend over an existing *host.Host.
func NewApprover(h *host.Host) *ApproverBackend { return &ApproverBackend{h: h} }

// Discover returns every not-yet-onboarded candidate host.Local knows
// about (this example's own fixture set, injected via
// local.WithDiscoverCandidates in the test, not host.Local's built-in
// defaults).
func (p *ProposerBackend) Discover(ctx context.Context) ([]*hostv1.Node, error) {
	resp, err := p.h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, err
	}
	return resp.Candidates, nil
}

// AddToGroup adds nodeID to groupID's membership set. This works on a node
// that is still DISCOVERED — deliberately: the milestone's flow groups a
// node before it is ever onboarded, and Inventory's AddNodeToGroup places
// no status restriction on the node it operates on.
func (p *ProposerBackend) AddToGroup(ctx context.Context, nodeID, groupID string) error {
	_, err := p.h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: nodeID, GroupId: groupID})
	return err
}

// AttachFacts converts each entry of facts into the single-field "v"
// convention via scalarFact and writes them onto nodeID through PutFacts,
// returning the updated node. Facts attach while the node is still
// DISCOVERED, the same way AddToGroup does.
func (p *ProposerBackend) AttachFacts(ctx context.Context, nodeID string, facts map[string]any) (*hostv1.Node, error) {
	wrapped := make(map[string]*hostv1.Json, len(facts))
	for k, v := range facts {
		j, err := scalarFact(v)
		if err != nil {
			return nil, err
		}
		wrapped[k] = j
	}
	return p.h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: nodeID, Facts: wrapped})
}

// GroupClasses returns the classes assigned to groupID.
func (p *ProposerBackend) GroupClasses(ctx context.Context, groupID string) ([]*hostv1.Class, error) {
	resp, err := p.h.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: groupID})
	if err != nil {
		return nil, err
	}
	return resp.Classes, nil
}

// ProposeOnboarding reads nodeID back out of Inventory (rather than
// accepting a caller-built node) and hands it straight to approval.Propose.
// Reading from Inventory is what carries every fact AttachFacts wrote into
// the proposal body. This method never builds a PutDocumentRequest itself:
// approval.Propose owns the create-only CAS semantics, and re-encoding
// them here would duplicate security-critical logic outside the package
// meant to own it. approval's error is returned unwrapped so a caller can
// branch on status.Code and on approval.IsAlreadyDecided.
func (p *ProposerBackend) ProposeOnboarding(ctx context.Context, nodeID string) (*approval.Proposal, error) {
	node, err := p.h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: nodeID})
	if err != nil {
		return nil, err
	}
	return approval.Propose(ctx, p.h, node, OnboardingKind)
}

// Onboard materializes proposalID's decided proposal into Inventory. A
// caller must invoke this only after a decide call (Approve or Reject) has
// returned success on the same call path — never off a separate read of
// the proposal via approval.Get — because two callers can both observe a
// pending proposal before either one commits its decision, and a read
// carries no authority of its own.
func (p *ProposerBackend) Onboard(ctx context.Context, proposalID string) (*hostv1.Node, error) {
	return p.h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: proposalID})
}

// FindByFact queries Inventory for nodes whose fact named field equals
// value, wrapping value in the same single-field "v" convention scalarFact
// uses for a write.
func (p *ProposerBackend) FindByFact(ctx context.Context, field string, value any) ([]*hostv1.Node, error) {
	j, err := scalarFact(value)
	if err != nil {
		return nil, err
	}
	resp, err := p.h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{Field: field, Op: hostv1.QueryNodesRequest_EQ, Value: j})
	if err != nil {
		return nil, err
	}
	return resp.Nodes, nil
}

// Approve presents tokenSecret (obtained by the caller, never by this
// package) to approval.Approve. The token secret parameter is the whole
// point: it arrives from outside this process, held by the person
// deciding, and nothing in this package can produce it.
func (a *ApproverBackend) Approve(ctx context.Context, proposalID, tokenSecret string) (*approval.Proposal, error) {
	return approval.Approve(ctx, a.h, approval.ApproveRequest{Kind: OnboardingKind, ProposalID: proposalID, TokenSecret: tokenSecret})
}

// Reject presents tokenSecret (obtained by the caller, never by this
// package) to approval.Reject along with a required human-readable reason.
func (a *ApproverBackend) Reject(ctx context.Context, proposalID, tokenSecret, reason string) (*approval.Proposal, error) {
	return approval.Reject(ctx, a.h, approval.RejectRequest{Kind: OnboardingKind, ProposalID: proposalID, TokenSecret: tokenSecret, Reason: reason})
}

// scalarFact wraps v in the single-field "v" convention
// (host/local/inventory.go's jsonScalar and approval's unwrapFact are the
// read-side partners across two package boundaries) — all three must agree
// on this convention or facts arrive corrupted after onboarding.
func scalarFact(v any) (*hostv1.Json, error) {
	s, err := structpb.NewStruct(map[string]any{"v": v})
	if err != nil {
		return nil, err
	}
	return &hostv1.Json{Value: s}, nil
}
