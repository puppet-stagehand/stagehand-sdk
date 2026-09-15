package local_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

// jsonWrap wraps v in the single-field "v" convention documentsServer.Query
// and inventoryServer.QueryNodes both rely on for scalar comparison values
// and, via PutFacts, for stored fact values.
func jsonWrap(t *testing.T, v any) *hostv1.Json {
	t.Helper()
	s, err := structpb.NewStruct(map[string]any{"v": v})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return &hostv1.Json{Value: s}
}

// jsonFacts builds a PutFactsRequest.Facts map from plain Go values, each
// wrapped via jsonWrap.
func jsonFacts(t *testing.T, facts map[string]any) map[string]*hostv1.Json {
	t.Helper()
	out := make(map[string]*hostv1.Json, len(facts))
	for k, v := range facts {
		out[k] = jsonWrap(t, v)
	}
	return out
}

// factValue unwraps a node's stored fact back to a plain Go value for
// assertions.
func factValue(t *testing.T, n *hostv1.Node, key string) any {
	t.Helper()
	j, ok := n.Facts[key]
	if !ok {
		t.Fatalf("node %q missing fact %q", n.Id, key)
	}
	if j == nil || j.Value == nil {
		return nil
	}
	return j.Value.AsMap()["v"]
}

func TestInventory_DiscoverThenGetNode(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()

	resp, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(resp.Candidates))
	}
	if resp.Candidates[0].Id != "db-01.example.test" || resp.Candidates[1].Id != "web-01.example.test" {
		t.Fatalf("expected ascending id order db-01,web-01, got %q,%q", resp.Candidates[0].Id, resp.Candidates[1].Id)
	}

	n, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "db-01.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if n.Id != "db-01.example.test" || n.DisplayName != "db-01" || n.Status != hostv1.Node_DISCOVERED || n.Environment != "staging" {
		t.Fatalf("identity fields did not round-trip: %+v", n)
	}
}

func TestInventory_DiscoverIsIdempotent(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()

	first, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Candidates) != len(second.Candidates) {
		t.Fatalf("expected equal length, got %d vs %d", len(first.Candidates), len(second.Candidates))
	}
	for i := range first.Candidates {
		if first.Candidates[i].Id != second.Candidates[i].Id {
			t.Fatalf("id mismatch at %d: %q vs %q", i, first.Candidates[i].Id, second.Candidates[i].Id)
		}
	}
	for _, c := range first.Candidates {
		if _, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: c.Id}); err != nil {
			t.Fatalf("GetNode(%q): %v", c.Id, err)
		}
	}
}

func TestInventory_GetNodeRejectsEmptyAndUnknownID(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()

	if _, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: ""}); err == nil {
		t.Fatal("expected error for empty id")
	} else if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", st.Code())
	}

	if _, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "nope"}); err == nil {
		t.Fatal("expected error for unknown id")
	} else if st, _ := status.FromError(err); st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", st.Code())
	}
}

func TestInventory_NodeIDIsByteExact(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()

	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "WEB-01.EXAMPLE.TEST"}); err == nil {
		t.Fatal("expected NotFound for uppercased id")
	} else if st, _ := status.FromError(err); st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", st.Code())
	}
}

func TestInventory_DiscoverCandidatesAreInjectable(t *testing.T) {
	ctx := context.Background()

	custom := &hostv1.Node{Id: "custom-01.example.test", DisplayName: "custom-01", Status: hostv1.Node_DISCOVERED, Environment: "dev"}
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(custom))
	resp, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].Id != "custom-01.example.test" {
		t.Fatalf("expected exactly the injected custom node, got %+v", resp.Candidates)
	}

	hEmpty := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates())
	respEmpty, err := hEmpty.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(respEmpty.Candidates) != 0 {
		t.Fatalf("expected empty candidate list for explicit empty option, got %+v", respEmpty.Candidates)
	}
}

func TestInventory_DiscoverIsRaceFree(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
				t.Error(err)
			}
			if _, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "db-01.example.test"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	resp, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 2 {
		t.Fatalf("expected stable final count of 2, got %d", len(resp.Candidates))
	}
}

