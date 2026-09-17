package local_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

func TestCode_CreateThenGetEnvironment(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	created, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if created.Name != "production" {
		t.Fatalf("created.Name = %q, want %q", created.Name, "production")
	}
	if created.CreatedAt == nil {
		t.Fatal("created.CreatedAt is nil, want populated")
	}
	if created.UpdatedAt == nil {
		t.Fatal("created.UpdatedAt is nil, want populated")
	}

	got, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if got.Name != "production" {
		t.Fatalf("got.Name = %q, want %q", got.Name, "production")
	}
}

func TestCode_CreateOnExistingNameIsAlreadyExists(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	first, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("first CreateEnvironment: %v", err)
	}

	_, err = h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"})
	if err == nil {
		t.Fatal("second CreateEnvironment succeeded, want AlreadyExists")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.AlreadyExists {
		t.Fatalf("second CreateEnvironment error = %v, want codes.AlreadyExists", err)
	}

	got, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("GetEnvironment after refused create: %v", err)
	}
	if !got.CreatedAt.AsTime().Equal(first.CreatedAt.AsTime()) {
		t.Fatalf("CreatedAt changed after refused duplicate create: got %v, want %v", got.CreatedAt.AsTime(), first.CreatedAt.AsTime())
	}

	list, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(list.Environments) != 1 {
		t.Fatalf("ListEnvironments after refused duplicate create returned %d environments, want 1", len(list.Environments))
	}
}

func TestCode_ListEnvironmentsPagination(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	// Empty store: zero environments, empty cursor, no error.
	empty, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
	if err != nil {
		t.Fatalf("ListEnvironments on empty store: %v", err)
	}
	if len(empty.Environments) != 0 {
		t.Fatalf("empty store returned %d environments, want 0", len(empty.Environments))
	}
	if empty.Page.NextCursor != "" {
		t.Fatalf("empty store NextCursor = %q, want empty", empty.Page.NextCursor)
	}

	for _, name := range []string{"staging", "production", "dev"} {
		if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: name}); err != nil {
			t.Fatalf("CreateEnvironment(%q): %v", name, err)
		}
	}

	all, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	wantOrder := []string{"dev", "production", "staging"}
	if len(all.Environments) != len(wantOrder) {
		t.Fatalf("ListEnvironments returned %d environments, want %d", len(all.Environments), len(wantOrder))
	}
	for i, name := range wantOrder {
		if all.Environments[i].Name != name {
			t.Fatalf("ListEnvironments[%d] = %q, want %q", i, all.Environments[i].Name, name)
		}
	}

	// Two-page walk with Limit: 2.
	page1, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{Page: &hostv1.Page{Limit: 2}})
	if err != nil {
		t.Fatalf("ListEnvironments page1: %v", err)
	}
	if len(page1.Environments) != 2 {
		t.Fatalf("page1 returned %d environments, want 2", len(page1.Environments))
	}
	if page1.Page.NextCursor == "" {
		t.Fatal("page1 NextCursor is empty, want non-empty")
	}

	page2, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{Page: &hostv1.Page{Limit: 2, Cursor: page1.Page.NextCursor}})
	if err != nil {
		t.Fatalf("ListEnvironments page2: %v", err)
	}
	if len(page2.Environments) != 1 {
		t.Fatalf("page2 returned %d environments, want 1", len(page2.Environments))
	}
	if page2.Page.NextCursor != "" {
		t.Fatalf("page2 NextCursor = %q, want empty", page2.Page.NextCursor)
	}
}

func TestCode_ListEnvironmentsRejectsBadCursor(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	for _, name := range []string{"staging", "production", "dev"} {
		if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: name}); err != nil {
			t.Fatalf("CreateEnvironment(%q): %v", name, err)
		}
	}

	for _, cursor := range []string{"-1", "abc"} {
		_, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{Page: &hostv1.Page{Cursor: cursor}})
		if err == nil {
			t.Fatalf("cursor %q succeeded, want InvalidArgument", cursor)
		}
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.InvalidArgument {
			t.Fatalf("cursor %q error = %v, want codes.InvalidArgument", cursor, err)
		}
	}

	outOfRange, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{Page: &hostv1.Page{Cursor: "99"}})
	if err != nil {
		t.Fatalf("out-of-range cursor: %v", err)
	}
	if len(outOfRange.Environments) != 0 {
		t.Fatalf("out-of-range cursor returned %d environments, want 0", len(outOfRange.Environments))
	}
	if outOfRange.Page.NextCursor != "" {
		t.Fatalf("out-of-range cursor NextCursor = %q, want empty", outOfRange.Page.NextCursor)
	}
}

func TestCode_CreateRejectsInvalidName(t *testing.T) {
	invalid := []string{"Production", "prod-1", "prod.1", "prod/1", "prod 1", "PROD", "", "naïve"}
	valid := []string{"production", "dev", "prod_1", "e2e_2026"}

	for _, name := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			h := local.New([]string{"code:rw"}, "controlrepo")
			_, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: name})
			if err == nil {
				t.Fatalf("CreateEnvironment(%q) succeeded, want InvalidArgument", name)
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.InvalidArgument {
				t.Fatalf("CreateEnvironment(%q) error = %v, want codes.InvalidArgument", name, err)
			}
		})
	}

	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			h := local.New([]string{"code:rw"}, "controlrepo")
			_, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: name})
			if err != nil {
				t.Fatalf("CreateEnvironment(%q) failed: %v", name, err)
			}
		})
	}
}

func TestCode_EnvironmentNameIsByteExact(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "PRODUCTION"}); err == nil {
		t.Fatal("GetEnvironment(\"PRODUCTION\") succeeded, want NotFound")
	} else if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("GetEnvironment(\"PRODUCTION\") error = %v, want codes.NotFound", err)
	}

	if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("GetEnvironment(\"production\") failed: %v", err)
	}
}

func TestCode_ReadPathClonesEnvironments(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	first, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	first.Name = "mutated"
	first.CreatedAt = nil

	second, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("GetEnvironment second read: %v", err)
	}
	if second.Name != "production" {
		t.Fatalf("second read Name = %q, want %q (mutation of first leaked into stored state)", second.Name, "production")
	}
	if second.CreatedAt == nil {
		t.Fatal("second read CreatedAt is nil (mutation of first leaked into stored state)")
	}
}
