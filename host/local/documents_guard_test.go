package local_test

// Tests for the pack-facing Documents approval guard (FND-03, D-03, D-04, D-06,
// D-15, D-16): a pack holding only Documents cannot decide a proposal, a decided
// proposal is immutable, and approval.Approve / approval.Reject still decide
// through the token they carry in gRPC metadata.
//
// No test prints a token. Failure messages name the collection, the document id
// and the error text only, and TestDocumentsGuard_NoTokenInErrors pins that no
// refusal echoes a secret (T-13-08).

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

// guardKinds are the two registered approval Kinds, as the facets that read the
// proposals pin them.
var guardKinds = []struct {
	name string
	kind approval.Kind
}{
	{"inventory-proposals", approval.Kind{Collection: "inventory-proposals", ApproveScope: "inventory:approve"}},
	{"code-overwrites", approval.Kind{Collection: code.OverwriteCollection, ApproveScope: code.OverwriteApproveScope}},
}

func newGuardHost() *host.Host {
	return local.New([]string{"inventory:rw", "code:rw", "tokens:issue"}, "pack")
}

// guardPropose creates a pending proposal with a payload the freeze rule can
// compare, through the same ProposeBody path real proposers use.
func guardPropose(t *testing.T, h *host.Host, kind approval.Kind, id string) {
	t.Helper()
	body := map[string]any{"payload": map[string]any{"target": "prod", "count": float64(1)}}
	if _, err := approval.ProposeBody(context.Background(), h, kind, id, body); err != nil {
		t.Fatalf("ProposeBody(%s/%s): %v", kind.Collection, id, err)
	}
}

// guardToken issues a five-minute token for scope and returns its secret and
// label. The secret never appears in any failure message.
func guardToken(t *testing.T, h *host.Host, scope, label string) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{Scope: scope, Label: label, TtlSeconds: 300})
	if err != nil {
		t.Fatalf("IssueToken(%s): %v", scope, err)
	}
	return tok.Secret
}

// withTokens returns ctx carrying the given values under the approver-token
// metadata key as incoming metadata, the way a gRPC interceptor-less in-process
// call would see them. Zero values return ctx unchanged.
func withTokens(ctx context.Context, secrets ...string) context.Context {
	if len(secrets) == 0 {
		return ctx
	}
	md := metadata.MD{}
	for _, s := range secrets {
		md.Append(approval.TokenMetadataKey, s)
	}
	return metadata.NewIncomingContext(ctx, md)
}

func guardGet(t *testing.T, h *host.Host, kind approval.Kind, id string) *hostv1.Document {
	t.Helper()
	doc, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{Collection: kind.Collection, DocId: id})
	if err != nil {
		t.Fatalf("Get(%s/%s): %v", kind.Collection, id, err)
	}
	return doc
}

// guardPut writes body over kind/id with the given IfVersion through the
// pack-facing Documents facet.
func guardPut(ctx context.Context, t *testing.T, h *host.Host, kind approval.Kind, id string, body map[string]any, ifVersion int64) error {
	t.Helper()
	var j *hostv1.Json
	if body != nil {
		s, err := structpb.NewStruct(body)
		if err != nil {
			t.Fatalf("structpb.NewStruct: %v", err)
		}
		j = &hostv1.Json{Value: s}
	}
	_, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{Collection: kind.Collection, DocId: id, Body: j, IfVersion: ifVersion})
	return err
}

// guardUnchanged fails the test unless the stored document equals before.
func guardUnchanged(t *testing.T, h *host.Host, kind approval.Kind, id string, before *hostv1.Document) {
	t.Helper()
	after := guardGet(t, h, kind, id)
	if after.Version != before.Version || !proto.Equal(after.Body, before.Body) {
		t.Fatalf("%s/%s changed after a refused write: version %d -> %d", kind.Collection, id, before.Version, after.Version)
	}
}

// guardTransition builds the body a legitimate decision would write over doc:
// every existing entry, plus the governance fields for status.
func guardTransition(doc *hostv1.Document, kind approval.Kind, newStatus, decidedBy string) map[string]any {
	body := doc.Body.Value.AsMap()
	body["status"] = newStatus
	body["approved_scope"] = kind.ApproveScope
	body["decided_by"] = decidedBy
	body["decided_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if newStatus == approval.StatusRejected {
		body["reason"] = "not today"
	}
	return body
}

