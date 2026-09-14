// Package opentofulite is the v0.1.0-rc.1 tracer slice's facet-sufficiency
// proof: it implements Terraform's real HTTP state-backend protocol (GET,
// POST, DELETE, LOCK, UNLOCK, plus a backend-credential mint) purely by
// composing the four in-scope host facets (Documents, Secrets, Auth —
// Settings isn't exercised by this protocol). It is not a real
// network-listening HTTP server and is not wire-compatibility tested
// against an actual tofu binary — that is stagehand-expansion-opentofu's
// job once the console host exists to terminate /api/v1/x/opentofu/*.
// What this proves: the chosen facets actually compose into Terraform's
// real locking/versioning semantics, not just a toy CRUD example.
package opentofulite

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

const (
	stateCollection = "state"
	lockCollection  = "locks"
	// stateVersionRetention is a fixed stand-in for the real OpenTofu pack's
	// settings-driven `retention` (manifest/testdata/opentofu.json's
	// settings_schema.retention.default is 20) — this proof has no console
	// operator UI to source that setting from, so it hardcodes the same
	// default rather than wiring up Settings for a value nothing here reads.
	stateVersionRetention = 20
)

// LockInfo mirrors Terraform's LOCK request body (trimmed to what this
// proof needs).
type LockInfo struct {
	ID  string
	Who string
}

// LockConflictError is returned when Lock is called on a workspace another
// caller already holds — it carries the current holder's info rather than
// a bare error, matching Terraform's real backend contract.
type LockConflictError struct {
	Holder LockInfo
}

func (e *LockConflictError) Error() string {
	return fmt.Sprintf("workspace is locked by %s (%s)", e.Holder.Who, e.Holder.ID)
}

// Backend composes host.Host's Documents, Secrets, and Auth facets into
// Terraform's state-backend operations.
type Backend struct {
	h *host.Host
}

func New(h *host.Host) *Backend { return &Backend{h: h} }

func lockDocID(workspace string) string { return workspace }

func (b *Backend) currentLock(ctx context.Context, workspace string) (LockInfo, bool, error) {
	doc, err := b.h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: lockCollection, DocId: lockDocID(workspace)})
	if err != nil {
		return LockInfo{}, false, nil // not found == unheld; real code would distinguish NotFound from other errors
	}
	m := doc.Body.Value.AsMap()
	id, _ := m["id"].(string)
	who, _ := m["who"].(string)
	return LockInfo{ID: id, Who: who}, true, nil
}

// Lock acquires the workspace lock, or returns *LockConflictError carrying
// the current holder's info if it's already held.
func (b *Backend) Lock(ctx context.Context, workspace string, info LockInfo) (LockInfo, error) {
	if held, ok, err := b.currentLock(ctx, workspace); err != nil {
		return LockInfo{}, err
	} else if ok {
		return LockInfo{}, &LockConflictError{Holder: held}
	}
	body, err := structpb.NewStruct(map[string]any{"id": info.ID, "who": info.Who})
	if err != nil {
		return LockInfo{}, err
	}
	if _, err := b.h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: lockCollection, DocId: lockDocID(workspace),
		Body: &hostv1.Json{Value: body}, IfVersion: 0, // create-only: races lose to whoever's Put lands first
	}); err != nil {
		// lost the create-only race; report whoever won as the holder
		if held, ok, gerr := b.currentLock(ctx, workspace); gerr == nil && ok {
			return LockInfo{}, &LockConflictError{Holder: held}
		}
		return LockInfo{}, err
	}
	return info, nil
}

// Unlock releases the lock, only when callerLockID matches the held lock.
func (b *Backend) Unlock(ctx context.Context, workspace, callerLockID string) error {
	held, ok, err := b.currentLock(ctx, workspace)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("workspace %q is not locked", workspace)
	}
	if held.ID != callerLockID {
		return fmt.Errorf("lock id mismatch: held by %q, caller provided %q", held.ID, callerLockID)
	}
	_, err = b.h.Documents.Delete(ctx, &hostv1.DeleteDocumentRequest{Collection: lockCollection, DocId: lockDocID(workspace)})
	return err
}

func (b *Backend) requireLock(ctx context.Context, workspace, callerLockID string) error {
	held, ok, err := b.currentLock(ctx, workspace)
	if err != nil {
		return err
	}
	if !ok || held.ID != callerLockID {
		return fmt.Errorf("state operation on %q requires holding its lock (have %q)", workspace, callerLockID)
	}
	return nil
}

