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