func TestDocumentsGuard_ForgedApprovalRefused(t *testing.T) {
	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newGuardHost()
			guardPropose(t, h, kc.kind, "p1")

			for _, newStatus := range []string{approval.StatusApproved, approval.StatusRejected} {
				for _, withProvenance := range []bool{false, true} {
					before := guardGet(t, h, kc.kind, "p1")
					body := before.Body.Value.AsMap()
					body["status"] = newStatus
					if withProvenance {
						body["approved_scope"] = kc.kind.ApproveScope
						body["decided_by"] = "forger"
					}
					err := guardPut(ctx, t, h, kc.kind, "p1", body, before.Version)
					if !local.IsApprovalTransitionRefused(err) {
						t.Fatalf("status %s provenance=%v: got %v, want IsApprovalTransitionRefused", newStatus, withProvenance, err)
					}
					if status.Code(err) != codes.PermissionDenied {
						t.Fatalf("status %s provenance=%v: code %v, want PermissionDenied", newStatus, withProvenance, status.Code(err))
					}
					guardUnchanged(t, h, kc.kind, "p1", before)
				}
			}

			// A proposal born approved is a forgery too.
			born := map[string]any{"status": approval.StatusApproved, "approved_scope": kc.kind.ApproveScope, "decided_by": "forger"}
			if err := guardPut(ctx, t, h, kc.kind, "born", born, 0); !local.IsApprovalTransitionRefused(err) {
				t.Fatalf("born-approved create: got %v, want IsApprovalTransitionRefused", err)
			}
			if _, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: kc.kind.Collection, DocId: "born"}); status.Code(err) != codes.NotFound {
				t.Fatalf("a refused born-approved create left a document behind: %v", err)
			}
		})
	}
}

func TestDocumentsGuard_PendingProposalsStayWritable(t *testing.T) {
	// D-04: creating and editing a pending proposal is an ordinary Put.
	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newGuardHost()
			if err := guardPut(ctx, t, h, kc.kind, "p1", map[string]any{"status": "pending", "payload": "one"}, 0); err != nil {
				t.Fatalf("create pending: %v", err)
			}
			doc := guardGet(t, h, kc.kind, "p1")
			if err := guardPut(ctx, t, h, kc.kind, "p1", map[string]any{"status": "pending", "payload": "two"}, doc.Version); err != nil {
				t.Fatalf("edit pending: %v", err)
			}
		})
	}
}

func TestDocumentsGuard_DecidedProposalIsImmutable(t *testing.T) {
	for _, kc := range guardKinds {
		for _, decision := range []string{approval.StatusApproved, approval.StatusRejected} {
			t.Run(kc.name+"/"+decision, func(t *testing.T) {
				ctx := context.Background()
				h := newGuardHost()
				secret := guardToken(t, h, kc.kind.ApproveScope, "ops-alice")
				guardPropose(t, h, kc.kind, "p1")
				if decision == approval.StatusApproved {
					if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: kc.kind, ProposalID: "p1", TokenSecret: secret}); err != nil {
						t.Fatalf("Approve: %v", err)
					}
				} else if _, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: kc.kind, ProposalID: "p1", TokenSecret: secret, Reason: "no"}); err != nil {
					t.Fatalf("Reject: %v", err)
				}

				before := guardGet(t, h, kc.kind, "p1")
				// Approved-to-approved swap: the governance fields copied, the payload not.
				swap := before.Body.Value.AsMap()
				swap["payload"] = map[string]any{"target": "elsewhere"}
				if err := guardPut(ctx, t, h, kc.kind, "p1", swap, before.Version); !local.IsApprovalProposalDecided(err) {
					t.Fatalf("payload swap over a decided proposal: got %v, want IsApprovalProposalDecided", err)
				}
				// Holding a valid token does not make a decided proposal editable.
				if err := guardPut(withTokens(ctx, secret), t, h, kc.kind, "p1", swap, before.Version); !local.IsApprovalProposalDecided(err) {
					t.Fatalf("payload swap with a valid token: got %v, want IsApprovalProposalDecided", err)
				}
				// Nor can it be taken back to pending.
				back := before.Body.Value.AsMap()
				back["status"] = approval.StatusPending
				if err := guardPut(withTokens(ctx, secret), t, h, kc.kind, "p1", back, before.Version); !local.IsApprovalProposalDecided(err) {
					t.Fatalf("decided to pending: got %v, want IsApprovalProposalDecided", err)
				}
				guardUnchanged(t, h, kc.kind, "p1", before)

				// Delete without a token, and with a token for another scope, is refused.
				del := &hostv1.DeleteDocumentRequest{Collection: kc.kind.Collection, DocId: "p1"}
				if _, err := h.Documents.Delete(ctx, del); !local.IsApprovalProposalDecided(err) {
					t.Fatalf("Delete without a token: got %v, want IsApprovalProposalDecided", err)
				}
				other := guardToken(t, h, "other:approve", "mallory")
				if _, err := h.Documents.Delete(withTokens(ctx, other), del); !local.IsApprovalProposalDecided(err) {
					t.Fatalf("Delete with another scope's token: got %v, want IsApprovalProposalDecided", err)
				}
				guardUnchanged(t, h, kc.kind, "p1", before)

				// A valid token for the Kind may delete it.
				if _, err := h.Documents.Delete(withTokens(ctx, secret), del); err != nil {
					t.Fatalf("Delete with a valid approver token: %v", err)
				}
				if _, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: kc.kind.Collection, DocId: "p1"}); status.Code(err) != codes.NotFound {
					t.Fatalf("proposal still present after an authorized Delete: %v", err)
				}
			})
		}

		t.Run(kc.name+"/pending delete needs no token", func(t *testing.T) {
			h := newGuardHost()
			guardPropose(t, h, kc.kind, "p2")
			if _, err := h.Documents.Delete(context.Background(), &hostv1.DeleteDocumentRequest{Collection: kc.kind.Collection, DocId: "p2"}); err != nil {
				t.Fatalf("Delete of a pending proposal: %v", err)
			}
		})
	}
}

