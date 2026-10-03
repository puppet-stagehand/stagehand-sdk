package local

import (
	"context"
	"sort"
	"strconv"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// proposalCollection is the D-08 Documents collection name OnboardNode
// resolves a proposal_id through — the single definition Phase 4's
// approval package and Phase 5's example both bind to. The literal is
// duplicated as OnboardingKind.Collection in
// examples/inventory-onboarding/inventory_onboarding.go; keep the two in
// step, because OnboardNode only sees approvals written to this collection.
// inventoryApprovalKind below pins the approval scope alongside it.
const proposalCollection = "inventory-proposals"

// inventoryApprovalKind is the code-defined approval.Kind OnboardNode requires
// a proposal to have been approved under. It is never built from a request
// field or a proposal body. The "inventory:approve" literal is duplicated as
// OnboardingKind.ApproveScope in examples/inventory-onboarding and as
// inventoryKind in approval/approval_test.go; keep the three in step.
var inventoryApprovalKind = approval.Kind{Collection: proposalCollection, ApproveScope: "inventory:approve"}

// inventoryServer is the real in-memory Inventory facet implementation: an
// in-process node store keyed by node id, materialized on Discover and
// updated by PutFacts/AddNodeToGroup/OnboardNode in later plans. It is not
// permission-gated on its own — gatedInventory wraps it with the
// inventory:rw check every method requires (D-00).
type inventoryServer struct {
	hostv1.UnimplementedInventoryServer
	packID string
	mu     sync.Mutex
	nodes  map[string]*hostv1.Node
	// docs is the SAME documentsServer instance host.Host.Documents holds,
	// so a later plan's OnboardNode can read proposals written through
	// h.Documents (D-08). Unused by this plan's two RPCs.
	docs *documentsServer
	// candidates is Discover's fixture/seed source (D-05), injected at
	// construction time via local.WithDiscoverCandidates or the built-in
	// default. Discover never reaches outside this slice.
	candidates []*hostv1.Node
	// groups holds every group AddNodeToGroup has auto-created or
	// WithGroupClasses has pre-declared, keyed by group id. There is no
	// separate CreateGroup RPC — first use is the only creation moment.
	groups map[string]*hostv1.Group
	// members is the forward membership index: group id -> set of member
	// node ids. A set rather than a slice is what makes a repeated
	// AddNodeToGroup naturally idempotent.
	members map[string]map[string]bool
	// memberOf is the reverse membership index: node id -> set of group
	// ids it belongs to. Kept alongside members so ListNodeGroups never
	// has to scan every group's member set.
	memberOf map[string]map[string]bool
	// classes holds each group's assigned classes, keyed by group id then
	// class name — name-keying is what makes a duplicate class name
	// collapse to one entry (GRP-02). Folded in from newInventoryServer's
	// classes parameter at construction time.
	classes map[string]map[string]*hostv1.Class
}

func newInventoryServer(packID string, docs *documentsServer, candidates []*hostv1.Node, classes map[string][]*hostv1.Class) *inventoryServer {
	s := &inventoryServer{
		packID:     packID,
		nodes:      map[string]*hostv1.Node{},
		docs:       docs,
		candidates: candidates,
		groups:     map[string]*hostv1.Group{},
		members:    map[string]map[string]bool{},
		memberOf:   map[string]map[string]bool{},
		classes:    map[string]map[string]*hostv1.Class{},
	}
	for gid, cls := range classes {
		// A class assignment is itself evidence the group exists, so every
		// group id present in the injected class map is visible to
		// ListGroups even before any node joins it.
		if _, ok := s.groups[gid]; !ok {
			s.groups[gid] = &hostv1.Group{Id: gid, Name: gid}
		}
		inner := s.classes[gid]
		if inner == nil {
			inner = map[string]*hostv1.Class{}
			s.classes[gid] = inner
		}
		for _, c := range cls {
			inner[c.Name] = c // last-write-wins upsert: a duplicate name is the same class
		}
	}
	return s
}

// Discover materializes any not-yet-seen candidate into the node store as
// DISCOVERED and returns every candidate whose stored status is not yet
// ONBOARDED, sorted ascending by id. A node already materialized is left
// completely untouched — its display name, environment, facts and status
// survive a repeat Discover call unchanged (idempotency).
func (s *inventoryServer) Discover(ctx context.Context, _ *emptypb.Empty) (*hostv1.DiscoverResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range s.candidates {
		if _, exists := s.nodes[c.Id]; exists {
			continue
		}
		n := cloneNode(c)
		n.Status = hostv1.Node_DISCOVERED
		s.nodes[c.Id] = n
	}

	ids := make([]string, 0, len(s.candidates))
	for _, c := range s.candidates {
		n, ok := s.nodes[c.Id]
		if ok && n.Status != hostv1.Node_ONBOARDED {
			ids = append(ids, c.Id)
		}
	}
	sort.Strings(ids)

	resp := &hostv1.DiscoverResponse{Candidates: make([]*hostv1.Node, 0, len(ids))}
	for _, id := range ids {
		resp.Candidates = append(resp.Candidates, cloneNode(s.nodes[id]))
	}
	return resp, nil
}

// GetNode returns a node of any status (D-06) by exact byte-equal id match
// — no case folding, no Unicode normalization, no trimming.
func (s *inventoryServer) GetNode(ctx context.Context, req *hostv1.GetNodeRequest) (*hostv1.Node, error) {
	if req.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "node id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[req.Id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no node %q", req.Id)
	}
	return cloneNode(n), nil
}

