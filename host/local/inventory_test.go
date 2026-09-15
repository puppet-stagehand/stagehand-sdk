package local_test

import (
	"context"
	"fmt"
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

func TestInventory_QueryNodesByFact(t *testing.T) {
	nodes := []*hostv1.Node{
		{Id: "n1", DisplayName: "n1", Status: hostv1.Node_DISCOVERED},
		{Id: "n2", DisplayName: "n2", Status: hostv1.Node_DISCOVERED},
		{Id: "n3", DisplayName: "n3", Status: hostv1.Node_DISCOVERED},
	}
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(nodes...))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "n1", Facts: jsonFacts(t, map[string]any{
		"os": "linux", "hostname": "web-front-1", "cores": float64(4),
		"cpu": map[string]any{"cores": float64(4)},
	})}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "n2", Facts: jsonFacts(t, map[string]any{
		"os": "windows", "hostname": "db-back-2", "cores": float64(8),
	})}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "n3", Facts: jsonFacts(t, map[string]any{
		"os": "linux", "hostname": "web-front-3", "cores": float64(2),
	})}); err != nil {
		t.Fatal(err)
	}

	resp, err := h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "os", Op: hostv1.QueryNodesRequest_EQ, Value: jsonWrap(t, "linux"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, resp.Nodes, "n1", "n3")

	resp, err = h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "hostname", Op: hostv1.QueryNodesRequest_CONTAINS, Value: jsonWrap(t, "front"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, resp.Nodes, "n1", "n3")

	resp, err = h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "cores", Op: hostv1.QueryNodesRequest_GT, Value: jsonWrap(t, float64(3)),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, resp.Nodes, "n1", "n2")

	resp, err = h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "nope", Op: hostv1.QueryNodesRequest_EQ, Value: jsonWrap(t, "x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 0 {
		t.Fatalf("expected 0 matches for a field no node carries, got %d", len(resp.Nodes))
	}

	resp, err = h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "os", Op: hostv1.QueryNodesRequest_GT, Value: jsonWrap(t, float64(1)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Nodes) != 0 {
		t.Fatalf("expected 0 matches for a type-mismatched comparison, got %d", len(resp.Nodes))
	}

	resp, err = h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "cpu.cores", Op: hostv1.QueryNodesRequest_EQ, Value: jsonWrap(t, float64(4)),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, resp.Nodes, "n1")

	resp, err = h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "os", Op: hostv1.QueryNodesRequest_EQ, Value: jsonWrap(t, "linux"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(resp.Nodes); i++ {
		if resp.Nodes[i-1].Id >= resp.Nodes[i].Id {
			t.Fatalf("expected ascending order, got %q before %q", resp.Nodes[i-1].Id, resp.Nodes[i].Id)
		}
	}

	firstPage, err := h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "os", Op: hostv1.QueryNodesRequest_EQ, Value: jsonWrap(t, "linux"), Page: &hostv1.Page{Limit: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Nodes) != 1 || firstPage.Page.NextCursor != "1" {
		t.Fatalf("expected 1 node and NextCursor \"1\" on the first page, got %d nodes, cursor %q", len(firstPage.Nodes), firstPage.Page.NextCursor)
	}
	secondPage, err := h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "os", Op: hostv1.QueryNodesRequest_EQ, Value: jsonWrap(t, "linux"), Page: &hostv1.Page{Cursor: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Nodes) != 1 || secondPage.Page.NextCursor != "" {
		t.Fatalf("expected 1 node and no cursor on the final page, got %d nodes, cursor %q", len(secondPage.Nodes), secondPage.Page.NextCursor)
	}

	if _, err := h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "os", Op: hostv1.QueryNodesRequest_EQ, Value: jsonWrap(t, "linux"), Page: &hostv1.Page{Cursor: "-1"},
	}); err == nil {
		t.Fatal("expected error for a negative cursor")
	} else if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", st.Code())
	}
}