func TestDocumentsGuard_WrongScopeTokenRefused(t *testing.T) {
	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newGuardHost()
			guardPropose(t, h, kc.kind, "p1")
			before := guardGet(t, h, kc.kind, "p1")

			wrongScope := guardToken(t, h, "other:approve", "mallory")
			expired, err := h.Auth.IssueToken(ctx, &hostv1.IssueTokenRequest{Scope: kc.kind.ApproveScope, Label: "ops-alice", TtlSeconds: 0})
			if err != nil {
				t.Fatalf("IssueToken: %v", err)
			}
			time.Sleep(5 * time.Millisecond)

			// The body is a correct transition for the label "ops-alice", so only
			// the token can be what is refused.
			body := guardTransition(before, kc.kind, approval.StatusApproved, "ops-alice")
			for name, secret := range map[string]string{
				"a token for another scope": wrongScope,
				"an expired token":          expired.Secret,
				"an unknown token":          "not-a-real-secret",
			} {
				if err := guardPut(withTokens(ctx, secret), t, h, kc.kind, "p1", body, before.Version); !local.IsApprovalTransitionRefused(err) {
					t.Fatalf("%s: got %v, want IsApprovalTransitionRefused", name, err)
				}
				guardUnchanged(t, h, kc.kind, "p1", before)
			}

			// Control: the same body with a valid token is accepted, so the refusals
			// above were about the token and nothing else.
			good := guardToken(t, h, kc.kind.ApproveScope, "ops-alice")
			if err := guardPut(withTokens(ctx, good), t, h, kc.kind, "p1", body, before.Version); err != nil {
				t.Fatalf("control transition with a valid token: %v", err)
			}
		})
	}
}

func TestDocumentsGuard_TransitionPayloadFrozen(t *testing.T) {
	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newGuardHost()
			secret := guardToken(t, h, kc.kind.ApproveScope, "ops-alice")
			guardPropose(t, h, kc.kind, "p1")
			before := guardGet(t, h, kc.kind, "p1")
			tctx := withTokens(ctx, secret)

			cases := map[string]func(map[string]any){
				"a changed payload entry":     func(m map[string]any) { m["payload"] = map[string]any{"target": "elsewhere"} },
				"an added entry":              func(m map[string]any) { m["extra"] = "smuggled" },
				"a removed entry":             func(m map[string]any) { delete(m, "payload") },
				"a decided_by other than the": func(m map[string]any) { m["decided_by"] = "somebody-else" },
				"a forged approved_scope":     func(m map[string]any) { m["approved_scope"] = "anything:else" },
				"a missing approved_scope":    func(m map[string]any) { delete(m, "approved_scope") },
				"a missing decided_by":        func(m map[string]any) { delete(m, "decided_by") },
			}
			for name, mutate := range cases {
				body := guardTransition(before, kc.kind, approval.StatusApproved, "ops-alice")
				mutate(body)
				if err := guardPut(tctx, t, h, kc.kind, "p1", body, before.Version); !local.IsApprovalTransitionRefused(err) {
					t.Fatalf("%s: got %v, want IsApprovalTransitionRefused", name, err)
				}
				guardUnchanged(t, h, kc.kind, "p1", before)
			}

			// Control: the exact transition is accepted.
			if err := guardPut(tctx, t, h, kc.kind, "p1", guardTransition(before, kc.kind, approval.StatusApproved, "ops-alice"), before.Version); err != nil {
				t.Fatalf("control transition: %v", err)
			}
		})
	}
}