// pageBounds is a deliberate copy of documentsServer.List's cursor-bounds
// logic (host/local/documents.go lines 100-137, specifically the guard at
// lines 110-124) — the one bug in this repo that was already found and
// fixed once, inline, and never extracted or tested. It is reproduced here
// verbatim rather than re-derived so the two copies cannot drift silently;
// a future fix to one must be checked against the other.
func pageBounds(page *hostv1.Page, total int) (start int, limit int, err error) {
	start = 0
	if page != nil && page.Cursor != "" {
		n, convErr := strconv.Atoi(page.Cursor)
		if convErr != nil || n < 0 {
			return 0, 0, status.Errorf(codes.InvalidArgument, "invalid page cursor %q", page.Cursor)
		}
		start = n
	}
	if start > total {
		start = total // an out-of-range (too large) cursor degrades to an empty page
	}
	limit = 500
	if page != nil && page.Limit > 0 && page.Limit < 500 {
		limit = int(page.Limit)
	}
	return start, limit, nil
}

// PutFacts upserts req.Facts into the node's stored fact map one key at a
// time (D-07) — the stored map is never rebound wholesale to the request's
// map, because a caller in the discover-then-group-then-onboard flow
// attaches facts incrementally and must not have to resend everything on
// each call. A nil or empty req.Facts is a no-op that still returns the
// node, not an error.
func (s *inventoryServer) PutFacts(ctx context.Context, req *hostv1.PutFactsRequest) (*hostv1.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[req.NodeId]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no node %q", req.NodeId)
	}
	if len(req.Facts) > 0 {
		if n.Facts == nil {
			n.Facts = map[string]*hostv1.Json{}
		}
		for k, v := range req.Facts {
			n.Facts[k] = v
		}
	}
	return cloneNode(n), nil
}

// ListNodes returns every node in the store regardless of Status (D-06) — a
// DISCOVERED candidate is a first-class record that can be grouped and
// fact-tagged before it is ever onboarded — ordered ascending and stably by
// id, paginated via pageBounds.
func (s *inventoryServer) ListNodes(ctx context.Context, req *hostv1.ListNodesRequest) (*hostv1.ListNodesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	start, limit, err := pageBounds(req.Page, len(ids))
	if err != nil {
		return nil, err
	}

	nodes := make([]*hostv1.Node, 0, limit)
	end := start
	for end < len(ids) && len(nodes) < limit {
		nodes = append(nodes, cloneNode(s.nodes[ids[end]]))
		end++
	}
	pageInfo := &hostv1.PageInfo{}
	if end < len(ids) {
		pageInfo.NextCursor = strconv.Itoa(end)
	}
	return &hostv1.ListNodesResponse{Nodes: nodes, Page: pageInfo}, nil
}