func TestInventory_QueryNodesOpParityWithDocuments(t *testing.T) {
	pairs := []struct {
		name string
		inv  hostv1.QueryNodesRequest_Op
		doc  hostv1.QueryDocumentsRequest_Op
	}{
		{"OP_UNSPECIFIED", hostv1.QueryNodesRequest_OP_UNSPECIFIED, hostv1.QueryDocumentsRequest_OP_UNSPECIFIED},
		{"EQ", hostv1.QueryNodesRequest_EQ, hostv1.QueryDocumentsRequest_EQ},
		{"NE", hostv1.QueryNodesRequest_NE, hostv1.QueryDocumentsRequest_NE},
		{"GT", hostv1.QueryNodesRequest_GT, hostv1.QueryDocumentsRequest_GT},
		{"GTE", hostv1.QueryNodesRequest_GTE, hostv1.QueryDocumentsRequest_GTE},
		{"LT", hostv1.QueryNodesRequest_LT, hostv1.QueryDocumentsRequest_LT},
		{"LTE", hostv1.QueryNodesRequest_LTE, hostv1.QueryDocumentsRequest_LTE},
		{"CONTAINS", hostv1.QueryNodesRequest_CONTAINS, hostv1.QueryDocumentsRequest_CONTAINS},
	}
	if len(pairs) != 8 {
		t.Fatalf("expected 8 operator pairs, got %d", len(pairs))
	}
	for _, p := range pairs {
		if int32(p.inv) != int32(p.doc) {
			t.Fatalf("%s: numeric mismatch, QueryNodesRequest_Op=%d QueryDocumentsRequest_Op=%d", p.name, int32(p.inv), int32(p.doc))
		}
	}
}

func TestInventory_GroupsRoundTrip(t *testing.T) {
	// WithGroupClasses(nil) opts out of the default class fixture (which
	// auto-creates a "webservers" group) so ListGroups reflects only the
	// membership this test creates.
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(
		&hostv1.Node{Id: "n1", DisplayName: "n1", Status: hostv1.Node_DISCOVERED},
	), local.WithGroupClasses(nil))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "n1", GroupId: "web"}); err != nil {
		t.Fatal(err)
	}

	groups, err := h.Inventory.ListGroups(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 1 || groups.Groups[0].Id != "web" || groups.Groups[0].Name != "web" {
		t.Fatalf("expected group %q with name %q, got %+v", "web", "web", groups.Groups)
	}

	groupNodes, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groupNodes.Nodes) != 1 || groupNodes.Nodes[0].Id != "n1" {
		t.Fatalf("expected node n1 in group web, got %+v", groupNodes.Nodes)
	}

	nodeGroups, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeGroups.Groups) != 1 || nodeGroups.Groups[0].Id != "web" {
		t.Fatalf("expected node n1 to belong to group web, got %+v", nodeGroups.Groups)
	}
}

func TestInventory_GroupsOnDiscoveredNode(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(
		&hostv1.Node{Id: "n1", DisplayName: "n1", Status: hostv1.Node_DISCOVERED},
	))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "n1", GroupId: "web"}); err != nil {
		t.Fatal(err)
	}

	groupNodes, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groupNodes.Nodes) != 1 || groupNodes.Nodes[0].Status != hostv1.Node_DISCOVERED {
		t.Fatalf("expected 1 DISCOVERED node, got %+v", groupNodes.Nodes)
	}

	nodeGroups, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeGroups.Groups) != 1 || nodeGroups.Groups[0].Id != "web" {
		t.Fatalf("expected group web, got %+v", nodeGroups.Groups)
	}
}