func TestDocumentsGuard_TokenMetadataCardinality(t *testing.T) {
	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newGuardHost()
			a := guardToken(t, h, kc.kind.ApproveScope, "ops-alice")
			b := guardToken(t, h, kc.kind.ApproveScope, "ops-alice")
			guardPropose(t, h, kc.kind, "p1")
			before := guardGet(t, h, kc.kind, "p1")
			body := guardTransition(before, kc.kind, approval.StatusApproved, "ops-alice")

			for name, c := range map[string]context.Context{
				"zero values":              withTokens(ctx),
				"two different values":     withTokens(ctx, a, b),
				"the same value repeated":  withTokens(ctx, a, a),
				"an empty single value":    withTokens(ctx, ""),
				"a valid and a junk value": withTokens(ctx, a, "junk"),
			} {
				if err := guardPut(c, t, h, kc.kind, "p1", body, before.Version); !local.IsApprovalTransitionRefused(err) {
					t.Fatalf("%s: got %v, want IsApprovalTransitionRefused", name, err)
				}
				guardUnchanged(t, h, kc.kind, "p1", before)
			}

			// Control: exactly one value is accepted.
			if err := guardPut(withTokens(ctx, a), t, h, kc.kind, "p1", body, before.Version); err != nil {
				t.Fatalf("control with one value: %v", err)
			}
		})
	}
}

func TestDocumentsGuard_EmptyAndNullBodies(t *testing.T) {
	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newGuardHost()
			secret := guardToken(t, h, kc.kind.ApproveScope, "ops-alice")
			tctx := withTokens(ctx, secret)
			guardPropose(t, h, kc.kind, "p1")
			before := guardGet(t, h, kc.kind, "p1")

			bodies := map[string]map[string]any{
				"a nil body":                    nil,
				"an empty body":                 {},
				"a non-string status":           {"status": float64(7), "payload": "x"},
				"a body with no status":         {"payload": "x"},
				"a pending body with a decider": {"status": "pending", "decided_by": "forger"},
				"a pending body with a scope":   {"status": "pending", "approved_scope": kc.kind.ApproveScope},
				"an unknown status":             {"status": "escalated"},
			}
			for name, body := range bodies {
				// Over an existing pending proposal, with and without a token.
				for _, c := range []context.Context{ctx, tctx} {
					if err := guardPut(c, t, h, kc.kind, "p1", body, before.Version); !local.IsApprovalTransitionRefused(err) {
						t.Fatalf("%s over a pending proposal: got %v, want IsApprovalTransitionRefused", name, err)
					}
				}
				guardUnchanged(t, h, kc.kind, "p1", before)
				// As a create.
				if err := guardPut(ctx, t, h, kc.kind, "fresh", body, 0); !local.IsApprovalTransitionRefused(err) {
					t.Fatalf("%s as a create: got %v, want IsApprovalTransitionRefused", name, err)
				}
			}

			// Delete of a proposal that does not exist is still NOT_FOUND, not a refusal.
			_, err := h.Documents.Delete(ctx, &hostv1.DeleteDocumentRequest{Collection: kc.kind.Collection, DocId: "ghost"})
			if status.Code(err) != codes.NotFound || local.IsApprovalProposalDecided(err) {
				t.Fatalf("Delete of a missing proposal: got %v, want NOT_FOUND", err)
			}
		})
	}
}