// unwrapJSON mirrors the single-field "v" wrapper convention
// documentsServer.Query already relies on (documents.go:151-154), applied
// here symmetrically: a scalar fact value ("os": "linux") matches
// field: "os" directly, while an object-valued fact ("cpu": {"cores": 8})
// still supports a dotted field like "cpu.cores" via factsAsMap/fieldAt.
//
// Byte-carrying obligation: facts in this phase are flat key-value strings
// and numbers only (INV-02). If a later phase needs a fact to carry raw
// bytes — a fingerprint or a checksum — it must base64-encode before
// wrapping in Json and decode symmetrically on read, because Json wraps a
// JSON Struct that cannot hold invalid UTF-8; host.Local never serializes,
// so it would tolerate the corruption silently while a real backend would
// reject it (CR-02 class).
func unwrapJSON(j *hostv1.Json) any {
	if j == nil || j.Value == nil {
		return nil
	}
	m := j.Value.AsMap()
	if v, ok := m["v"]; ok && len(m) == 1 {
		return v
	}
	return m
}

// factsAsMap builds the map fieldAt (documents.go) walks, from a node's
// stored Facts.
func factsAsMap(n *hostv1.Node) map[string]any {
	out := make(map[string]any, len(n.Facts))
	for k, v := range n.Facts {
		out[k] = unwrapJSON(v)
	}
	return out
}

// docOp bridges QueryNodesRequest_Op to QueryDocumentsRequest_Op by direct
// numeric conversion. Both enums declare OP_UNSPECIFIED = 0 through
// CONTAINS = 7 in the same order;
// TestInventory_QueryNodesOpParityWithDocuments pins that numeric parity so
// a future proto edit that diverges the two enums fails loudly instead of
// silently mis-comparing.
func docOp(op hostv1.QueryNodesRequest_Op) hostv1.QueryDocumentsRequest_Op {
	return hostv1.QueryDocumentsRequest_Op(op)
}

// QueryNodes filters the node store by a single fact using the same
// field/op/value grammar documentsServer.Query defines, calling its
// matchOp/fieldAt directly rather than reimplementing them — a second
// matcher would be a second place for comparison semantics to drift.
//
// Unlike documentsServer.Query, which returns its whole matched set with an
// empty PageInfo, QueryNodes paginates the matched set with pageBounds —
// QueryNodesRequest declares a page field, and leaving it unread would be a
// silent, undetectable hole for a caller. This divergence from Documents is
// deliberate, not an inconsistency.
func (s *inventoryServer) QueryNodes(ctx context.Context, req *hostv1.QueryNodesRequest) (*hostv1.ListNodesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := unwrapJSON(req.Value)

	ids := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	matched := make([]*hostv1.Node, 0)
	for _, id := range ids {
		n := s.nodes[id]
		got := fieldAt(factsAsMap(n), req.Field)
		if matchOp(docOp(req.Op), got, want) {
			matched = append(matched, n)
		}
	}

	start, limit, err := pageBounds(req.Page, len(matched))
	if err != nil {
		return nil, err
	}
	nodes := make([]*hostv1.Node, 0, limit)
	end := start
	for end < len(matched) && len(nodes) < limit {
		nodes = append(nodes, cloneNode(matched[end]))
		end++
	}
	pageInfo := &hostv1.PageInfo{}
	if end < len(matched) {
		pageInfo.NextCursor = strconv.Itoa(end)
	}
	return &hostv1.ListNodesResponse{Nodes: nodes, Page: pageInfo}, nil
}

// AddNodeToGroup adds an existing node's id to the named group's membership
// set, auto-creating the group on first use — there is no separate
// CreateGroup RPC in v1. It works on a node of any Status, including
// DISCOVERED (D-06): the milestone's flow groups a node before it is ever
// onboarded. A repeated call with the same pair is a no-op in both
// directions — the underlying sets make a duplicate insert naturally
// idempotent — and an already-existing group's Name is left untouched.
func (s *inventoryServer) AddNodeToGroup(ctx context.Context, req *hostv1.GroupMembershipRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.nodes[req.NodeId]; !ok {
		// A membership naming a node that does not exist would be
		// unreachable state, and this is a write path, so refuse it.
		return nil, status.Errorf(codes.NotFound, "no node %q", req.NodeId)
	}
	if req.GroupId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "group id is required")
	}

	if _, ok := s.groups[req.GroupId]; !ok {
		s.groups[req.GroupId] = &hostv1.Group{Id: req.GroupId, Name: req.GroupId}
	}

	if s.members[req.GroupId] == nil {
		s.members[req.GroupId] = map[string]bool{}
	}
	s.members[req.GroupId][req.NodeId] = true

	if s.memberOf[req.NodeId] == nil {
		s.memberOf[req.NodeId] = map[string]bool{}
	}
	s.memberOf[req.NodeId][req.GroupId] = true

	return &emptypb.Empty{}, nil
}

