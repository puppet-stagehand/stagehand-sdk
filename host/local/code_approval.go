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
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

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

// overwriteAppliedCollection is the Code facet's own record of which approved
// proposals have already been materialized, keyed by proposal id. It is not a
// status change on the proposal, so the "Apply never decides" rule holds: the
// approval package owns the proposal document and this file only writes a
// separate marker beside it. A marker is what stops an approval from being a
// standing permission: once a proposal is applied, an Apply call is only
// honoured when it would change nothing.
const overwriteAppliedCollection = "code-overwrite-applied"

// The Apply RPC names the Put-path refusals point a caller at.
const (
	applyPuppetfileModuleRPC     = "ApplyPuppetfileModuleOverwrite"
	applyEnvironmentSettingsRPC  = "ApplyEnvironmentSettings"
	applyEnvironmentDuplicateRPC = "ApplyEnvironmentDuplicate"
	applyHieraLevelRPC           = "ApplyHieraLevelOverwrite"
	applyHieraDataKeyRPC         = "ApplyHieraDataKeyOverwrite"
)

// describeOverwriteTarget words what the refused write would replace, per
// resource kind, for the refusal message.
func describeOverwriteTarget(t code.OverwriteTarget) string {
	switch t.Resource {
	case code.OverwriteResourceSettings:
		return "environment " + t.Environment + " already has a settings record"
	case code.OverwriteResourceEnvironment:
		return "environment " + t.Environment + " already exists and would be replaced by a copy of " + t.Source
	case code.OverwriteResourceHieraDataKey:
		return "environment " + t.Environment + " already has hiera data key " + t.Name + " in " + t.Path
	default:
		return "environment " + t.Environment + " already has " + t.Resource + " " + t.Name
	}
}

