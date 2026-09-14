package local_test

import (
	"context"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestSettings_CurrentDefaultsEmpty(t *testing.T) {
	h := local.New(nil, "opentofu")
	got, err := h.Settings.Current(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 0 {
		t.Fatalf("expected version 0 with no settings written yet, got %d", got.Version)
	}
}

func TestSettings_AlwaysAvailable_NoPermissionNeeded(t *testing.T) {
	h := local.New([]string{}, "opentofu")
	if _, err := h.Settings.Current(context.Background(), &emptypb.Empty{}); err != nil {
		t.Fatalf("Settings must be always-available: %v", err)
	}
}