// GetState returns the current state body as raw JSON bytes, byte-for-byte
// identical to what was written by PutState (see the "raw" wrapper note
// there — this is why GetState reads a string field back out rather than
// re-marshaling the stored structpb.Struct).
func (b *Backend) GetState(ctx context.Context, workspace string) ([]byte, error) {
	doc, err := b.h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: stateCollection, DocId: workspace})
	if err != nil {
		return nil, err
	}
	raw, _ := doc.Body.Value.AsMap()["raw"].(string)
	return []byte(raw), nil
}

// PutState writes new state, requiring the caller to hold the workspace's
// lock, then seals the previous version into a pruned version history.
func (b *Backend) PutState(ctx context.Context, workspace, callerLockID string, body []byte) error {
	if err := b.requireLock(ctx, workspace, callerLockID); err != nil {
		return err
	}
	if prev, err := b.h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: stateCollection, DocId: workspace}); err == nil {
		if err := b.archiveVersion(ctx, workspace, prev); err != nil {
			return err
		}
	}
	// Validate body is well-formed JSON before accepting it, but do NOT
	// store the unmarshaled structpb.Struct directly: protojson (which
	// backs structpb.Struct.MarshalJSON) re-serializes map fields with
	// keys sorted alphabetically, not in the source's original order.
	// Terraform's real state JSON has meaningful key order that a naive
	// unmarshal/remarshal round trip would silently scramble, breaking
	// GetState's exact-bytes contract. Instead, wrap the raw bytes in a
	// single-field struct ("raw" -> string) — a one-key object has no
	// ordering to lose, so the exact bytes survive the Documents facet's
	// JSON-native storage untouched.
	var validate structpb.Struct
	if err := validate.UnmarshalJSON(body); err != nil {
		return fmt.Errorf("invalid state JSON: %w", err)
	}
	wrapped, err := structpb.NewStruct(map[string]any{"raw": string(body)})
	if err != nil {
		return err
	}
	ifVersion := int64(0)
	if existing, err := b.h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: stateCollection, DocId: workspace}); err == nil {
		ifVersion = existing.Version
	}
	_, err = b.h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: stateCollection, DocId: workspace,
		Body: &hostv1.Json{Value: wrapped}, IfVersion: ifVersion,
	})
	return err
}

func (b *Backend) archiveVersion(ctx context.Context, workspace string, prev *hostv1.Document) error {
	sealed, err := b.h.Secrets.Seal(ctx, &hostv1.SecretValue{Plaintext: mustJSON(prev.Body.Value)})
	if err != nil {
		return err
	}
	archiveBody, err := structpb.NewStruct(map[string]any{
		"ciphertext": string(sealed.Ciphertext), "nonce": string(sealed.Nonce), "key_id": sealed.KeyId,
	})
	if err != nil {
		return err
	}
	versionDocID := fmt.Sprintf("%s/%d", workspace, prev.Version)
	if _, err := b.h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: "state-versions", DocId: versionDocID,
		Body: &hostv1.Json{Value: archiveBody}, IfVersion: 0,
	}); err != nil {
		return err
	}
	return b.pruneVersions(ctx, workspace)
}

func (b *Backend) pruneVersions(ctx context.Context, workspace string) error {
	listed, err := b.h.Documents.List(ctx, &hostv1.ListDocumentsRequest{Collection: "state-versions"})
	if err != nil {
		return err
	}
	var mine []*hostv1.Document
	for _, d := range listed.Documents {
		if len(d.DocId) > len(workspace) && d.DocId[:len(workspace)+1] == workspace+"/" {
			mine = append(mine, d)
		}
	}
	if len(mine) <= stateVersionRetention {
		return nil
	}
	excess := len(mine) - stateVersionRetention
	for i := 0; i < excess; i++ {
		if _, err := b.h.Documents.Delete(ctx, &hostv1.DeleteDocumentRequest{Collection: "state-versions", DocId: mine[i].DocId}); err != nil {
			return err
		}
	}
	return nil
}

func mustJSON(s *structpb.Struct) []byte {
	b, _ := s.MarshalJSON()
	return b
}

// DeleteState removes the workspace's state, requiring the caller to hold
// its lock.
func (b *Backend) DeleteState(ctx context.Context, workspace, callerLockID string) error {
	if err := b.requireLock(ctx, workspace, callerLockID); err != nil {
		return err
	}
	_, err := b.h.Documents.Delete(ctx, &hostv1.DeleteDocumentRequest{Collection: stateCollection, DocId: workspace})
	return err
}

// MintCredential issues a short-lived token for a runner to authenticate
// back to this backend.
func (b *Backend) MintCredential(ctx context.Context, label string) (tokenID, secret string, err error) {
	tok, err := b.h.Auth.IssueToken(ctx, &hostv1.IssueTokenRequest{Scope: "state:rw", Label: label, TtlSeconds: 3600})
	if err != nil {
		return "", "", err
	}
	return tok.TokenId, tok.Secret, nil
}
