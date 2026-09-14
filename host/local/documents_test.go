package local_test

import (
	"context"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDocuments_CreateOnly(t *testing.T) {
	h := local.New(nil, "opentofu")
	ctx := context.Background()

	resp, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: "state", DocId: "prod",
		Body:      &hostv1.Json{Value: mustStruct(t, map[string]any{"v": 1})},
		IfVersion: 0,
	})
	if err != nil {
		t.Fatalf("create-only put on empty doc: %v", err)
	}
	if resp.Version != 1 {
		t.Fatalf("expected version 1, got %d", resp.Version)
	}

	_, err = h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: "state", DocId: "prod",
		Body:      &hostv1.Json{Value: mustStruct(t, map[string]any{"v": 2})},
		IfVersion: 0,
	})
	if err == nil {
		t.Fatal("expected create-only put to fail when doc already exists")
	}
}

func TestDocuments_CASConflict(t *testing.T) {
	h := local.New(nil, "opentofu")
	ctx := context.Background()

	first, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: "state", DocId: "prod",
		Body: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": 1})},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: "state", DocId: "prod",
		Body:      &hostv1.Json{Value: mustStruct(t, map[string]any{"v": 2})},
		IfVersion: first.Version + 1, // stale on purpose
	})
	if err == nil {
		t.Fatal("expected stale if_version to be rejected")
	}
}

func TestDocuments_GetDeleteRoundTrip(t *testing.T) {
	h := local.New(nil, "opentofu")
	ctx := context.Background()

	put, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: "state", DocId: "prod",
		Body: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": 1})},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: "state", DocId: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != put.Version {
		t.Fatalf("expected version %d, got %d", put.Version, got.Version)
	}

	if _, err := h.Documents.Delete(ctx, &hostv1.DeleteDocumentRequest{
		Collection: "state", DocId: "prod", IfVersion: got.Version,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: "state", DocId: "prod"}); err == nil {
		t.Fatal("expected Get to fail after Delete")
	}
}

func TestDocuments_AlwaysAvailable_NoPermissionNeeded(t *testing.T) {
	h := local.New([]string{}, "opentofu") // deliberately empty permission set
	ctx := context.Background()
	if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: "state", DocId: "prod",
		Body: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": 1})},
	}); err != nil {
		t.Fatalf("Documents must be always-available regardless of declared permissions: %v", err)
	}
}