// ListGroups returns every known group, ascending and stably by id. An
// empty store yields an empty list and a nil error.
func (s *inventoryServer) ListGroups(ctx context.Context, _ *emptypb.Empty) (*hostv1.GroupList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.groups))
	for id := range s.groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	groups := make([]*hostv1.Group, 0, len(ids))
	for _, id := range ids {
		groups = append(groups, cloneGroup(s.groups[id]))
	}
	return &hostv1.GroupList{Groups: groups}, nil
}

// ListGroupNodes returns the nodes belonging to a group, ascending and
// stably by node id, paginated with the same pageBounds rules ListNodes
// uses. A group id that was never created is read with a plain map index —
// mirroring how documentsServer.List treats an unknown collection
// (documents.go:103) — so it yields an empty result rather than an error. A
// member id no longer present in s.nodes is skipped, so a stale edge can
// never surface as a phantom node.
func (s *inventoryServer) ListGroupNodes(ctx context.Context, req *hostv1.ListGroupNodesRequest) (*hostv1.ListNodesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	members := s.members[req.GroupId]
	ids := make([]string, 0, len(members))
	for id := range members {
		if _, ok := s.nodes[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	start, limit, err := pageBounds(req.Page, len(ids))
	if err != nil {
		return nil, err
	}

	nodes := make([]*hostv1.Node, 0, limit)
	end := start
	for end < len(ids) && len(nodes) < limit {
		nodes = append(nodes, cloneNode(s.nodes[ids[end]]))
		end++
	}
	pageInfo := &hostv1.PageInfo{}
	if end < len(ids) {
		pageInfo.NextCursor = strconv.Itoa(end)
	}
	return &hostv1.ListNodesResponse{Nodes: nodes, Page: pageInfo}, nil
}

// ListNodeGroups returns the groups a node belongs to, ascending and stably
// by group id. An unknown or ungrouped node id is read with a plain map
// index, symmetric with ListGroupNodes, so it yields an empty list and a
// nil error rather than an error.
//
// ListNodeGroupsRequest carries no page field and GroupList carries no
// PageInfo, so this response is deliberately unpaginated.
func (s *inventoryServer) ListNodeGroups(ctx context.Context, req *hostv1.ListNodeGroupsRequest) (*hostv1.GroupList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	memberOf := s.memberOf[req.NodeId]
	ids := make([]string, 0, len(memberOf))
	for id := range memberOf {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	groups := make([]*hostv1.Group, 0, len(ids))
	for _, id := range ids {
		if g, ok := s.groups[id]; ok {
			groups = append(groups, cloneGroup(g))
		}
	}
	return &hostv1.GroupList{Groups: groups}, nil
}

// cloneNode returns a deep copy so a caller mutating the returned node can
// never reach into the facet's internal state.
func cloneNode(n *hostv1.Node) *hostv1.Node {
	return proto.Clone(n).(*hostv1.Node)
}

// cloneGroup returns a deep copy so a caller mutating a returned group can
// never reach into the facet's internal state. Every group returned by any
// Inventory RPC goes through it.
func cloneGroup(g *hostv1.Group) *hostv1.Group {
	return proto.Clone(g).(*hostv1.Group)
}

// ListClasses returns the classes assigned to a group, ascending and
// stably by name. This is read-only in v1: no rule evaluation, no
// ancestor-group walk and no merging across a group hierarchy occurs here
// — the contract comment says so and GRP-03 (a rule engine) is a v2
// requirement. A group id that was never created, and a group that exists
// but carries no classes, both read via a plain map index — symmetric with
// ListGroupNodes — and so both yield an empty list and a nil error rather
// than an error.
func (s *inventoryServer) ListClasses(ctx context.Context, req *hostv1.GroupRef) (*hostv1.ClassList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inner := s.classes[req.Id]
	names := make([]string, 0, len(inner))
	for name := range inner {
		names = append(names, name)
	}
	sort.Strings(names)

	classes := make([]*hostv1.Class, 0, len(names))
	for _, name := range names {
		classes = append(classes, cloneClass(inner[name]))
	}
	return &hostv1.ClassList{Classes: classes}, nil
}

// cloneClass returns a deep copy so a caller mutating a returned class —
// notably its Parameters, a Json a caller could otherwise rewrite in place
// — can never reach into the facet's internal state (T-03-14).
func cloneClass(c *hostv1.Class) *hostv1.Class {
	return proto.Clone(c).(*hostv1.Class)
}

// cloneGroupClassesMap deep-copies every class in a group-id-to-classes
// map, giving each host.Local instance its own private copy of
// defaultGroupClasses rather than sharing pointers across instances —
// mirrors cloneNodeSlice.
func cloneGroupClassesMap(m map[string][]*hostv1.Class) map[string][]*hostv1.Class {
	out := make(map[string][]*hostv1.Class, len(m))
	for gid, classes := range m {
		cloned := make([]*hostv1.Class, len(classes))
		for i, c := range classes {
			cloned[i] = cloneClass(c)
		}
		out[gid] = cloned
	}
	return out
}

// cloneNodeSlice deep-copies every node in a slice, used to give each
// host.Local instance its own private copy of the built-in Discover
// fixture set (defaultDiscoverCandidates) rather than sharing pointers
// across instances.
func cloneNodeSlice(nodes []*hostv1.Node) []*hostv1.Node {
	out := make([]*hostv1.Node, len(nodes))
	for i, n := range nodes {
		out[i] = cloneNode(n)
	}
	return out
}

// jsonScalar wraps a materialized fact value in the same single-field "v"
// convention unwrapJSON reads back (documents.go's Query and this file's
// QueryNodes both rely on it) — a fact that arrives through an onboarding
// proposal and a fact that arrives through PutFacts are indistinguishable
// afterwards. An object-valued fact is instead wrapped as the struct built
// directly from the map, matching how PutFacts callers already build
// object-valued Json (see unwrapJSON's own doc comment on the "v"-vs-object
// distinction).
func jsonScalar(v any) *hostv1.Json {
	if m, ok := v.(map[string]any); ok {
		s, err := structpb.NewStruct(m)
		if err != nil {
			return nil
		}
		return &hostv1.Json{Value: s}
	}
	s, err := structpb.NewStruct(map[string]any{"v": v})
	if err != nil {
		return nil
	}
	return &hostv1.Json{Value: s}
}

// nodeFromProposal builds the Node payload OnboardNode materializes from a
// D-08 proposal document body's "node" object. proposalID is always the
// authoritative Id — D-08 makes the doc id, the proposal id and the node id
// one value, so nodeFromProposal requires node.id, when present, to agree
// with proposalID and refuses to materialize a disagreement rather than
// guessing which one is right.
func nodeFromProposal(m map[string]any, proposalID string) (*hostv1.Node, error) {
	nodeRaw, ok := m["node"]
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "proposal %q has no node object", proposalID)
	}
	nodeMap, ok := nodeRaw.(map[string]any)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "proposal %q node is not an object", proposalID)
	}
	if idRaw, ok := nodeMap["id"]; ok {
		idStr, isStr := idRaw.(string)
		if !isStr || idStr != proposalID {
			return nil, status.Errorf(codes.FailedPrecondition, "proposal %q node.id %v disagrees with the proposal id", proposalID, idRaw)
		}
	}

	displayName, _ := nodeMap["display_name"].(string)
	environment, _ := nodeMap["environment"].(string)

	n := &hostv1.Node{
		Id:          proposalID,
		DisplayName: displayName,
		Environment: environment,
		Status:      hostv1.Node_ONBOARDED,
	}

	if factsRaw, ok := nodeMap["facts"]; ok {
		if factsMap, ok := factsRaw.(map[string]any); ok {
			n.Facts = make(map[string]*hostv1.Json, len(factsMap))
			for k, v := range factsMap {
				n.Facts[k] = jsonScalar(v)
			}
		}
	}

	return n, nil
}