func TestDocumentsGuard_ConcurrentDecideOneWinner(t *testing.T) {
	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newGuardHost()
			guardPropose(t, h, kc.kind, "race")

			const n = 16
			secrets := make([]string, n)
			for i := range secrets {
				secrets[i] = guardToken(t, h, kc.kind.ApproveScope, fmt.Sprintf("racer-%d", i))
			}
			errs := make([]error, n)
			gate := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(n)
			for i := 0; i < n; i++ {
				go func(i int) {
					defer wg.Done()
					<-gate
					if i%2 == 0 {
						_, errs[i] = approval.Approve(ctx, h, approval.ApproveRequest{Kind: kc.kind, ProposalID: "race", TokenSecret: secrets[i]})
					} else {
						_, errs[i] = approval.Reject(ctx, h, approval.RejectRequest{Kind: kc.kind, ProposalID: "race", TokenSecret: secrets[i], Reason: "racing"})
					}
				}(i)
			}
			close(gate)
			wg.Wait()

			wins := 0
			for i, err := range errs {
				switch {
				case err == nil:
					wins++
				case local.IsApprovalTransitionRefused(err) || local.IsApprovalProposalDecided(err):
					t.Fatalf("goroutine %d: a loser saw a guard refusal instead of already-decided: %v", i, err)
				case !approval.IsAlreadyDecided(err):
					t.Fatalf("goroutine %d: got %v, want approval.IsAlreadyDecided", i, err)
				}
			}
			if wins != 1 {
				t.Fatalf("got %d winners, want exactly 1", wins)
			}
			if got := guardGet(t, h, kc.kind, "race").Version; got != 2 {
				t.Fatalf("stored version %d, want 2 (exactly one decision landed)", got)
			}
		})
	}
}

// refusalText flattens everything a caller can read out of err: the message and
// every ErrorDetail field.
func refusalText(err error) string {
	st := status.Convert(err)
	var b strings.Builder
	b.WriteString(err.Error())
	b.WriteString(st.Message())
	for _, d := range st.Details() {
		if ed, ok := d.(*hostv1.ErrorDetail); ok {
			b.WriteString(ed.Code + ed.Message + ed.Fix)
		}
	}
	return b.String()
}

func TestDocumentsGuard_NoTokenInErrors(t *testing.T) {
	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newGuardHost()
			good := guardToken(t, h, kc.kind.ApproveScope, "ops-alice")
			wrongScope := guardToken(t, h, "other:approve", "mallory")
			expired, err := h.Auth.IssueToken(ctx, &hostv1.IssueTokenRequest{Scope: kc.kind.ApproveScope, Label: "ops-alice", TtlSeconds: 0})
			if err != nil {
				t.Fatalf("IssueToken: %v", err)
			}
			time.Sleep(5 * time.Millisecond)
			const unknown = "unknown-secret-0123456789abcdef"

			guardPropose(t, h, kc.kind, "p1")
			before := guardGet(t, h, kc.kind, "p1")
			transition := guardTransition(before, kc.kind, approval.StatusApproved, "ops-alice")

			type refusal struct {
				name    string
				err     error
				secrets []string
			}
			var refusals []refusal
			put := func(name string, c context.Context, body map[string]any, secrets ...string) {
				refusals = append(refusals, refusal{name, guardPut(c, t, h, kc.kind, "p1", body, before.Version), secrets})
			}
			put("no token", ctx, transition)
			put("wrong scope", withTokens(ctx, wrongScope), transition, wrongScope)
			put("expired", withTokens(ctx, expired.Secret), transition, expired.Secret)
			put("unknown", withTokens(ctx, unknown), transition, unknown)
			put("two values", withTokens(ctx, good, unknown), transition, good, unknown)
			put("frozen payload", withTokens(ctx, good), map[string]any{"status": "approved", "approved_scope": kc.kind.ApproveScope, "decided_by": "ops-alice"}, good)

			// Decided proposal: Put and Delete refusals while a token is on the wire.
			if err := guardPut(withTokens(ctx, good), t, h, kc.kind, "p1", transition, before.Version); err != nil {
				t.Fatalf("control transition: %v", err)
			}
			decided := guardGet(t, h, kc.kind, "p1")
			refusals = append(refusals, refusal{"put over decided", guardPut(withTokens(ctx, wrongScope), t, h, kc.kind, "p1", transition, decided.Version), []string{wrongScope}})
			_, derr := h.Documents.Delete(withTokens(ctx, wrongScope), &hostv1.DeleteDocumentRequest{Collection: kc.kind.Collection, DocId: "p1"})
			refusals = append(refusals, refusal{"delete decided", derr, []string{wrongScope}})

			for _, r := range refusals {
				if r.err == nil {
					t.Fatalf("%s: expected a refusal, got nil", r.name)
				}
				text := refusalText(r.err)
				for _, secret := range append(r.secrets, good, wrongScope, expired.Secret, unknown) {
					if strings.Contains(text, secret) {
						t.Fatalf("%s: the refusal text contains a token secret", r.name)
					}
				}
			}
		})
	}
}