func TestInventory_AddNodeToGroupIsIdempotent(t *testing.T) {
	// WithGroupClasses(nil) opts out of the default class fixture so the
	// "exactly 1 group" assertion below reflects only this test's own add.
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(
		&hostv1.Node{Id: "n1", DisplayName: "n1", Status: hostv1.Node_DISCOVERED},
	), local.WithGroupClasses(nil))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "n1", GroupId: "web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "n1", GroupId: "web"}); err != nil {
		t.Fatal(err)
	}

	groups, err := h.Inventory.ListGroups(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 1 || groups.Groups[0].Name != "web" {
		t.Fatalf("expected exactly 1 group named %q, got %+v", "web", groups.Groups)
	}

	groupNodes, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groupNodes.Nodes) != 1 {
		t.Fatalf("expected exactly 1 member, got %d", len(groupNodes.Nodes))
	}

	nodeGroups, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeGroups.Groups) != 1 {
		t.Fatalf("expected exactly 1 membership, got %d", len(nodeGroups.Groups))
	}

	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "nope", GroupId: "web"}); err == nil {
		t.Fatal("expected error for unknown node id")
	} else if st, _ := status.FromError(err); st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", st.Code())
	}

	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "n1", GroupId: ""}); err == nil {
		t.Fatal("expected error for empty group id")
	} else if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", st.Code())
	}
}

func TestInventory_GroupsEmptyAndUnknown(t *testing.T) {
	// WithGroupClasses(nil) is the explicit "no injected classes" case the
	// GRP-01 truth describes: a fresh host with no injected classes has 0
	// groups. The bare-default-host, non-empty-webservers case is covered
	// separately by TestInventory_ListClasses.
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(
		&hostv1.Node{Id: "n1", DisplayName: "n1", Status: hostv1.Node_DISCOVERED},
	), local.WithGroupClasses(nil))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	groups, err := h.Inventory.ListGroups(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("expected no error for a fresh host, got %v", err)
	}
	if len(groups.Groups) != 0 {
		t.Fatalf("expected 0 groups on a fresh host, got %d", len(groups.Groups))
	}

	groupNodes, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "nope"})
	if err != nil {
		t.Fatalf("expected no error for an unknown group id, got %v", err)
	}
	if len(groupNodes.Nodes) != 0 {
		t.Fatalf("expected 0 nodes for an unknown group id, got %d", len(groupNodes.Nodes))
	}

	nodeGroups, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "n1"})
	if err != nil {
		t.Fatalf("expected no error for an ungrouped node, got %v", err)
	}
	if len(nodeGroups.Groups) != 0 {
		t.Fatalf("expected 0 groups for an ungrouped node, got %d", len(nodeGroups.Groups))
	}
}