// OnboardNode is the one Inventory RPC that reaches outside the facet: it
// resolves req.ProposalId through the shared Documents store (D-08, the
// SAME documentsServer instance host.Host.Documents holds) and refuses to
// materialize anything unless the referenced document records an approval:
// approval.RequireApproved(doc, inventoryApprovalKind) requires the status to
// read "approved", approved_scope to equal inventoryApprovalKind.ApproveScope
// and decided_by to be non-empty. A status string alone is not enough, because
// the Documents store has no ACL and anything holding the host can write one;
// the scope and the decider are the provenance only approval.Approve records,
// so only forged or hand-written records are refused that it would accept
// before. This is the only additional check OnboardNode carries beyond the
// facet-level inventory:rw gate (D-01) — it reads a decision someone else
// recorded. It never consults a token, never evaluates a scope against one,
// and never writes back into proposalCollection; the approval decision and
// the act of onboarding stay in separate code, which is what keeps this gate
// from being able to defeat itself (T-03-20).
func (s *inventoryServer) OnboardNode(ctx context.Context, req *hostv1.OnboardNodeRequest) (*hostv1.Node, error) {
	// Idempotency short-circuit (D-02): a repeat onboard of an
	// already-ONBOARDED node is a success, not an error, and it must not
	// re-read the proposal at all — this is what keeps a repeat call cheap
	// and keeps it working after the proposal document is gone.
	s.mu.Lock()
	if n, ok := s.nodes[req.ProposalId]; ok && n.Status == hostv1.Node_ONBOARDED {
		result := cloneNode(n)
		s.mu.Unlock()
		return result, nil
	}
	s.mu.Unlock()

	// Resolve the proposal on the shared Documents store — not a new
	// store, and not through host.Host.Documents, which this file has no
	// reference to. The lock is deliberately NOT held across this call.
	doc, err := s.docs.Get(ctx, &hostv1.GetDocumentRequest{Collection: proposalCollection, DocId: req.ProposalId})
	if err != nil {
		return nil, err // the Documents store's own NotFound, unchanged
	}
	if doc.Body == nil || doc.Body.Value == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "proposal %q has no body", req.ProposalId)
	}
	if err := approval.RequireApproved(doc, inventoryApprovalKind); err != nil {
		return nil, err
	}
	m := doc.Body.Value.AsMap()

	proposed, err := nodeFromProposal(m, req.ProposalId)
	if err != nil {
		return nil, err
	}

	// Materialize. Re-take the lock and re-check the ONBOARDED
	// short-circuit before writing — between releasing the lock above and
	// re-taking it here, another goroutine may have completed the whole
	// sequence, and without this second check both would write and
	// "exactly once" would hold only by luck (T-03-22).
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.nodes[req.ProposalId]; ok {
		if existing.Status == hostv1.Node_ONBOARDED {
			return cloneNode(existing), nil
		}
		// Update in place: preserve facts and group memberships attached
		// while the node was DISCOVERED (D-06/D-07). Group memberships
		// need no handling here — they key on node id in a separate
		// index and are untouched by this path.
		existing.Status = hostv1.Node_ONBOARDED
		if proposed.DisplayName != "" {
			existing.DisplayName = proposed.DisplayName
		}
		if proposed.Environment != "" {
			existing.Environment = proposed.Environment
		}
		if len(proposed.Facts) > 0 {
			if existing.Facts == nil {
				existing.Facts = map[string]*hostv1.Json{}
			}
			for k, v := range proposed.Facts {
				existing.Facts[k] = v
			}
		}
		return cloneNode(existing), nil
	}

	s.nodes[req.ProposalId] = proposed
	return cloneNode(proposed), nil
}

