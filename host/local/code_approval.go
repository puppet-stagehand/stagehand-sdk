package local

// This file holds the Code facet's overwrite-approval gate: the two refusal
// errors a gated Put* returns, the lookup that decides whether an approved
// proposal covers a target, and the Apply* RPC bodies that materialize an
// approved proposal.
//
// The gate has a hard structural rule. Nothing in this file decides a
// proposal. The decision belongs to the approval package, reached only by a
// caller holding a code:approve token; the code here reads the status that
// decision left behind and never changes it. Keeping "decide" and
// "materialize" in separate code is what stops the gate from defeating
// itself: if materializing a proposal could also move its status, an Apply
// caller could approve its own overwrite.

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// Detail codes attached to the two overwrite refusals, read back by
// IsCodeOverwriteRequiresApproval and IsCodeOverwriteApplyPending.
const (
	detailCodeOverwriteRequiresApproval = "code_overwrite_requires_approval"
	detailCodeOverwriteApplyPending     = "code_overwrite_apply_pending"
)

// overwriteStatusKey and overwriteStatusApproved name the one status entry
// this file reads from a proposal body and the one value that authorizes an
// overwrite. The comparison is plain string equality on a decoded map; there
// is no "already decided" flag a caller could supply instead.
const (
	overwriteStatusKey      = "status"
	overwriteStatusApproved = "approved"
)

// applyPuppetfileModuleRPC is the RPC name the Put-path refusals point a
// caller at.
const applyPuppetfileModuleRPC = "ApplyPuppetfileModuleOverwrite"

// ErrCodeOverwriteRequiresApproval builds the refusal a Put* RPC returns when
// it would replace existing content and no approved proposal covers the
// target t. applyRPC names the Apply RPC the caller uses once one is
// approved. It is FailedPrecondition rather than AlreadyExists on purpose:
// the caller can proceed once a proposal exists and is approved, so this is a
// precondition not yet met, not a permanent conflict.
func ErrCodeOverwriteRequiresApproval(t code.OverwriteTarget, applyRPC string) error {
	msg := "environment " + t.Environment + " already has " + t.Resource + " " + t.Name +
		"; replacing it requires an approved overwrite proposal"
	st := status.New(codes.FailedPrecondition, msg)
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{
		Code:    detailCodeOverwriteRequiresApproval,
		Message: msg,
		Fix: "propose the overwrite into the " + code.OverwriteCollection + " collection, have an operator holding the " +
			code.OverwriteApproveScope + " scope approve it, then call " + applyRPC + " with the proposal id",
	})
	if err != nil {
		return st.Err() // details are best-effort; the status itself must never fail to construct
	}
	return withDetails.Err()
}

// ErrCodeOverwriteApplyPending builds the refusal a Put* RPC returns when an
// approved proposal already covers the target: the overwrite is authorized,
// but a Put never materializes it. That is the Apply RPC's job.
func ErrCodeOverwriteApplyPending(proposalID, applyRPC string) error {
	msg := "overwrite proposal " + proposalID + " is approved but has not been applied; a Put never applies an approved overwrite"
	st := status.New(codes.FailedPrecondition, msg)
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{
		Code:    detailCodeOverwriteApplyPending,
		Message: msg,
		Fix:     "call " + applyRPC + " with proposal id " + proposalID + " instead of the Put RPC",
	})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// IsCodeOverwriteRequiresApproval reports whether err is (or wraps) an
// ErrCodeOverwriteRequiresApproval error. It compares the ErrorDetail code
// and does not branch on codes.FailedPrecondition alone, which a malformed
// body also produces.
func IsCodeOverwriteRequiresApproval(err error) bool {
	return overwriteDetailCode(err) == detailCodeOverwriteRequiresApproval
}

// IsCodeOverwriteApplyPending reports whether err is (or wraps) an
// ErrCodeOverwriteApplyPending error.
func IsCodeOverwriteApplyPending(err error) bool {
	return overwriteDetailCode(err) == detailCodeOverwriteApplyPending
}

// overwriteDetailCode returns the Code of the first *hostv1.ErrorDetail on
// err, or "" when err is nil, is not a status error, or carries none.
func overwriteDetailCode(err error) string {
	if err == nil {
		return ""
	}
	for _, d := range status.Convert(err).Details() {
		if ed, ok := d.(*hostv1.ErrorDetail); ok {
			return ed.Code
		}
	}
	return ""
}