func TestInventory_GroupsOrderingIsStable(t *testing.T) {
	nodes := []*hostv1.Node{
		{Id: "c1", DisplayName: "c1", Status: hostv1.Node_DISCOVERED},
		{Id: "a1", DisplayName: "a1", Status: hostv1.Node_DISCOVERED},
		{Id: "b1", DisplayName: "b1", Status: hostv1.Node_DISCOVERED},
	}
	// WithGroupClasses(nil) opts out of the default class fixture so the
	// exact-3-groups assertions below reflect only this test's own adds.
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(nodes...), local.WithGroupClasses(nil))
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	// three groups, each membership cross-cutting so both directions have
	// more than one entry to sort.
	groupIDs := []string{"gc", "ga", "gb"}
	for _, gid := range groupIDs {
		for _, n := range nodes {
			if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: n.Id, GroupId: gid}); err != nil {
				t.Fatal(err)
			}
		}
	}

	for i := 0; i < 2; i++ {
		groups, err := h.Inventory.ListGroups(ctx, &emptypb.Empty{})
		if err != nil {
			t.Fatal(err)
		}
		wantGroups := []string{"ga", "gb", "gc"}
		if len(groups.Groups) != len(wantGroups) {
			t.Fatalf("call %d: expected %d groups, got %d", i, len(wantGroups), len(groups.Groups))
		}
		for j, w := range wantGroups {
			if groups.Groups[j].Id != w {
				t.Fatalf("call %d: expected group[%d]=%q, got %q", i, j, w, groups.Groups[j].Id)
			}
		}

		groupNodes, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "ga"})
		if err != nil {
			t.Fatal(err)
		}
		wantNodes := []string{"a1", "b1", "c1"}
		if len(groupNodes.Nodes) != len(wantNodes) {
			t.Fatalf("call %d: expected %d nodes, got %d", i, len(wantNodes), len(groupNodes.Nodes))
		}
		for j, w := range wantNodes {
			if groupNodes.Nodes[j].Id != w {
				t.Fatalf("call %d: expected node[%d]=%q, got %q", i, j, w, groupNodes.Nodes[j].Id)
			}
		}

		nodeGroups, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "a1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(nodeGroups.Groups) != len(wantGroups) {
			t.Fatalf("call %d: expected %d node-groups, got %d", i, len(wantGroups), len(nodeGroups.Groups))
		}
		for j, w := range wantGroups {
			if nodeGroups.Groups[j].Id != w {
				t.Fatalf("call %d: expected nodeGroups[%d]=%q, got %q", i, j, w, nodeGroups.Groups[j].Id)
			}
		}
	}

	first, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "ga", Page: &hostv1.Page{Limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Nodes) != 1 || first.Page.NextCursor != "1" {
		t.Fatalf("expected 1 node and NextCursor \"1\", got %d nodes, cursor %q", len(first.Nodes), first.Page.NextCursor)
	}
	seen := map[string]bool{first.Nodes[0].Id: true}
	cursor := first.Page.NextCursor
	for i := 0; i < 10 && cursor != ""; i++ {
		resp, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "ga", Page: &hostv1.Page{Cursor: cursor, Limit: 1}})
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

	if _, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "ga", Page: &hostv1.Page{Cursor: "-1"}}); err == nil {
		t.Fatal("expected error for a negative cursor")
	} else if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", st.Code())
	}

	groups, err := h.Inventory.ListGroups(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	groups.Groups[0].Name = "mutated"
	again, err := h.Inventory.ListGroups(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Groups[0].Name == "mutated" {
		t.Fatal("expected mutation of a returned group to not affect the next ListGroups call")
	}
}

func TestInventory_ListClasses(t *testing.T) {
	h := local.New([]string{"inventory:rw"}, "opentofu")
	ctx := context.Background()
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	classes, err := h.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: "webservers"})
	if err != nil {
		t.Fatal(err)
	}
	if len(classes.Classes) != 2 || classes.Classes[0].Name != "profile::base" || classes.Classes[1].Name != "profile::web" {
		t.Fatalf("expected default classes profile::base, profile::web ascending, got %+v", classes.Classes)
	}

	unknown, err := h.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: "nope"})
	if err != nil {
		t.Fatalf("expected no error for an unknown group id, got %v", err)
	}
	if len(unknown.Classes) != 0 {
		t.Fatalf("expected 0 classes for an unknown group id, got %d", len(unknown.Classes))
	}

	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "db-01.example.test", GroupId: "dbservers"}); err != nil {
		t.Fatal(err)
	}
	noClasses, err := h.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: "dbservers"})
	if err != nil {
		t.Fatalf("expected no error for a group with no classes, got %v", err)
	}
	if len(noClasses.Classes) != 0 {
		t.Fatalf("expected 0 classes for a member-only group, got %d", len(noClasses.Classes))
	}

	classes.Classes[0].Name = "mutated"
	again, err := h.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: "webservers"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Classes[0].Name == "mutated" {
		t.Fatal("expected mutation of a returned class to not affect the next ListClasses call")
	}
}

