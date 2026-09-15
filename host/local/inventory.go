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

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

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
}

func newInventoryServer(packID string, docs *documentsServer, candidates []*hostv1.Node) *inventoryServer {
	return &inventoryServer{
		packID:     packID,
		nodes:      map[string]*hostv1.Node{},
		docs:       docs,
		candidates: candidates,
	}
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

// cloneNode returns a deep copy so a caller mutating the returned node can
// never reach into the facet's internal state.
func cloneNode(n *hostv1.Node) *hostv1.Node {
	return proto.Clone(n).(*hostv1.Node)
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

// gatedInventory wraps inventoryServer with the inventory:rw permission
// check every one of the 11 Inventory RPCs requires, including the
// read-only ones (D-00) — there is no separate, narrower check for
// OnboardNode here; the onboarding decision's approval authority is a
// different namespace entirely and is Phase 4's concern.
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

func (g *gatedInventory) GetNode(ctx context.Context, req *hostv1.GetNodeRequest) (*hostv1.Node, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.GetNode(ctx, req)
}

func (g *gatedInventory) Discover(ctx context.Context, req *emptypb.Empty) (*hostv1.DiscoverResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.Discover(ctx, req)
}

func (g *gatedInventory) ListNodes(ctx context.Context, req *hostv1.ListNodesRequest) (*hostv1.ListNodesResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ListNodes(ctx, req)
}

func (g *gatedInventory) QueryNodes(ctx context.Context, req *hostv1.QueryNodesRequest) (*hostv1.ListNodesResponse, error) {
	if err := g.check(); err != nil {
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
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ListGroups(ctx, req)
}

func (g *gatedInventory) ListGroupNodes(ctx context.Context, req *hostv1.ListGroupNodesRequest) (*hostv1.ListNodesResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ListGroupNodes(ctx, req)
}

func (g *gatedInventory) ListNodeGroups(ctx context.Context, req *hostv1.ListNodeGroupsRequest) (*hostv1.GroupList, error) {
	if err := g.check(); err != nil {
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
	if err := g.check(); err != nil {
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
