package opentofulite_test

import (
	"context"
	"os"
	"testing"

	opentofulite "github.com/puppet-stagehand/stagehand-sdk/examples/opentofu-lite"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

func newBackend(t *testing.T) *opentofulite.Backend {
	t.Helper()
	raw, err := os.ReadFile("../../manifest/testdata/opentofu.json")
	if err != nil {
		t.Fatal(err)
	}
	m, findings := manifest.Parse(raw)
	if len(findings) > 0 {
		t.Fatalf("opentofu.json failed to parse: %v", findings)
	}
	h := local.New(m.Permissions, m.ID)
	return opentofulite.New(h)
}

func TestLock_ConflictReturnsHolderInfo(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()

	held, err := b.Lock(ctx, "prod", opentofulite.LockInfo{ID: "lock-a", Who: "alice"})
	if err != nil {
		t.Fatalf("first lock should succeed: %v", err)
	}
	if held.ID != "lock-a" {
		t.Fatalf("expected held lock id lock-a, got %q", held.ID)
	}

	_, err = b.Lock(ctx, "prod", opentofulite.LockInfo{ID: "lock-b", Who: "bob"})
	if err == nil {
		t.Fatal("expected second lock attempt to fail while prod is held")
	}
	conflict, ok := err.(*opentofulite.LockConflictError)
	if !ok {
		t.Fatalf("expected *opentofulite.LockConflictError, got %T: %v", err, err)
	}
	if conflict.Holder.ID != "lock-a" || conflict.Holder.Who != "alice" {
		t.Fatalf("expected conflict to carry the current holder's info, got %+v", conflict.Holder)
	}
}

func TestPutState_RejectedWithoutLock(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()

	err := b.PutState(ctx, "prod", "no-such-lock", []byte(`{"version":4}`))
	if err == nil {
		t.Fatal("expected PutState without a held lock to be rejected")
	}
}

func TestFullLockPutGetUnlockCycle(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()

	held, err := b.Lock(ctx, "prod", opentofulite.LockInfo{ID: "lock-a", Who: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	if err := b.PutState(ctx, "prod", held.ID, []byte(`{"version":4,"serial":1}`)); err != nil {
		t.Fatalf("PutState with a valid held lock should succeed: %v", err)
	}

	got, err := b.GetState(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"version":4,"serial":1}` {
		t.Fatalf("expected round-tripped state, got %q", got)
	}

	if err := b.Unlock(ctx, "prod", held.ID); err != nil {
		t.Fatalf("unlock with the correct lock id should succeed: %v", err)
	}

	// now unheld: a second Lock attempt must succeed
	if _, err := b.Lock(ctx, "prod", opentofulite.LockInfo{ID: "lock-c", Who: "carol"}); err != nil {
		t.Fatalf("expected lock to succeed once unheld: %v", err)
	}
}

func TestDeleteState_RejectedWhileLocked(t *testing.T) {
	b := newBackend(t)
	ctx := context.Background()
	held, err := b.Lock(ctx, "prod", opentofulite.LockInfo{ID: "lock-a", Who: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	_ = held
	if err := b.DeleteState(ctx, "prod", "wrong-lock-id"); err == nil {
		t.Fatal("expected DeleteState with the wrong lock id to be rejected")
	}
}

func TestMintCredential(t *testing.T) {
	b := newBackend(t)
	tokenID, secret, err := b.MintCredential(context.Background(), "runner-1")
	if err != nil {
		t.Fatal(err)
	}
	if tokenID == "" || secret == "" {
		t.Fatal("expected a non-empty token id and secret")
	}
}