// gatedInventory wraps inventoryServer with a permission check on every
// one of the 11 Inventory RPCs. D-00's original design required
// inventory:rw uniformly; as of the stagehand-planner Phase B parity fix
// (D-08/D-19), the 8 read-only RPCs also accept the dormant inventory:read
// literal (see checkRead below), matching the console-side admission table.
// The 3 mutating RPCs (PutFacts, AddNodeToGroup, OnboardNode) still require
// inventory:rw only. There is no separate, narrower check for OnboardNode
// beyond that; the onboarding decision's approval authority is a different
// namespace entirely and is Phase 4's concern.
type gatedInventory struct {
	hostv1.UnimplementedInventoryServer
	perms  map[string]bool
	packID string
	inner  *inventoryServer
}

func (g *gatedInventory) check() error {
	if !g.perms["inventory:rw"] {
		return ErrPermissionDenied("inventory:rw")
	}
	return nil
}

// checkRead gates the 8 read-only Inventory RPCs (GetNode, Discover,
// ListNodes, QueryNodes, ListGroups, ListGroupNodes, ListNodeGroups,
// ListClasses): either inventory:read or inventory:rw satisfies it. This is
// a parity fix against the console-side admission table
// (stagehand-console backend/internal/expansions/interceptor.go's
// FacetPermissions), which has long admitted these same RPCs under either
// permission; inventory:read already exists in manifest.Permissions and
// manifest/validate.go rePerm (inherited, previously unused by this
// facet) — not a new permission or facet. Authorized by stagehand-planner
// work order B (D-08/D-19, parity fix, no-reopen). The 3 mutating RPCs
// (PutFacts, AddNodeToGroup, OnboardNode) still require inventory:rw via
// check() above.
func (g *gatedInventory) checkRead() error {
	if g.perms["inventory:read"] || g.perms["inventory:rw"] {
		return nil
	}
	return ErrPermissionDenied("inventory:read or inventory:rw")
}