func TestInventory_ListClassesAreInjectable(t *testing.T) {
	ctx := context.Background()

	params, err := structpb.NewStruct(map[string]any{"port": float64(443)})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}

	injected := map[string][]*hostv1.Class{
		"lb": {
			{Name: "profile::lb", Parameters: &hostv1.Json{Value: params}},
			{Name: "profile::dup"},
			{Name: "profile::dup"}, // duplicate name on the same group must collapse to one entry
		},
		"db": {
			{Name: "profile::db"},
		},
	}
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithGroupClasses(injected))

	lb, err := h.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: "lb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(lb.Classes) != 2 {
		t.Fatalf("expected duplicate class name to collapse to 1 entry alongside profile::lb, got %d: %+v", len(lb.Classes), lb.Classes)
	}
	var found bool
	for _, c := range lb.Classes {
		if c.Name == "profile::lb" {
			found = true
			if c.Parameters == nil || c.Parameters.Value.AsMap()["port"] != float64(443) {
				t.Fatalf("expected Parameters to round-trip, got %+v", c.Parameters)
			}
		}
	}
	if !found {
		t.Fatal("expected profile::lb in the result")
	}

	groups, err := h.Inventory.ListGroups(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	seenGroups := map[string]bool{}
	for _, g := range groups.Groups {
		seenGroups[g.Id] = true
	}
	if !seenGroups["lb"] || !seenGroups["db"] {
		t.Fatalf("expected both injected group ids in ListGroups before any node joins them, got %+v", groups.Groups)
	}

	hNil := local.New([]string{"inventory:rw"}, "opentofu", local.WithGroupClasses(nil))
	for _, gid := range []string{"webservers", "lb", "db"} {
		classes, err := hNil.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: gid})
		if err != nil {
			t.Fatal(err)
		}
		if len(classes.Classes) != 0 {
			t.Fatalf("expected WithGroupClasses(nil) to yield 0 classes for %q, got %d", gid, len(classes.Classes))
		}
	}
}

func assertNodeIDs(t *testing.T, nodes []*hostv1.Node, want ...string) {
	t.Helper()
	got := nodeIDs(nodes)
	if len(nodes) != len(want) {
		t.Fatalf("expected %d nodes %v, got %d: %v", len(want), want, len(nodes), got)
	}
	for i, w := range want {
		if nodes[i].Id != w {
			t.Fatalf("expected node[%d].Id == %q, got %q (full: %v)", i, w, nodes[i].Id, got)
		}
	}
}

func nodeIDs(nodes []*hostv1.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Id
	}
	return out
}

// writeProposal writes a D-08-shaped document directly through
// h.Documents.Put — collection "inventory-proposals", body fields "status"
// and "node" — standing in for Phase 4's governed proposal-writer, which
// does not exist yet. The Documents store accepts a write to this
// collection from anywhere; a future reader must not mistake this test
// scaffolding for the intended production writer. Passing an empty
// proposalStatus omits the "status" key entirely (simulating a proposal
// document with no status field); passing a nil node omits the "node" key.
func writeProposal(t *testing.T, h *host.Host, proposalID, proposalStatus string, node map[string]any) {
	t.Helper()
	body := map[string]any{}
	if proposalStatus != "" {
		body["status"] = proposalStatus
	}
	if node != nil {
		body["node"] = node
	}
	s, err := structpb.NewStruct(body)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	if _, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: "inventory-proposals",
		DocId:      proposalID,
		Body:       &hostv1.Json{Value: s},
		IfVersion:  0,
	}); err != nil {
		t.Fatalf("writeProposal Put(%q): %v", proposalID, err)
	}
}

func TestInventory_OnboardNodeRequiresApprovedProposal(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		id     string
		status string
		node   map[string]any
		none   bool // no document written at all
	}{
		{name: "no document", id: "p-none", none: true},
		{name: "pending", id: "p-pending", status: "pending", node: map[string]any{"id": "p-pending"}},
		{name: "rejected", id: "p-rejected", status: "rejected", node: map[string]any{"id": "p-rejected"}},
		{name: "missing status", id: "p-nostatus", status: "", node: map[string]any{"id": "p-nostatus"}},
		{name: "approved but no node object", id: "p-nonode", status: "approved", node: nil},
		{name: "node.id disagrees with proposal id", id: "p-mismatch", status: "approved", node: map[string]any{"id": "someone-else"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := local.New([]string{"inventory:rw"}, "opentofu")
			if !tc.none {
				writeProposal(t, h, tc.id, tc.status, tc.node)
			}
			_, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: tc.id})
			if err == nil {
				t.Fatalf("expected an error for case %q, got nil", tc.name)
			}
			wantCode := codes.FailedPrecondition
			if tc.none {
				wantCode = codes.NotFound
			}
			if status.Code(err) != wantCode {
				t.Fatalf("case %q: expected %v, got %v (%v)", tc.name, wantCode, status.Code(err), err)
			}
		})
	}
}