// guardDocumentsClient adapts a Documents gRPC client to the DocumentsServer
// interface host.Host expects, passing ctx through so outgoing metadata set by
// approval.WithApproverToken travels to the server as incoming metadata.
type guardDocumentsClient struct {
	hostv1.UnimplementedDocumentsServer
	c hostv1.DocumentsClient
}

func (a *guardDocumentsClient) Get(ctx context.Context, r *hostv1.GetDocumentRequest) (*hostv1.Document, error) {
	return a.c.Get(ctx, r)
}
func (a *guardDocumentsClient) Put(ctx context.Context, r *hostv1.PutDocumentRequest) (*hostv1.PutDocumentResponse, error) {
	return a.c.Put(ctx, r)
}
func (a *guardDocumentsClient) Delete(ctx context.Context, r *hostv1.DeleteDocumentRequest) (*emptypb.Empty, error) {
	return a.c.Delete(ctx, r)
}
func (a *guardDocumentsClient) List(ctx context.Context, r *hostv1.ListDocumentsRequest) (*hostv1.ListDocumentsResponse, error) {
	return a.c.List(ctx, r)
}
func (a *guardDocumentsClient) Query(ctx context.Context, r *hostv1.QueryDocumentsRequest) (*hostv1.ListDocumentsResponse, error) {
	return a.c.Query(ctx, r)
}

func TestDocumentsGuard_OverGRPC(t *testing.T) {
	ctx := context.Background()
	h := newGuardHost()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	hostv1.RegisterDocumentsServer(srv, h.Documents)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The pack's view of the host: Documents over the wire, Auth in process.
	wire := &host.Host{Documents: &guardDocumentsClient{c: hostv1.NewDocumentsClient(conn)}, Auth: h.Auth}

	for _, kc := range guardKinds {
		t.Run(kc.name, func(t *testing.T) {
			secret := guardToken(t, h, kc.kind.ApproveScope, "ops-alice")
			if _, err := approval.ProposeBody(ctx, wire, kc.kind, "g1", map[string]any{"payload": "x"}); err != nil {
				t.Fatalf("ProposeBody over gRPC: %v", err)
			}

			// A client Put of approved with no metadata is refused, and the
			// ErrorDetail code survives the wire.
			doc, err := wire.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: kc.kind.Collection, DocId: "g1"})
			if err != nil {
				t.Fatalf("Get over gRPC: %v", err)
			}
			forged := doc.Body.Value.AsMap()
			forged["status"] = approval.StatusApproved
			s, _ := structpb.NewStruct(forged)
			_, err = wire.Documents.Put(ctx, &hostv1.PutDocumentRequest{Collection: kc.kind.Collection, DocId: "g1", Body: &hostv1.Json{Value: s}, IfVersion: doc.Version})
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("forged Put over gRPC: code %v (%v), want PermissionDenied", status.Code(err), err)
			}
			var detail *hostv1.ErrorDetail
			for _, d := range status.Convert(err).Details() {
				if ed, ok := d.(*hostv1.ErrorDetail); ok {
					detail = ed
				}
			}
			if detail == nil || detail.Code != "approval_transition_requires_token" {
				t.Fatalf("forged Put over gRPC: ErrorDetail %v, want code approval_transition_requires_token", detail)
			}
			if !local.IsApprovalTransitionRefused(err) {
				t.Fatalf("IsApprovalTransitionRefused did not recognise the wire error: %v", err)
			}

			// approval.Approve through the same client carries the token in
			// metadata and decides the proposal.
			if _, err := approval.Approve(ctx, wire, approval.ApproveRequest{Kind: kc.kind, ProposalID: "g1", TokenSecret: secret}); err != nil {
				t.Fatalf("Approve over gRPC: %v", err)
			}
			got, err := approval.Get(ctx, wire, kc.kind, "g1")
			if err != nil || got.Status != approval.StatusApproved || got.DecidedBy != "ops-alice" {
				t.Fatalf("after Approve over gRPC: %+v, %v", got, err)
			}

			// And the decided proposal is immutable over the wire too.
			decided, _ := wire.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: kc.kind.Collection, DocId: "g1"})
			swap := decided.Body.Value.AsMap()
			swap["payload"] = "swapped"
			s, _ = structpb.NewStruct(swap)
			_, err = wire.Documents.Put(ctx, &hostv1.PutDocumentRequest{Collection: kc.kind.Collection, DocId: "g1", Body: &hostv1.Json{Value: s}, IfVersion: decided.Version})
			if !local.IsApprovalProposalDecided(err) {
				t.Fatalf("payload swap over gRPC: got %v, want IsApprovalProposalDecided", err)
			}
		})
	}
}

