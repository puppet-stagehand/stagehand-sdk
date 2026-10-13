package local

import (
	"context"
	"sort"
	"strconv"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// instancesPermission is the one manifest permission the Instances facet
// requires, for both RPCs.
const instancesPermission = "instances:read"

// WithInstances seeds host.Local's Instances facet with the estates a pack's
// tests need. host.Local stores private copies, so mutating an argument after
// New returns changes nothing. A later entry with the same id replaces an
// earlier one; a nil entry is skipped. Without this option the facet lists no
// estates.
//
// The seeded messages are whatever the test builds: the contract itself has
// no field that can carry a credential (see host.proto, service Instances),
// so a seed cannot smuggle one either.
func WithInstances(instances ...*hostv1.Instance) Option {
	return func(c *config) {
		for _, in := range instances {
			if in != nil {
				c.instances = append(c.instances, proto.Clone(in).(*hostv1.Instance))
			}
		}
	}
}

// RemoveInstance drops the estate with the given id from a host built by New,
// as the console does when an operator removes an estate. It returns false if
// h was not built by New or holds no such estate. It exists so a pack's tests
// can prove the pack copes with GetInstance answering NOT_FOUND for an id it
// stored earlier (D-19); the real console exposes no such RPC to packs.
func RemoveInstance(h *host.Host, id string) bool {
	if h == nil {
		return false
	}
	g, ok := h.Instances.(*gatedInstances)
	if !ok || g == nil || g.inner == nil {
		return false
	}
	return g.inner.remove(id)
}

// instancesServer is host.Local's in-memory Instances facet: the seeded
// estates keyed by id. It is not permission-gated on its own; gatedInstances
// wraps it with the instances:read check.
type instancesServer struct {
	hostv1.UnimplementedInstancesServer
	mu   sync.Mutex
	byID map[string]*hostv1.Instance
}

func newInstancesServer(seed []*hostv1.Instance) *instancesServer {
	s := &instancesServer{byID: make(map[string]*hostv1.Instance, len(seed))}
	for _, in := range seed {
		s.byID[in.GetId()] = in // already private clones (WithInstances)
	}
	return s
}

func (s *instancesServer) remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return false
	}
	delete(s.byID, id)
	return true
}

// ListInstances returns every estate ordered ascending by id, paginated with
// the same cursor rules as Inventory.ListNodes (pageBounds).
func (s *instancesServer) ListInstances(_ context.Context, req *hostv1.ListInstancesRequest) (*hostv1.ListInstancesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	start, limit, err := pageBounds(req.GetPage(), len(ids))
	if err != nil {
		return nil, err
	}
	out := make([]*hostv1.Instance, 0, min(limit, len(ids)-start))
	end := start
	for end < len(ids) && len(out) < limit {
		out = append(out, proto.Clone(s.byID[ids[end]]).(*hostv1.Instance))
		end++
	}
	page := &hostv1.PageInfo{}
	if end < len(ids) {
		page.NextCursor = strconv.Itoa(end)
	}
	return &hostv1.ListInstancesResponse{Instances: out, Page: page}, nil
}

// GetInstance returns a copy of one estate by exact id, or NOT_FOUND for an id
// that is not (or is no longer) registered.
func (s *instancesServer) GetInstance(_ context.Context, req *hostv1.GetInstanceRequest) (*hostv1.Instance, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "instance id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.byID[req.GetId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no instance %q", req.GetId())
	}
	return proto.Clone(in).(*hostv1.Instance), nil
}

// gatedInstances refuses both Instances RPCs unless the manifest declares
// instances:read, with the same PERMISSION_DENIED the console returns.
type gatedInstances struct {
	hostv1.UnimplementedInstancesServer
	perms map[string]bool
	inner *instancesServer
}

func (g *gatedInstances) check() error {
	if !g.perms[instancesPermission] {
		return ErrPermissionDenied(instancesPermission)
	}
	return nil
}

func (g *gatedInstances) ListInstances(ctx context.Context, req *hostv1.ListInstancesRequest) (*hostv1.ListInstancesResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ListInstances(ctx, req)
}

func (g *gatedInstances) GetInstance(ctx context.Context, req *hostv1.GetInstanceRequest) (*hostv1.Instance, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.GetInstance(ctx, req)
}
