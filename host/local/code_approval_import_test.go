package local

// Internal tests for the two import-scoped changes in code_approval.go: the
// describeOverwriteTarget import arm and the import-only skip of the
// environment-name check in resolveApplyProposalLocked. They live in package
// local, not local_test, because resolveApplyProposalLocked is unexported and
// there is no ApplyImport RPC yet to reach it through (10-06 owns that).

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

var importTestKind = approval.Kind{
	Collection:   code.OverwriteCollection,
	ApproveScope: code.OverwriteApproveScope,
}

func importTestHost(t *testing.T) (*host.Host, *codeServer) {
	t.Helper()
	h := New([]string{"code:rw", "tokens:issue"}, "controlrepo")
	g, ok := h.Code.(*gatedCode)
	if !ok {
		t.Fatalf("host Code facet is %T, want *gatedCode", h.Code)
	}
	return h, g.inner
}

func importTestSnapshot() *hostv1.ImportSnapshot {
	return &hostv1.ImportSnapshot{
		Url: "https://git.example.com/org/control-repo.git",
		Branches: []*hostv1.ImportBranchSnapshot{
			{Branch: "staging", Commit: "aaaa", Importable: true},
			{Branch: "production", Commit: "bbbb", Importable: true},
		},
	}
}

func importTestPropose(t *testing.T, h *host.Host, id string, body map[string]any) {
	t.Helper()
	if _, err := approval.ProposeBody(context.Background(), h, importTestKind, id, body); err != nil {
		t.Fatalf("ProposeBody(%s): %v", id, err)
	}
}

func importTestApprove(t *testing.T, h *host.Host, kind approval.Kind, id string) {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{Scope: kind.ApproveScope, Label: "operator", TtlSeconds: 300})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if _, err := approval.Approve(context.Background(), h, approval.ApproveRequest{Kind: kind, ProposalID: id, TokenSecret: tok.Secret}); err != nil {
		t.Fatalf("Approve(%s): %v", id, err)
	}
}

func importTestResolve(s *codeServer, id, want string) (map[string]any, code.OverwriteTarget, error) {
	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()
	return s.resolveApplyProposalLocked(id, want)
}

func importBody(t *testing.T) map[string]any {
	t.Helper()
	body, err := code.OverwriteBodyForImport(importTestSnapshot())
	if err != nil {
		t.Fatalf("OverwriteBodyForImport: %v", err)
	}
	return body
}

func TestResolveApplyProposalImportSkipsEnvNameOnly(t *testing.T) {
	t.Run("approved import with an empty environment resolves", func(t *testing.T) {
		h, s := importTestHost(t)
		importTestPropose(t, h, "imp-1", importBody(t))
		importTestApprove(t, h, importTestKind, "imp-1")

		body, target, err := importTestResolve(s, "imp-1", code.OverwriteResourceImport)
		if err != nil {
			t.Fatalf("got %v, want success", err)
		}
		if target.Environment != "" || target.Resource != code.OverwriteResourceImport || target.Name != "production,staging" {
			t.Fatalf("unexpected target %+v", target)
		}
		if _, err := code.OverwritePayloadImport(body); err != nil {
			t.Fatalf("resolved body does not decode as an import snapshot: %v", err)
		}
	})

	t.Run("pending import is refused as not approved", func(t *testing.T) {
		h, s := importTestHost(t)
		importTestPropose(t, h, "imp-pending", importBody(t))

		_, _, err := importTestResolve(s, "imp-pending", code.OverwriteResourceImport)
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not approved") {
			t.Fatalf("got %v, want FailedPrecondition 'not approved'", err)
		}
	})

	t.Run("approved import without approval provenance is refused", func(t *testing.T) {
		h, s := importTestHost(t)
		importTestPropose(t, h, "imp-noprov", importBody(t))
		// Decided under a different scope: status becomes approved but the
		// recorded scope is not code:approve.
		importTestApprove(t, h, approval.Kind{Collection: code.OverwriteCollection, ApproveScope: "other:approve"}, "imp-noprov")

		_, _, err := importTestResolve(s, "imp-noprov", code.OverwriteResourceImport)
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "no record of a decision") {
			t.Fatalf("got %v, want FailedPrecondition about missing decision record", err)
		}
	})

	t.Run("settings with an invalid environment name is still refused", func(t *testing.T) {
		h, s := importTestHost(t)
		body, err := code.OverwriteBodyForSettings(&hostv1.EnvironmentSettings{Environment: "Bad-Name"})
		if err != nil {
			t.Fatal(err)
		}
		importTestPropose(t, h, "set-bad", body)
		importTestApprove(t, h, importTestKind, "set-bad")

		_, _, err = importTestResolve(s, "set-bad", code.OverwriteResourceSettings)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "environment name") {
			t.Fatalf("got %v, want InvalidArgument from validateEnvName", err)
		}
	})

	t.Run("import wanted but proposal targets settings is a resource mismatch", func(t *testing.T) {
		h, s := importTestHost(t)
		body, err := code.OverwriteBodyForSettings(&hostv1.EnvironmentSettings{Environment: "prod"})
		if err != nil {
			t.Fatal(err)
		}
		importTestPropose(t, h, "set-ok", body)
		importTestApprove(t, h, importTestKind, "set-ok")

		_, _, err = importTestResolve(s, "set-ok", code.OverwriteResourceImport)
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "use the matching Apply RPC") {
			t.Fatalf("got %v, want FailedPrecondition resource mismatch", err)
		}
		if strings.Contains(err.Error(), "environment name") {
			t.Fatalf("mismatch was reported as an environment error: %v", err)
		}
	})

	t.Run("unknown proposal id is the store's NotFound for both kinds", func(t *testing.T) {
		_, s := importTestHost(t)
		for _, want := range []string{code.OverwriteResourceImport, code.OverwriteResourceSettings} {
			_, _, err := importTestResolve(s, "no-such-proposal", want)
			if status.Code(err) != codes.NotFound {
				t.Fatalf("%s: got %v, want NotFound", want, err)
			}
		}
	})
}

func TestDescribeOverwriteTargetCoversImport(t *testing.T) {
	got := describeOverwriteTarget(code.OverwriteTarget{
		Resource: code.OverwriteResourceImport,
		Source:   "https://git.example.com/org/control-repo.git",
		Name:     "production,staging",
	})
	for _, want := range []string{"https://git.example.com/org/control-repo.git", "production,staging", "import"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q should mention %q", got, want)
		}
	}
	if strings.Contains(got, "already has") {
		t.Errorf("description %q claims an environment already has an import", got)
	}
}