func (g *gatedInventory) GetNode(ctx context.Context, req *hostv1.GetNodeRequest) (*hostv1.Node, error) {
	if err := g.checkRead(); err != nil {
		return nil, err
	}
	return g.inner.GetNode(ctx, req)
}

func (g *gatedInventory) Discover(ctx context.Context, req *emptypb.Empty) (*hostv1.DiscoverResponse, error) {
	if err := g.checkRead(); err != nil {
		return nil, err
	}
	return g.inner.Discover(ctx, req)
}

func (g *gatedInventory) ListNodes(ctx context.Context, req *hostv1.ListNodesRequest) (*hostv1.ListNodesResponse, error) {
	if err := g.checkRead(); err != nil {
		return nil, err
	}
	return g.inner.ListNodes(ctx, req)
}

func (g *gatedInventory) QueryNodes(ctx context.Context, req *hostv1.QueryNodesRequest) (*hostv1.ListNodesResponse, error) {
	if err := g.checkRead(); err != nil {
		return nil, err
	}
	return g.inner.QueryNodes(ctx, req)
}

func (g *gatedInventory) PutFacts(ctx context.Context, req *hostv1.PutFactsRequest) (*hostv1.Node, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.PutFacts(ctx, req)
}

func (g *gatedInventory) ListGroups(ctx context.Context, req *emptypb.Empty) (*hostv1.GroupList, error) {
	if err := g.checkRead(); err != nil {
		return nil, err
	}
	return g.inner.ListGroups(ctx, req)
}

func (g *gatedInventory) ListGroupNodes(ctx context.Context, req *hostv1.ListGroupNodesRequest) (*hostv1.ListNodesResponse, error) {
	if err := g.checkRead(); err != nil {
		return nil, err
	}
	return g.inner.ListGroupNodes(ctx, req)
}

func (g *gatedInventory) ListNodeGroups(ctx context.Context, req *hostv1.ListNodeGroupsRequest) (*hostv1.GroupList, error) {
	if err := g.checkRead(); err != nil {
		return nil, err
	}
	return g.inner.ListNodeGroups(ctx, req)
}

func (g *gatedInventory) AddNodeToGroup(ctx context.Context, req *hostv1.GroupMembershipRequest) (*emptypb.Empty, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.AddNodeToGroup(ctx, req)
}

func (g *gatedInventory) ListClasses(ctx context.Context, req *hostv1.GroupRef) (*hostv1.ClassList, error) {
	if err := g.checkRead(); err != nil {
		return nil, err
	}
	return g.inner.ListClasses(ctx, req)
}

func (g *gatedInventory) OnboardNode(ctx context.Context, req *hostv1.OnboardNodeRequest) (*hostv1.Node, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.OnboardNode(ctx, req)
}