// ErrCodeOverwriteRequiresApproval builds the refusal a Put* RPC returns when
// it would replace existing content and no approved proposal covers the
// target t. applyRPC names the Apply RPC the caller uses once one is
// approved. It is FailedPrecondition rather than AlreadyExists on purpose:
// the caller can proceed once a proposal exists and is approved, so this is a
// precondition not yet met, not a permanent conflict.
func ErrCodeOverwriteRequiresApproval(t code.OverwriteTarget, applyRPC string) error {
	msg := describeOverwriteTarget(t) + "; replacing it requires an approved overwrite proposal"
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

// overwriteAppliedLocked reports whether proposalID has already been
// materialized by an Apply RPC. The caller must already hold s.docs.mu.
func (s *codeServer) overwriteAppliedLocked(proposalID string) bool {
	_, ok := s.docs.getLocked(overwriteAppliedCollection, proposalID)
	return ok
}

// markOverwriteAppliedLocked records that proposalID has been materialized. It
// is a no-op when the marker already exists. The caller must already hold
// s.docs.mu and must call it only after the Apply's write (or its verified
// no-op) succeeded.
func (s *codeServer) markOverwriteAppliedLocked(proposalID string) error {
	if s.overwriteAppliedLocked(proposalID) {
		return nil
	}
	st, err := structpb.NewStruct(map[string]any{"applied_at": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return status.Errorf(codes.Internal, "record applied overwrite %q: %v", proposalID, err)
	}
	if _, err := s.docs.putLocked(overwriteAppliedCollection, proposalID, &hostv1.Json{Value: st}, true); err != nil {
		return status.Errorf(codes.Internal, "record applied overwrite %q: %v", proposalID, err)
	}
	return nil
}

// errOverwriteAlreadyApplied is the refusal an Apply RPC returns when its
// proposal was already applied and the target no longer matches what the
// proposal describes. Repeating an applied approval is only allowed when it
// would change nothing; anything else needs a new proposal and a new approval.
func errOverwriteAlreadyApplied(proposalID string) error {
	return status.Errorf(codes.FailedPrecondition,
		"overwrite proposal %q was already applied and the target has changed since; an approval covers one application, so propose the change again and have it approved",
		proposalID)
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

// resolveApplyProposal is phase one of every Apply RPC: with no lock held it
// resolves proposalID, requires the status string "approved", parses the
// target and refuses a resource other than wantResource. It reads a decision
// someone else recorded and never changes it.
func (s *codeServer) resolveApplyProposal(ctx context.Context, proposalID, wantResource string) (map[string]any, code.OverwriteTarget, error) {
	body, err := s.approvedOverwriteBody(ctx, proposalID)
	if err != nil {
		return nil, code.OverwriteTarget{}, err
	}
	target, err := code.ParseOverwriteTarget(body)
	if err != nil {
		return nil, code.OverwriteTarget{}, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q: %v", proposalID, err)
	}
	if target.Resource != wantResource {
		return nil, code.OverwriteTarget{}, status.Errorf(codes.FailedPrecondition,
			"overwrite proposal %q targets a %q, not a %q; use the matching Apply RPC",
			proposalID, target.Resource, wantResource)
	}
	if err := validateEnvName(target.Environment); err != nil {
		return nil, code.OverwriteTarget{}, err
	}
	return body, target, nil
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

// ApplyEnvironmentSettings materializes an approved settings overwrite
// proposal. It takes a proposal id and nothing else: the record it writes is
// the payload frozen in the proposal at propose time, so an approved proposal
// can never write settings its approver did not see. Like every Apply RPC it
// only reads the decision, never records one.
//
// Phase one resolves and validates the proposal with no lock held. Phase two
// takes s.docs.mu once, re-reads the environment under that lock and uses only
// *Locked helpers. The call is idempotent: when the stored settings already
// equal the frozen record it returns them without writing.
func (s *codeServer) ApplyEnvironmentSettings(ctx context.Context, req *hostv1.ApplyEnvironmentSettingsRequest) (*hostv1.EnvironmentSettings, error) {
	body, target, err := s.resolveApplyProposal(ctx, req.ProposalId, code.OverwriteResourceSettings)
	if err != nil {
		return nil, err
	}
	frozen, err := code.OverwritePayloadSettings(body)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q: %v", req.ProposalId, err)
	}
	if err := checkSettingsRoundTrip(frozen); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	doc, ok := s.docs.getLocked(envCollection, target.Environment)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", target.Environment)
	}
	current, err := settingsFromDoc(doc, target.Environment)
	if err != nil {
		return nil, err
	}
	if proto.Equal(current, frozen) {
		return proto.Clone(current).(*hostv1.EnvironmentSettings), nil
	}

	newBody, err := settingsIntoBody(doc.Body, frozen)
	if err != nil {
		return nil, err
	}
	if _, err := s.docs.putLocked(envCollection, target.Environment, newBody, false); err != nil {
		return nil, err
	}
	stored, ok := s.docs.getLocked(envCollection, target.Environment)
	if !ok {
		return nil, status.Errorf(codes.Internal, "environment %q vanished immediately after settings write", target.Environment)
	}
	result, err := settingsFromDoc(stored, target.Environment)
	if err != nil {
		return nil, err
	}
	return proto.Clone(result).(*hostv1.EnvironmentSettings), nil
}

// ApplyEnvironmentDuplicate materializes an approved environment-duplicate
// proposal: the target environment's whole owned document set is replaced with
// a copy of the source environment's.
//
// This differs from the four content-write Apply RPCs on purpose. Those
// materialize the exact payload frozen in the proposal at propose time. Here
// what the approver approved is the operation, replace the target with a copy
// of the source, so the source is read at apply time and no document set is
// frozen in the proposal. That is deliberate, not an oversight.
//
// Phase one resolves and validates the proposal with no lock held. Phase two
// takes s.docs.mu once for the whole materialize step: it plans the rekeyed
// copies with the same two helpers DuplicateEnvironment uses, so ownership is
// decided in exactly one place, deletes everything the target owns, then writes
// every planned copy. No documentsServer gRPC method runs in between, so a
// concurrent reader sees either the complete replacement or the prior state,
// never a half-copied environment. The call is idempotent: when the target
// already holds exactly the planned documents it returns without writing.
//
// An approval covers one application. The source is read at apply time, so a
// proposal that stayed usable forever would let one approval stamp whatever the
// source later became over the target. Once the proposal has been applied, a
// repeat call succeeds only when the target already equals the planned copy (a
// retry after a lost response); if the source or target has moved on it is
// refused and a new proposal is needed.
func (s *codeServer) ApplyEnvironmentDuplicate(ctx context.Context, req *hostv1.ApplyEnvironmentDuplicateRequest) (*hostv1.Environment, error) {
	_, target, err := s.resolveApplyProposal(ctx, req.ProposalId, code.OverwriteResourceEnvironment)
	if err != nil {
		return nil, err
	}
	if err := validateEnvName(target.Source); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q has no usable source environment: %v", req.ProposalId, err)
	}
	if target.Source == target.Environment {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q names %q as both source and target", req.ProposalId, target.Source)
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, target.Source); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", target.Source)
	}

	writes, err := s.planRekeyedWritesLocked(s.envOwnedDocsLocked(target.Source), target.Environment)
	if err != nil {
		return nil, err
	}
	existing := s.envOwnedDocsLocked(target.Environment)

	if !s.targetMatchesPlanLocked(existing, writes) {
		// The source is read at apply time, so an approval that could be
		// replayed would let the approver's one decision copy whatever the
		// source has become. An applied proposal is therefore single-use: a
		// repeat call is honoured only when it would write nothing.
		if s.overwriteAppliedLocked(req.ProposalId) {
			return nil, errOverwriteAlreadyApplied(req.ProposalId)
		}
		for _, ref := range existing {
			s.docs.deleteLocked(ref.collection, ref.docID)
		}
		for _, w := range writes {
			if _, err := s.docs.putLocked(w.collection, w.docID, w.body, true); err != nil {
				return nil, status.Errorf(codes.Internal, "apply duplicate: writing %s/%s: %v", w.collection, w.docID, err)
			}
		}
	}
	if err := s.markOverwriteAppliedLocked(req.ProposalId); err != nil {
		return nil, err
	}

	doc, ok := s.docs.getLocked(envCollection, target.Environment)
	if !ok {
		return nil, status.Errorf(codes.Internal, "duplicated environment %q vanished immediately", target.Environment)
	}
	env, err := environmentFromDoc(doc)
	if err != nil {
		return nil, err
	}
	return cloneEnvironment(env), nil
}