func TestInventory_OnboardNodeMaterializes(t *testing.T) {
	ctx := context.Background()
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(
		&hostv1.Node{Id: "n1", DisplayName: "n1-discovered", Status: hostv1.Node_DISCOVERED, Environment: "staging"},
	))

	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "n1", GroupId: "onboard-group"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "n1", Facts: jsonFacts(t, map[string]any{"os": "linux"})}); err != nil {
		t.Fatal(err)
	}

	writeProposal(t, h, "n1", "approved", map[string]any{
		"id":           "n1",
		"display_name": "", // empty proposal name must not blank a non-empty discovery name
		"environment":  "production",
		"facts": map[string]any{
			"role": "web",
		},
	})

	n, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if n.Id != "n1" {
		t.Fatalf("expected Id n1, got %q", n.Id)
	}
	if n.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected ONBOARDED, got %v", n.Status)
	}
	if n.DisplayName != "n1-discovered" {
		t.Fatalf("expected discovery display name preserved, got %q", n.DisplayName)
	}
	if n.Environment != "production" {
		t.Fatalf("expected environment from proposal, got %q", n.Environment)
	}
	if factValue(t, n, "os") != "linux" {
		t.Fatal("expected pre-onboarding fact 'os' preserved")
	}
	if factValue(t, n, "role") != "web" {
		t.Fatal("expected proposal fact 'role' merged in")
	}

	got, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("GetNode: expected ONBOARDED, got %v", got.Status)
	}

	lst, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var foundOnboarded bool
	for _, nn := range lst.Nodes {
		if nn.Id == "n1" && nn.Status == hostv1.Node_ONBOARDED {
			foundOnboarded = true
		}
	}
	if !foundOnboarded {
		t.Fatal("expected ListNodes to show ONBOARDED n1")
	}

	groups, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 1 || groups.Groups[0].Id != "onboard-group" {
		t.Fatalf("expected group membership preserved after onboarding, got %+v", groups.Groups)
	}

	disc, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range disc.Candidates {
		if c.Id == "n1" {
			t.Fatal("expected onboarded node to no longer appear as a Discover candidate")
		}
	}
}

func TestInventory_OnboardNodeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates())
	writeProposal(t, h, "n9", "approved", map[string]any{"id": "n9", "display_name": "n9", "environment": "prod"})

	first, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: "n9"})
	if err != nil {
		t.Fatal(err)
	}

	// Delete the proposal document to prove the second call does not
	// re-read it — the ONBOARDED short-circuit must fire before any
	// Documents access.
	if _, err := h.Documents.Delete(ctx, &hostv1.DeleteDocumentRequest{Collection: "inventory-proposals", DocId: "n9"}); err != nil {
		t.Fatal(err)
	}

	second, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: "n9"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != hostv1.Node_ONBOARDED || second.Id != first.Id {
		t.Fatalf("expected idempotent onboard, got %+v", second)
	}
}

func TestInventory_OnboardNodeConcurrentIsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates())
	writeProposal(t, h, "n10", "approved", map[string]any{"id": "n10"})

	const n = 2
	var wg sync.WaitGroup
	errs := make([]error, n)
	statuses := make([]hostv1.Node_Status, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			node, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: "n10"})
			errs[i] = err
			if node != nil {
				statuses[i] = node.Status
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error %v", i, err)
		}
		if statuses[i] != hostv1.Node_ONBOARDED {
			t.Fatalf("goroutine %d: expected ONBOARDED, got %v", i, statuses[i])
		}
	}

	lst, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, node := range lst.Nodes {
		if node.Id == "n10" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 node record for n10, got %d", count)
	}
}

