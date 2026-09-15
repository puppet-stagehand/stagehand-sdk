package local_test

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
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
