package local_test

import (
	"context"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

func TestAuth_IssueVerifyRevoke(t *testing.T) {
	h := local.New([]string{"tokens:issue"}, "opentofu")
	ctx := context.Background()

	tok, err := h.Auth.IssueToken(ctx, &hostv1.IssueTokenRequest{Scope: "state:rw", Label: "runner-1", TtlSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Secret == "" || tok.TokenId == "" {
		t.Fatal("expected a non-empty token id and secret")
	}

	principal, err := h.Auth.Verify(ctx, &hostv1.VerifyTokenRequest{Secret: tok.Secret, Scope: "state:rw"})
	if err != nil {
		t.Fatal(err)
	}
	if principal.TokenId != tok.TokenId {
		t.Fatalf("expected token id %q, got %q", tok.TokenId, principal.TokenId)
	}

	if _, err := h.Auth.Revoke(ctx, &hostv1.TokenRef{TokenId: tok.TokenId}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Auth.Verify(ctx, &hostv1.VerifyTokenRequest{Secret: tok.Secret, Scope: "state:rw"}); err == nil {
		t.Fatal("expected Verify to fail after Revoke")
	}
}

func TestAuth_VerifyWrongScopeFails(t *testing.T) {
	h := local.New([]string{"tokens:issue"}, "opentofu")
	ctx := context.Background()
	tok, err := h.Auth.IssueToken(ctx, &hostv1.IssueTokenRequest{Scope: "state:rw", Label: "runner-1", TtlSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Auth.Verify(ctx, &hostv1.VerifyTokenRequest{Secret: tok.Secret, Scope: "other:scope"}); err == nil {
		t.Fatal("expected Verify to fail for a scope the token wasn't issued with")
	}
}

func TestAuth_DeniedWithoutPermission(t *testing.T) {
	h := local.New(nil, "opentofu") // tokens:issue NOT declared
	_, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{Scope: "state:rw", TtlSeconds: 60})
	if err == nil {
		t.Fatal("expected PERMISSION_DENIED without tokens:issue")
	}
}