// TestInventory_EndToEndDiscoverGroupProposeOnboard walks the milestone's
// documented discover-group-propose-onboard flow as one composed sequence
// with assertions between every step, proving what no single RPC's unit
// coverage can: that a fact survives grouping, that grouping survives
// onboarding, and that a fact attached before onboarding is still
// queryable after it.
func TestInventory_EndToEndDiscoverGroupProposeOnboard(t *testing.T) {
	ctx := context.Background()
	candidate := &hostv1.Node{Id: "e2e-01", DisplayName: "e2e-01-discovered", Status: hostv1.Node_DISCOVERED, Environment: "staging"}
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(candidate))

	disc, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, disc.Candidates, "e2e-01")
	if disc.Candidates[0].Status != hostv1.Node_DISCOVERED {
		t.Fatalf("expected DISCOVERED at discovery, got %v", disc.Candidates[0].Status)
	}

	if _, err := h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "e2e-01", GroupId: "e2e-group"}); err != nil {
		t.Fatal(err)
	}

	beforeOnboard, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "e2e-01"})
	if err != nil {
		t.Fatal(err)
	}
	if beforeOnboard.Status != hostv1.Node_DISCOVERED {
		t.Fatalf("expected DISCOVERED at grouping time, got %v", beforeOnboard.Status)
	}

	if _, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{NodeId: "e2e-01", Facts: jsonFacts(t, map[string]any{
		"os": "linux",
		"az": "us-east-1a",
	})}); err != nil {
		t.Fatal(err)
	}

	groupsBefore, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "e2e-01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groupsBefore.Groups) != 1 || groupsBefore.Groups[0].Id != "e2e-group" {
		t.Fatalf("expected group membership before onboarding, got %+v", groupsBefore.Groups)
	}

	writeProposal(t, h, "e2e-01", "approved", map[string]any{
		"id":           "e2e-01",
		"display_name": "", // empty proposal name must not blank the discovery name
		"environment":  "production",
		"facts": map[string]any{
			"role": "web",
		},
	})

	onboarded, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: "e2e-01"})
	if err != nil {
		t.Fatal(err)
	}
	if onboarded.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected ONBOARDED, got %v", onboarded.Status)
	}

	got, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "e2e-01"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("GetNode: expected ONBOARDED, got %v", got.Status)
	}

	queried, err := h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "os",
		Op:    hostv1.QueryNodesRequest_EQ,
		Value: jsonWrap(t, "linux"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNodeIDs(t, queried.Nodes, "e2e-01")

	groupsAfter, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: "e2e-01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(groupsAfter.Groups) != 1 || groupsAfter.Groups[0].Id != "e2e-group" {
		t.Fatalf("expected group membership preserved after onboarding, got %+v", groupsAfter.Groups)
	}

	discAfter, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range discAfter.Candidates {
		if c.Id == "e2e-01" {
			t.Fatal("expected onboarded node to no longer appear as a Discover candidate")
		}
	}

	// The same walk on a host without inventory:rw is refused at its
	// first Inventory call — the gate fronts the whole flow, not just
	// individual methods.
	hNoPerm := local.New(nil, "opentofu", local.WithDiscoverCandidates(candidate))
	if _, err := hNoPerm.Inventory.Discover(ctx, &emptypb.Empty{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied without inventory:rw, got %v", err)
	}
}