// inventoryCalls returns one minimally-valid closure per Inventory RPC,
// used by TestInventory_DeniedWithoutPermission to exercise all 11 methods
// uniformly regardless of which ones are implemented yet.
func inventoryCalls(ctx context.Context, h *host.Host) map[string]func() error {
	return map[string]func() error{
		"GetNode":        func() error { _, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "x"}); return err },
		"Discover":       func() error { _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); return err },
		"ListNodes":      func() error { _, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{}); return err },
		"QueryNodes":     func() error { _, err := h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{}); return err },
		"PutFacts":       func() error { _, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "x"}); return err },
		"ListGroups":     func() error { _, err := h.Inventory.ListGroups(ctx, &emptypb.Empty{}); return err },
		"ListGroupNodes": func() error { _, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "x"}); return err },
		"ListNodeGroups": func() error { _, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "x"}); return err },
		"AddNodeToGroup": func() error {
			_, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "x", GroupId: "y"})
			return err
		},
		"ListClasses": func() error { _, err := h.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: "x"}); return err },
		"OnboardNode": func() error { _, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: "x"}); return err },
	}
}

func assertInventoryAllDenied(t *testing.T, ctx context.Context, h *host.Host, label string) {
	t.Helper()
	for name, call := range inventoryCalls(ctx, h) {
		err := call()
		if err == nil {
			t.Fatalf("%s/%s: expected error, got nil", label, name)
		}
		st, _ := status.FromError(err)
		if st.Code() != codes.PermissionDenied {
			t.Fatalf("%s/%s: expected PermissionDenied, got %v (%v)", label, name, st.Code(), err)
		}
		found := false
		for _, d := range st.Details() {
			ed, ok := d.(*hostv1.ErrorDetail)
			if !ok {
				continue
			}
			found = true
			if ed.Code != "facet_not_declared" {
				t.Fatalf("%s/%s: expected ErrorDetail.Code facet_not_declared, got %q", label, name, ed.Code)
			}
			if !strings.Contains(ed.Message, "inventory:rw") {
				t.Fatalf("%s/%s: expected ErrorDetail.Message to mention inventory:rw, got %q", label, name, ed.Message)
			}
		}
		if !found {
			t.Fatalf("%s/%s: expected an *hostv1.ErrorDetail in status details", label, name)
		}
	}
}

func TestInventory_DeniedWithoutPermission(t *testing.T) {
	ctx := context.Background()

	assertInventoryAllDenied(t, ctx, local.New(nil, "opentofu"), "no-perms")
	assertInventoryAllDenied(t, ctx, local.New([]string{"secrets:rw"}, "opentofu"), "secrets-only")
}

func TestInventory_ReadPathClonesNodes(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()

	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	n, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "db-01.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	n.DisplayName = "mutated"
	if n.Facts == nil {
		n.Facts = map[string]*hostv1.Json{}
	}
	n.Facts["injected"] = &hostv1.Json{}

	again, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "db-01.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if again.DisplayName != "db-01" {
		t.Fatalf("expected display name unaffected by caller mutation, got %q", again.DisplayName)
	}
	if _, ok := again.Facts["injected"]; ok {
		t.Fatal("expected injected fact key to be absent from the facet's stored record")
	}
}

func TestInventory_PutFactsMerges(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(
		&hostv1.Node{Id: "n1", DisplayName: "n1", Status: hostv1.Node_DISCOVERED},
	))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	n, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "n1", Facts: jsonFacts(t, map[string]any{"os": "linux"})})
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Facts) != 1 {
		t.Fatalf("expected 1 fact after first call, got %d", len(n.Facts))
	}

	n, err = h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "n1", Facts: jsonFacts(t, map[string]any{"env": "prod"})})
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Facts) != 2 {
		t.Fatalf("expected 2 facts after merge, got %d: %+v", len(n.Facts), n.Facts)
	}

	n, err = h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "n1", Facts: jsonFacts(t, map[string]any{"os": "windows"})})
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Facts) != 2 {
		t.Fatalf("expected still 2 facts after overwrite of one key, got %d", len(n.Facts))
	}
	if v := factValue(t, n, "os"); v != "windows" {
		t.Fatalf("expected os=windows after overwrite, got %v", v)
	}
	if v := factValue(t, n, "env"); v != "prod" {
		t.Fatalf("expected env=prod preserved, got %v", v)
	}

	unchanged, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "n1", Facts: nil})
	if err != nil {
		t.Fatalf("expected nil facts map to be a no-op with no error, got %v", err)
	}
	if len(unchanged.Facts) != 2 {
		t.Fatalf("expected nil facts map to leave the node unchanged, got %d facts", len(unchanged.Facts))
	}

	got, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 2 {
		t.Fatalf("expected GetNode to see the same 2 facts written via PutFacts, got %d", len(got.Facts))
	}
}