// approvedOverwriteProposalLocked returns the id of the first approved
// overwrite proposal whose target equals t, and whether one exists. Two
// targets are equal only when all five fields are equal by plain struct
// equality; no prefix, substring or case-insensitive comparison exists on
// this path. It iterates idsLocked, whose sort.Strings ordering is the
// specified tie-break when several approved proposals cover one target: the
// lowest doc id wins, deterministically, never map iteration order. A
// document whose status is not exactly "approved", or whose target cannot be
// read, covers nothing. The caller must already hold s.docs.mu; this method
// calls no documentsServer gRPC method, because those take the same lock.
func (s *codeServer) approvedOverwriteProposalLocked(t code.OverwriteTarget) (string, bool) {
	for _, id := range s.docs.idsLocked(code.OverwriteCollection) {
		doc, ok := s.docs.getLocked(code.OverwriteCollection, id)
		if !ok || doc.Body == nil || doc.Body.Value == nil {
			continue
		}
		body := doc.Body.Value.AsMap()
		if st, isStr := body[overwriteStatusKey].(string); !isStr || st != overwriteStatusApproved {
			continue
		}
		got, err := code.ParseOverwriteTarget(body)
		if err != nil {
			continue
		}
		if got == t {
			return id, true
		}
	}
	return "", false
}

// approvedOverwriteBody resolves proposalID through the Documents store and
// returns its decoded body, refusing unless its status is exactly the string
// "approved". It must be called with NO lock held: it uses the documentsServer
// Get method, which takes s.docs.mu itself. The store's own NotFound is
// returned unchanged for an unknown proposal id.
func (s *codeServer) approvedOverwriteBody(ctx context.Context, proposalID string) (map[string]any, error) {
	doc, err := s.docs.Get(ctx, &hostv1.GetDocumentRequest{Collection: code.OverwriteCollection, DocId: proposalID})
	if err != nil {
		return nil, err
	}
	if doc.Body == nil || doc.Body.Value == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q has no body", proposalID)
	}
	body := doc.Body.Value.AsMap()
	if st, isStr := body[overwriteStatusKey].(string); !isStr || st != overwriteStatusApproved {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q is not approved", proposalID)
	}
	return body, nil
}

// ApplyPuppetfileModuleOverwrite materializes an approved puppetfile_module
// overwrite proposal. It takes a proposal id and nothing else: the module it
// writes is the payload frozen in the proposal at propose time, so an
// approved proposal can never be used to write content its approver did not
// see. It reads a decision someone else recorded and is the only thing here
// that writes the Puppetfile on an overwrite's behalf. It never consults a
// token, never evaluates a scope and never writes into the overwrite
// collection; the decision and the materialization stay in separate code.
//
// Phase one runs with no lock held and resolves and validates the proposal.
// Phase two takes s.docs.mu once, re-reads the current model under that lock
// rather than trusting anything read earlier, and uses only *Locked helpers,
// so two concurrent calls converge on one stored Puppetfile. The call is
// idempotent: when the stored module already equals the frozen payload it
// returns it without writing.
func (s *codeServer) ApplyPuppetfileModuleOverwrite(ctx context.Context, req *hostv1.ApplyPuppetfileModuleOverwriteRequest) (*hostv1.PuppetfileModule, error) {
	body, err := s.approvedOverwriteBody(ctx, req.ProposalId)
	if err != nil {
		return nil, err
	}

	target, err := code.ParseOverwriteTarget(body)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q: %v", req.ProposalId, err)
	}
	if target.Resource != code.OverwriteResourcePuppetfileModule {
		return nil, status.Errorf(codes.FailedPrecondition,
			"overwrite proposal %q targets a %q, not a %q; use the matching Apply RPC",
			req.ProposalId, target.Resource, code.OverwriteResourcePuppetfileModule)
	}
	if err := validateEnvName(target.Environment); err != nil {
		return nil, err
	}
	frozen, err := code.OverwritePayloadPuppetfileModule(body)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q: %v", req.ProposalId, err)
	}
	if frozen.GetName() != target.Name {
		return nil, status.Errorf(codes.FailedPrecondition,
			"overwrite proposal %q names module %q in its target but carries a payload for %q",
			req.ProposalId, target.Name, frozen.GetName())
	}
	if err := code.ValidateModule(frozen); err != nil {
		return nil, mapPuppetfileErr(err)
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, target.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", target.Environment)
	}

	pf, err := s.loadPuppetfileLocked(target.Environment)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i, m := range pf.Modules {
		if m.GetName() == frozen.GetName() {
			idx = i
			break
		}
	}
	if idx >= 0 && proto.Equal(pf.Modules[idx], frozen) {
		return clonePuppetfileModule(pf.Modules[idx]), nil
	}

	written := clonePuppetfileModule(frozen)
	if idx >= 0 {
		pf.Modules[idx] = written
	} else {
		pf.Modules = append(pf.Modules, written)
	}
	if err := s.storePuppetfileLocked(target.Environment, pf); err != nil {
		return nil, err
	}
	return clonePuppetfileModule(written), nil
}
