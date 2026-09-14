package local_test

import (
	"bytes"
	"context"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

func TestSecrets_StoreRevealDelete(t *testing.T) {
	h := local.New([]string{"secrets:rw"}, "opentofu")
	ctx := context.Background()

	ref, err := h.Secrets.Store(ctx, &hostv1.StoreSecretRequest{Name: "backend-token", Plaintext: []byte("s3cr3t")})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref != "expansion/opentofu/backend-token" {
		t.Fatalf("expected namespaced ref, got %q", ref.Ref)
	}

	val, err := h.Secrets.Reveal(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(val.Plaintext, []byte("s3cr3t")) {
		t.Fatalf("expected round-trip plaintext, got %q", val.Plaintext)
	}

	if _, err := h.Secrets.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Secrets.Reveal(ctx, ref); err == nil {
		t.Fatal("expected Reveal to fail after Delete")
	}
}

func TestSecrets_SealOpenRoundTrip(t *testing.T) {
	h := local.New([]string{"secrets:rw"}, "opentofu")
	ctx := context.Background()

	sealed, err := h.Secrets.Seal(ctx, &hostv1.SecretValue{Plaintext: []byte("embed-me")})
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed.Ciphertext) == 0 || len(sealed.Nonce) == 0 {
		t.Fatal("expected non-empty ciphertext and nonce")
	}

	opened, err := h.Secrets.Open(ctx, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened.Plaintext, []byte("embed-me")) {
		t.Fatalf("expected round-trip plaintext, got %q", opened.Plaintext)
	}
}

func TestSecrets_DeniedWithoutPermission(t *testing.T) {
	h := local.New(nil, "opentofu") // secrets:rw NOT declared
	_, err := h.Secrets.Store(context.Background(), &hostv1.StoreSecretRequest{Name: "x", Plaintext: []byte("y")})
	if err == nil {
		t.Fatal("expected PERMISSION_DENIED without secrets:rw")
	}
}