func TestSeedDocumentIsTestAndOperatorOnly(t *testing.T) {
	// Structural: SeedDocument is not reachable through the pack-facing surface.
	if _, ok := reflect.TypeOf((*hostv1.DocumentsServer)(nil)).Elem().MethodByName("SeedDocument"); ok {
		t.Fatal("SeedDocument is a method of the pack-facing Documents interface")
	}
	if _, ok := reflect.TypeOf(host.Host{}).FieldByName("SeedDocument"); ok {
		t.Fatal("SeedDocument is a field of host.Host")
	}
	if _, ok := reflect.TypeOf(&host.Host{}).MethodByName("SeedDocument"); ok {
		t.Fatal("SeedDocument is a method of host.Host")
	}

	// Textual: no non-test production file other than seed.go calls it.
	root := filepath.Join("..", "..")
	parsed, sawSeed := 0, false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".planning", ".gsd", ".claude", "gen", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		parsed++
		isSeed := filepath.ToSlash(rel) == "host/local/seed.go"
		sawSeed = sawSeed || isSeed
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if name == "SeedDocument" && !isSeed {
				t.Errorf("%s calls SeedDocument; it is an operator/test seam and production pack code must never call it", filepath.ToSlash(rel))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
	// Liveness: a walk that saw nothing proves nothing.
	if parsed < 10 || !sawSeed {
		t.Fatalf("the walk parsed %d production files and saw seed.go=%v; the confinement check did not run", parsed, sawSeed)
	}
}

// ---- Reserved collections (FND-03, D-05, D-06) ----------------------------
//
// The expected names below are written out by hand on purpose: deriving them
// from the production list would let a dropped entry go unnoticed.

// reservedPut and reservedDelete write through the pack-facing Documents facet.
func reservedPut(h *host.Host, collection, docID string, ifVersion int64) error {
	body, _ := structpb.NewStruct(map[string]any{"pack": "wrote this"})
	_, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: collection, DocId: docID, Body: &hostv1.Json{Value: body}, IfVersion: ifVersion,
	})
	return err
}

func reservedDelete(h *host.Host, collection, docID string) error {
	_, err := h.Documents.Delete(context.Background(), &hostv1.DeleteDocumentRequest{Collection: collection, DocId: docID})
	return err
}

// requireReserved fails unless err is a collection_reserved refusal that names
// the collection and carries a non-empty fix line.
func requireReserved(t *testing.T, err error, collection, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s on %s: want a collection_reserved refusal, got success", op, collection)
	}
	if !local.IsCollectionReserved(err) {
		t.Fatalf("%s on %s: want IsCollectionReserved, got %v", op, collection, err)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("%s on %s: code = %v, want PermissionDenied", op, collection, status.Code(err))
	}
	if !strings.Contains(err.Error(), collection) {
		t.Fatalf("%s on %s: refusal %q does not name the collection", op, collection, err)
	}
	var ed *hostv1.ErrorDetail
	for _, d := range status.Convert(err).Details() {
		if x, ok := d.(*hostv1.ErrorDetail); ok {
			ed = x
		}
	}
	if ed == nil || ed.Code != "collection_reserved" || ed.Fix == "" {
		t.Fatalf("%s on %s: ErrorDetail = %+v, want code collection_reserved and a non-empty fix", op, collection, ed)
	}
	if !strings.Contains(ed.Fix, "forge-sources") || !strings.Contains(ed.Fix, "llm-providers") {
		t.Fatalf("%s on %s: fix %q does not say which names pack collections must avoid", op, collection, ed.Fix)
	}
}