func TestInventory_PutFactsUnknownNode(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()

	_, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "nope", Facts: jsonFacts(t, map[string]any{"os": "linux"})})
	if err == nil {
		t.Fatal("expected error for unknown node id")
	}
	if st, _ := status.FromError(err); st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", st.Code())
	}
}

func TestInventory_ListNodesPagination(t *testing.T) {
	nodes := []*hostv1.Node{
		{Id: "c1", DisplayName: "c1", Status: hostv1.Node_DISCOVERED},
		{Id: "a1", DisplayName: "a1", Status: hostv1.Node_DISCOVERED},
		{Id: "b1", DisplayName: "b1", Status: hostv1.Node_DISCOVERED},
	}
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(nodes...))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	first, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{Page: &hostv1.Page{Limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Nodes) != 1 || first.Page.NextCursor != "1" {
		t.Fatalf("expected 1 node and NextCursor \"1\", got %d nodes, cursor %q", len(first.Nodes), first.Page.NextCursor)
	}

	seen := map[string]bool{first.Nodes[0].Id: true}
	cursor := first.Page.NextCursor
	for i := 0; i < 10 && cursor != ""; i++ {
		resp, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{Page: &hostv1.Page{Cursor: cursor, Limit: 1}})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Nodes) != 1 {
			t.Fatalf("expected 1 node per page, got %d", len(resp.Nodes))
		}
		if seen[resp.Nodes[0].Id] {
			t.Fatalf("node %q seen twice while walking pages", resp.Nodes[0].Id)
		}
		seen[resp.Nodes[0].Id] = true
		cursor = resp.Page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("expected to see all 3 nodes exactly once, saw %d: %+v", len(seen), seen)
	}
}

func TestInventory_ListNodesRejectsBadCursor(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(
		&hostv1.Node{Id: "n1", DisplayName: "n1", Status: hostv1.Node_DISCOVERED},
	))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	badCursors := []struct {
		name   string
		cursor string
	}{
		{"negative", "-1"},
		{"non-numeric", "not-a-number"},
	}
	for _, tc := range badCursors {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{Page: &hostv1.Page{Cursor: tc.cursor}})
			if err == nil {
				t.Fatal("expected error")
			}
			if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", st.Code())
			}
		})
	}

	huge, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{Page: &hostv1.Page{Cursor: "", Limit: 10000}})
	if err != nil {
		t.Fatalf("expected no error for empty cursor with an oversized limit, got %v", err)
	}
	if len(huge.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(huge.Nodes))
	}

	oversized, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{Page: &hostv1.Page{Cursor: "9999"}})
	if err != nil {
		t.Fatalf("expected no error for a far-oversized numeric cursor, got %v", err)
	}
	if len(oversized.Nodes) != 0 || oversized.Page.NextCursor != "" {
		t.Fatalf("expected 0 nodes and empty cursor, got %d nodes, cursor %q", len(oversized.Nodes), oversized.Page.NextCursor)
	}

	hEmpty := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates())
	empty, err := hEmpty.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{})
	if err != nil {
		t.Fatalf("expected no error for an empty store, got %v", err)
	}
	if len(empty.Nodes) != 0 || empty.Page.NextCursor != "" {
		t.Fatalf("expected 0 nodes and empty cursor for an empty store, got %d nodes, cursor %q", len(empty.Nodes), empty.Page.NextCursor)
	}
}

func TestInventory_ListNodesOrderingIsStable(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	first, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Nodes) != len(second.Nodes) {
		t.Fatalf("expected equal length, got %d vs %d", len(first.Nodes), len(second.Nodes))
	}
	for i := range first.Nodes {
		if first.Nodes[i].Id != second.Nodes[i].Id {
			t.Fatalf("id mismatch at %d: %q vs %q", i, first.Nodes[i].Id, second.Nodes[i].Id)
		}
	}
	for i := 1; i < len(first.Nodes); i++ {
		if first.Nodes[i-1].Id >= first.Nodes[i].Id {
			t.Fatalf("expected ascending order, got %q before %q", first.Nodes[i-1].Id, first.Nodes[i].Id)
		}
	}

	foundDiscovered := false
	for _, n := range first.Nodes {
		if n.Status == hostv1.Node_DISCOVERED {
			foundDiscovered = true
		}
	}
	if !foundDiscovered {
		t.Fatal("expected at least one DISCOVERED node in the list")
	}
}