// targetMatchesPlanLocked reports whether the documents existing already
// hold exactly what writes plans: the same set of collection and doc id pairs,
// each with a proto.Equal body. It is what makes ApplyEnvironmentDuplicate
// idempotent without churning document versions. The caller must already hold
// s.docs.mu.
func (s *codeServer) targetMatchesPlanLocked(existing []docRef, writes []plannedDocWrite) bool {
	if len(existing) != len(writes) {
		return false
	}
	have := make(map[docRef]*hostv1.Json, len(existing))
	for _, ref := range existing {
		doc, ok := s.docs.getLocked(ref.collection, ref.docID)
		if !ok {
			return false
		}
		have[ref] = doc.Body
	}
	for _, w := range writes {
		body, ok := have[docRef{collection: w.collection, docID: w.docID}]
		if !ok || !proto.Equal(body, w.body) {
			return false
		}
	}
	return true
}

// ApplyHieraLevelOverwrite materializes an approved hiera_level overwrite
// proposal. It takes a proposal id and nothing else: the level, index and
// insert flag it writes are the ones frozen in the proposal at propose time.
// Like every Apply RPC it only reads the decision, never records one.
//
// Phase one resolves and validates the proposal with no lock held. Phase two
// takes s.docs.mu once, re-reads the stored hierarchy under that lock and uses
// only *Locked helpers. When a level of the approved name exists it is
// replaced in place so the hierarchy order is preserved; the frozen insert
// flag is only honoured when no such level exists, since inserting a second
// level of the same name would corrupt the hierarchy. The call is idempotent:
// when the stored level already equals the approved one nothing is written.
func (s *codeServer) ApplyHieraLevelOverwrite(ctx context.Context, req *hostv1.ApplyHieraLevelOverwriteRequest) (*hostv1.PutHieraLevelResponse, error) {
	body, target, err := s.resolveApplyProposal(ctx, req.ProposalId, code.OverwriteResourceHieraLevel)
	if err != nil {
		return nil, err
	}
	lvl, index, insert, err := code.OverwritePayloadHieraLevel(body)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q: %v", req.ProposalId, err)
	}
	if lvl.GetName() != target.Name {
		return nil, status.Errorf(codes.FailedPrecondition,
			"overwrite proposal %q names level %q in its target but carries a payload for %q",
			req.ProposalId, target.Name, lvl.GetName())
	}
	if len(lvl.GetLookupOptions()) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"overwrite proposal %q carries lookup_options, which is read-only and cannot be written", req.ProposalId)
	}
	warnings := code.LintLevelPaths(lvl)

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, target.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", target.Environment)
	}

	text, _ := s.hierarchyTextLocked(target.Environment)
	current, err := code.ParseHierarchy(text)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	idx := -1
	for i, existing := range current.GetLevels() {
		if existing.GetName() == lvl.GetName() {
			idx = i
			break
		}
	}
	if idx < 0 && !insert {
		return nil, status.Errorf(codes.FailedPrecondition,
			"overwrite proposal %q replaces level %q in place but the hierarchy no longer has it", req.ProposalId, lvl.GetName())
	}

	if idx < 0 || !proto.Equal(current.GetLevels()[idx], lvl) {
		newText, err := code.PutLevel(text, lvl, index, insert && idx < 0)
		if err != nil {
			return nil, mapHieraErr(err)
		}
		if err := s.storeHierarchyLocked(target.Environment, newText); err != nil {
			return nil, err
		}
	}

	stored, _ := s.hierarchyTextLocked(target.Environment)
	h, err := code.ParseHierarchy(stored)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	h.Environment = target.Environment
	s.mirrorLookupOptionsLocked(target.Environment, h)
	return &hostv1.PutHieraLevelResponse{
		Hierarchy: proto.Clone(h).(*hostv1.HieraHierarchy),
		Warnings:  warnings,
	}, nil
}