func TestDocumentsGuard_ReservedCollections(t *testing.T) {
	for _, collection := range []string{
		"code-environments", "code-puppetfiles", "code-hiera-hierarchy", "code-hiera-data",
		"code-overwrite-applied", "code-anything",
		"deploy-runs", "deploy-proposals",
		"bolt-runs",
		"inventory-nodes",
	} {
		t.Run(collection, func(t *testing.T) {
			h := newGuardHost()
			if err := local.SeedDocument(h, collection, "doc", map[string]any{"seeded": "yes"}); err != nil {
				t.Fatalf("SeedDocument: %v", err)
			}
			before, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{Collection: collection, DocId: "doc"})
			if err != nil {
				t.Fatalf("Get seeded doc: %v", err)
			}

			requireReserved(t, reservedPut(h, collection, "fresh", 0), collection, "create Put")
			requireReserved(t, reservedPut(h, collection, "doc", before.Version), collection, "update Put")
			requireReserved(t, reservedDelete(h, collection, "doc"), collection, "Delete")
			// A refused write must not even create the collection's new doc.
			if _, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{Collection: collection, DocId: "fresh"}); status.Code(err) != codes.NotFound {
				t.Fatalf("refused create left a document behind: %v", err)
			}
			after, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{Collection: collection, DocId: "doc"})
			if err != nil {
				t.Fatalf("seeded doc gone after refused Delete: %v", err)
			}
			if after.Version != before.Version || !proto.Equal(after.Body, before.Body) {
				t.Fatalf("seeded doc changed after refused writes: version %d -> %d", before.Version, after.Version)
			}
		})
	}
}

func TestDocumentsGuard_ReservedReadsAllowed(t *testing.T) {
	h := newGuardHost()
	ctx := context.Background()
	for _, collection := range []string{"code-puppetfiles", "deploy-runs", "bolt-runs", "inventory-nodes"} {
		if err := local.SeedDocument(h, collection, "doc", map[string]any{"k": "v"}); err != nil {
			t.Fatalf("SeedDocument(%s): %v", collection, err)
		}
		if _, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: collection, DocId: "doc"}); err != nil {
			t.Errorf("Get on %s: %v", collection, err)
		}
		list, err := h.Documents.List(ctx, &hostv1.ListDocumentsRequest{Collection: collection})
		if err != nil || len(list.Documents) != 1 {
			t.Errorf("List on %s: %v, %d documents, want 1", collection, err, len(list.GetDocuments()))
		}
		val, _ := structpb.NewValue("v")
		q, err := h.Documents.Query(ctx, &hostv1.QueryDocumentsRequest{
			Collection: collection, Field: "k", Op: hostv1.QueryDocumentsRequest_EQ, Value: &hostv1.Json{Value: &structpb.Struct{Fields: map[string]*structpb.Value{"v": val}}},
		})
		if err != nil {
			t.Errorf("Query on %s: %v", collection, err)
			_ = q
		}
	}
}

func TestDocumentsGuard_FacetsUnaffected(t *testing.T) {
	h := newGuardHost()
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment with the guard on: %v", err)
	}
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
		Environment: "prod", Module: forgeModule("puppetlabs/apache", "5.0.0"),
	}); err != nil {
		t.Fatalf("PutPuppetfileModule with the guard on: %v", err)
	}

	// Propose, approve, apply an overwrite: the proposal write is a registered
	// Kind write and the code-overwrite-applied marker is a facet write.
	overwriteViaApproval(t, h, "apache-6", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
	if _, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: "code-overwrite-applied", DocId: "apache-6"}); err != nil {
		t.Fatalf("the facet did not write its code-overwrite-applied marker: %v", err)
	}

	node := &hostv1.Node{Id: "web01.example.test", DisplayName: "web01", Status: hostv1.Node_DISCOVERED}
	if _, err := approval.Propose(ctx, h, node, approval.Kind{Collection: "inventory-proposals", ApproveScope: "inventory:approve"}); err != nil {
		t.Fatalf("approval.Propose into inventory-proposals with the guard on: %v", err)
	}
}

func TestDocumentsGuard_ReservedNameAdjacency(t *testing.T) {
	h := newGuardHost()
	for _, name := range []string{"CODE-ENVIRONMENTS", "Code-x", "code-", "deploy-", "Bolt-runs"} {
		requireReserved(t, reservedPut(h, name, "doc", 0), name, "Put")
		if err := reservedDelete(h, name, "doc"); !local.IsCollectionReserved(err) {
			t.Errorf("Delete on %s: want collection_reserved, got %v", name, err)
		}
	}
	for _, name := range []string{"code", "codex-notes", "my-code-notes", "deployments", "bolts", "state", "locks", "state-versions", "pack-private-state"} {
		if err := reservedPut(h, name, "doc", 0); err != nil {
			t.Errorf("Put on %s must stay writable: %v", name, err)
			continue
		}
		if err := reservedDelete(h, name, "doc"); err != nil {
			t.Errorf("Delete on %s must stay possible: %v", name, err)
		}
	}
}