// TestInventory_ConcurrentMixedRPCsAreRaceFree gives -race something worth
// instrumenting across the whole facet: ten goroutines mixing read and
// write paths on shared state, including a concurrent OnboardNode against a
// pre-written approved proposal and two goroutines writing different fact
// keys to the same node — the per-key merge means neither writer may lose
// the other's key.
func TestInventory_ConcurrentMixedRPCsAreRaceFree(t *testing.T) {
	ctx := context.Background()

	nodes := make([]*hostv1.Node, 0, 10)
	for i := 0; i < 10; i++ {
		nodes = append(nodes, &hostv1.Node{
			Id:          fmt.Sprintf("mix-%02d", i),
			DisplayName: fmt.Sprintf("mix-%02d", i),
			Status:      hostv1.Node_DISCOVERED,
		})
	}
	h := local.New([]string{"inventory:rw"}, "opentofu", local.WithDiscoverCandidates(nodes...))
	if _, err := h.Inventory.Discover(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	// Pre-write an approved proposal so a concurrent OnboardNode has
	// something real to do.
	writeProposal(t, h, "mix-00", "approved", map[string]any{"id": "mix-00"})

	const iterations = 5
	var wg sync.WaitGroup
	errCh := make(chan error, 200)

	mixedWorker := func(idx int) {
		defer wg.Done()
		for iter := 0; iter < iterations; iter++ {
			var err error
			switch idx % 8 {
			case 0:
				_, err = h.Inventory.Discover(ctx, &emptypb.Empty{})
			case 1:
				_, err = h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "mix-01"})
			case 2:
				_, err = h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{
					NodeId: "mix-02",
					Facts:  jsonFacts(t, map[string]any{fmt.Sprintf("k-a-%d", iter): "va"}),
				})
			case 3:
				_, err = h.Inventory.AddNodeToGroup(ctx, &hostv1.GroupMembershipRequest{NodeId: "mix-03", GroupId: "mixgroup"})
			case 4:
				_, err = h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{})
			case 5:
				_, err = h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{Field: "os", Op: hostv1.QueryNodesRequest_EQ, Value: jsonWrap(t, "linux")})
			case 6:
				_, err = h.Inventory.ListClasses(ctx, &hostv1.GroupRef{Id: "webservers"})
			case 7:
				_, err = h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: "mix-00"})
			}
			if err != nil {
				errCh <- err
			}
		}
	}

	wg.Add(10)
	for i := 0; i < 8; i++ {
		go mixedWorker(i)
	}
	go func() {
		defer wg.Done()
		for iter := 0; iter < iterations; iter++ {
			if _, err := h.Inventory.PutFacts(ctx, &hostv1.PutFactsRequest{
				NodeId: "mix-02",
				Facts:  jsonFacts(t, map[string]any{fmt.Sprintf("k-b-%d", iter): "vb"}),
			}); err != nil {
				errCh <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for iter := 0; iter < iterations; iter++ {
			if _, err := h.Inventory.ListGroupNodes(ctx, &hostv1.ListGroupNodesRequest{GroupId: "mixgroup"}); err != nil {
				errCh <- err
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("unexpected error from concurrent call: %v", err)
	}

	lst, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range lst.Nodes {
		seen[n.Id]++
	}
	for _, n := range nodes {
		if seen[n.Id] != 1 {
			t.Fatalf("expected exactly 1 record for %q, got %d", n.Id, seen[n.Id])
		}
	}

	onboarded, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "mix-00"})
	if err != nil {
		t.Fatal(err)
	}
	if onboarded.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected mix-00 ONBOARDED, got %v", onboarded.Status)
	}

	finalMix02, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: "mix-02"})
	if err != nil {
		t.Fatal(err)
	}
	for iter := 0; iter < iterations; iter++ {
		if _, ok := finalMix02.Facts[fmt.Sprintf("k-a-%d", iter)]; !ok {
			t.Fatalf("expected fact k-a-%d to survive concurrent writes, got %+v", iter, finalMix02.Facts)
		}
		if _, ok := finalMix02.Facts[fmt.Sprintf("k-b-%d", iter)]; !ok {
			t.Fatalf("expected fact k-b-%d to survive concurrent writes, got %+v", iter, finalMix02.Facts)
		}
	}
}