// ApplyHieraDataKeyOverwrite materializes an approved hiera_data_key overwrite
// proposal: the value frozen in the proposal replaces the key's value in the
// named data file, through code.PutDataKey so comments and key order survive.
// Like every Apply RPC it only reads the decision, never records one.
//
// Phase one resolves and validates the proposal with no lock held. Phase two
// takes s.docs.mu once and uses only *Locked helpers. The call is idempotent:
// when the file already holds the approved value under that key nothing is
// written.
func (s *codeServer) ApplyHieraDataKeyOverwrite(ctx context.Context, req *hostv1.ApplyHieraDataKeyOverwriteRequest) (*hostv1.HieraDataFile, error) {
	body, target, err := s.resolveApplyProposal(ctx, req.ProposalId, code.OverwriteResourceHieraDataKey)
	if err != nil {
		return nil, err
	}
	if target.Name == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q names no data key", req.ProposalId)
	}
	if err := code.ValidateDataPath(target.Path); err != nil {
		return nil, mapHieraErr(err)
	}
	value, err := code.OverwritePayloadHieraDataKey(body)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q: %v", req.ProposalId, err)
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, target.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", target.Environment)
	}

	text, present := s.dataFileTextLocked(target.Environment, target.Path)
	unchanged := false
	if present {
		df, err := code.ParseDataFile(text)
		if err != nil {
			return nil, mapHieraErr(err)
		}
		if have, exists := df.Values[target.Name]; exists && proto.Equal(have, value) {
			unchanged = true
		}
	}
	if !unchanged {
		newText, err := code.PutDataKey(text, target.Name, value)
		if err != nil {
			return nil, mapHieraErr(err)
		}
		if err := s.storeDataFileLocked(target.Environment, target.Path, newText); err != nil {
			return nil, err
		}
	}

	stored, _ := s.dataFileTextLocked(target.Environment, target.Path)
	df, err := code.ParseDataFile(stored)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	df.Environment = target.Environment
	df.Path = target.Path
	return proto.Clone(df).(*hostv1.HieraDataFile), nil
}
