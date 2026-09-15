package local

import (
	"context"
	"sort"
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
