package local_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

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
