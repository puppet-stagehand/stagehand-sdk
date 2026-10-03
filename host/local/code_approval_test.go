package local_test

// End-to-end tests for the Code facet's overwrite-approval gate, exercised on
// the Puppetfile-module resource type: propose into the shared
// code-overwrites collection, approve with a code:approve token, watch
// PutPuppetfileModule refuse, and materialize through
// ApplyPuppetfileModuleOverwrite.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

// overwriteKind is composed at the call site, exactly as a pack would: the
// code package defines the two strings and never imports approval.
var overwriteKind = approval.Kind{
	Collection:   code.OverwriteCollection,
	ApproveScope: code.OverwriteApproveScope,
}

// newOverwriteHost builds a host that can use the Code facet and the Auth
// facet, which approval.Approve needs to verify a token.
func newOverwriteHost() *host.Host {
	return local.New([]string{"code:rw", "tokens:issue"}, "controlrepo")
}

func forgeModule(name, version string) *hostv1.PuppetfileModule {
	return &hostv1.PuppetfileModule{
		Name:   name,
		Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: version}},
	}
}

func putModuleErr(h *host.Host, env string, m *hostv1.PuppetfileModule) error {
	_, err := h.Code.PutPuppetfileModule(context.Background(), &hostv1.PutPuppetfileModuleRequest{Environment: env, Module: m})
	return err
}

func mustPutModule(t *testing.T, h *host.Host, env string, m *hostv1.PuppetfileModule) {
	t.Helper()
	if err := putModuleErr(h, env, m); err != nil {
		t.Fatalf("PutPuppetfileModule(%s): %v", m.GetName(), err)
	}
}

func mustCreateEnv(t *testing.T, h *host.Host, name string) {
	t.Helper()
	if _, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: name}); err != nil {
		t.Fatalf("CreateEnvironment(%s): %v", name, err)
	}
}

// proposeOverwrite writes a pending overwrite proposal for module m in env.
func proposeOverwrite(t *testing.T, h *host.Host, proposalID, env string, m *hostv1.PuppetfileModule) {
	t.Helper()
	body, err := code.OverwriteBodyForPuppetfileModule(env, m)
	if err != nil {
		t.Fatalf("OverwriteBodyForPuppetfileModule: %v", err)
	}
	if _, err := approval.ProposeBody(context.Background(), h, overwriteKind, proposalID, body); err != nil {
		t.Fatalf("ProposeBody(%s): %v", proposalID, err)
	}
}

// approverSecret mints a token carrying the overwrite approval scope. This
// is the operator's path; nothing in the code facet can reach it.
func approverSecret(t *testing.T, h *host.Host) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{
		Scope:      code.OverwriteApproveScope,
		Label:      "operator",
		TtlSeconds: 300,
	})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	return tok.Secret
}

func approveOverwrite(t *testing.T, h *host.Host, proposalID string) {
	t.Helper()
	if _, err := approval.Approve(context.Background(), h, approval.ApproveRequest{
		Kind: overwriteKind, ProposalID: proposalID, TokenSecret: approverSecret(t, h),
	}); err != nil {
		t.Fatalf("Approve(%s): %v", proposalID, err)
	}
}

func rejectOverwrite(t *testing.T, h *host.Host, proposalID string) {
	t.Helper()
	if _, err := approval.Reject(context.Background(), h, approval.RejectRequest{
		Kind: overwriteKind, ProposalID: proposalID, TokenSecret: approverSecret(t, h), Reason: "not wanted",
	}); err != nil {
		t.Fatalf("Reject(%s): %v", proposalID, err)
	}
}

// overwriteViaApproval runs the whole sanctioned path for one module:
// propose, approve, Apply. Other test files use it to edit a module that
// already exists.
func overwriteViaApproval(t *testing.T, h *host.Host, proposalID, env string, m *hostv1.PuppetfileModule) {
	t.Helper()
	proposeOverwrite(t, h, proposalID, env, m)
	approveOverwrite(t, h, proposalID)
	if _, err := h.Code.ApplyPuppetfileModuleOverwrite(context.Background(), &hostv1.ApplyPuppetfileModuleOverwriteRequest{ProposalId: proposalID}); err != nil {
		t.Fatalf("ApplyPuppetfileModuleOverwrite(%s): %v", proposalID, err)
	}
}

func applyModule(h *host.Host, proposalID string) (*hostv1.PuppetfileModule, error) {
	return h.Code.ApplyPuppetfileModuleOverwrite(context.Background(), &hostv1.ApplyPuppetfileModuleOverwriteRequest{ProposalId: proposalID})
}

// errorDetail returns the first ErrorDetail on err, or nil.
func errorDetail(err error) *hostv1.ErrorDetail {
	for _, d := range status.Convert(err).Details() {
		if ed, ok := d.(*hostv1.ErrorDetail); ok {
			return ed
		}
	}
	return nil
}

func listModuleNames(t *testing.T, h *host.Host, env string) []string {
	t.Helper()
	resp, err := h.Code.ListPuppetfileModules(context.Background(), &hostv1.ListPuppetfileModulesRequest{Environment: env})
	if err != nil {
		t.Fatalf("ListPuppetfileModules: %v", err)
	}
	names := make([]string, len(resp.Modules))
	for i, m := range resp.Modules {
		names[i] = m.GetName()
	}
	return names
}

func TestCodeOverwriteTracer_PuppetfileModule(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()
	mustCreateEnv(t, h, "prod")

	mustPutModule(t, h, "prod", forgeModule("puppetlabs/ntp", ""))
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/stdlib", ""))
	before := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod")

	// Adding a module that does not yet exist never needed a proposal, and
	// still does not. Overwrite is decided per module, not per environment.
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/concat", ""))
	mustRemove := func() {
		if _, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "prod", Name: "puppetlabs/concat"}); err != nil {
			t.Fatalf("RemovePuppetfileModule(concat): %v", err)
		}
	}
	mustRemove()
	if got := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod"); got != before {
		t.Fatalf("append then remove of a new module changed the stored text:\nbefore: %q\nafter:  %q", before, got)
	}

	replacement := forgeModule("puppetlabs/apache", "6.1.0")

	// No proposal yet: the silent overwrite is refused and the text is intact.
	if err := putModuleErr(h, "prod", replacement); !local.IsCodeOverwriteRequiresApproval(err) {
		t.Fatalf("Put over existing module with no proposal: got %v, want requires-approval", err)
	}

	proposeOverwrite(t, h, "apache-6", "prod", replacement)
	approveOverwrite(t, h, "apache-6")

	// Approved, but a Put never materializes an overwrite.
	err := putModuleErr(h, "prod", replacement)
	if !local.IsCodeOverwriteApplyPending(err) {
		t.Fatalf("Put after approval: got %v, want apply-pending", err)
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("apply-pending code = %v, want FailedPrecondition", status.Code(err))
	}
	if ed := errorDetail(err); ed == nil || !strings.Contains(ed.Fix, "apache-6") || !strings.Contains(ed.Fix, "ApplyPuppetfileModuleOverwrite") {
		t.Fatalf("apply-pending Fix should name the proposal and the Apply RPC, got %+v", ed)
	}
	if got := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod"); got != before {
		t.Fatalf("stored text changed by a refused Put:\nbefore: %q\nafter:  %q", before, got)
	}

	got, err := applyModule(h, "apache-6")
	if err != nil {
		t.Fatalf("ApplyPuppetfileModuleOverwrite: %v", err)
	}
	if got.GetName() != "puppetlabs/apache" || got.GetForge().GetVersion() != "6.1.0" {
		t.Fatalf("Apply returned %+v, want apache 6.1.0", got)
	}

	resp, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListPuppetfileModules: %v", err)
	}
	if len(resp.Modules) != 3 {
		t.Fatalf("got %d modules, want 3 (replaced, not appended)", len(resp.Modules))
	}
	if resp.Modules[1].GetName() != "puppetlabs/apache" || resp.Modules[1].GetForge().GetVersion() != "6.1.0" {
		t.Fatalf("apache not replaced in place at index 1: %+v", resp.Modules)
	}
	if resp.Modules[0].GetName() != "puppetlabs/ntp" || resp.Modules[2].GetName() != "puppetlabs/stdlib" {
		t.Fatalf("neighbours disturbed: %v", listModuleNames(t, h, "prod"))
	}
}

func TestCodeOverwriteRefusal_PuppetfileModule(t *testing.T) {
	replacement := forgeModule("puppetlabs/apache", "6.1.0")

	cases := []struct {
		name  string
		setup func(t *testing.T, h *host.Host)
	}{
		{"no proposal", func(t *testing.T, h *host.Host) {}},
		{"pending proposal", func(t *testing.T, h *host.Host) {
			proposeOverwrite(t, h, "p1", "prod", replacement)
		}},
		{"rejected proposal", func(t *testing.T, h *host.Host) {
			proposeOverwrite(t, h, "p1", "prod", replacement)
			rejectOverwrite(t, h, "p1")
		}},
		{"approved proposal for a different module name", func(t *testing.T, h *host.Host) {
			proposeOverwrite(t, h, "p1", "prod", forgeModule("puppetlabs/ntp", "9.9.9"))
			approveOverwrite(t, h, "p1")
		}},
		{"approved proposal whose name only shares a prefix", func(t *testing.T, h *host.Host) {
			proposeOverwrite(t, h, "p1", "prod", forgeModule("puppetlabs/apache2", "1.0.0"))
			approveOverwrite(t, h, "p1")
		}},
		{"approved proposal whose name differs only by case", func(t *testing.T, h *host.Host) {
			proposeOverwrite(t, h, "p1", "prod", forgeModule("puppetlabs/Apache", "1.0.0"))
			approveOverwrite(t, h, "p1")
		}},
		{"approved proposal for the same module in another environment", func(t *testing.T, h *host.Host) {
			mustCreateEnv(t, h, "staging")
			proposeOverwrite(t, h, "p1", "staging", replacement)
			approveOverwrite(t, h, "p1")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newOverwriteHost()
			ctx := context.Background()
			mustCreateEnv(t, h, "prod")
			mustPutModule(t, h, "prod", forgeModule("puppetlabs/ntp", ""))
			mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))
			before := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod")

			tc.setup(t, h)

			err := putModuleErr(h, "prod", replacement)
			if !local.IsCodeOverwriteRequiresApproval(err) {
				t.Fatalf("got %v, want requires-approval", err)
			}
			if local.IsCodeOverwriteApplyPending(err) {
				t.Fatalf("a proposal that does not cover the target must not report apply-pending")
			}
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
			}
			ed := errorDetail(err)
			if ed == nil || ed.Message == "" || !strings.Contains(ed.Fix, code.OverwriteCollection) ||
				!strings.Contains(ed.Fix, code.OverwriteApproveScope) || !strings.Contains(ed.Fix, "ApplyPuppetfileModuleOverwrite") {
				t.Fatalf("refusal must carry a structured Code/Message/Fix naming the collection, scope and Apply RPC, got %+v", ed)
			}
			if got := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod"); got != before {
				t.Fatalf("stored text changed by a refused Put:\nbefore: %q\nafter:  %q", before, got)
			}
		})
	}
}

// TestCodeOverwriteLookupTieBreak proves that when several approved proposals
// cover one target, the one with the lowest doc id is named, whatever order
// they were created in.
func TestCodeOverwriteLookupTieBreak(t *testing.T) {
	h := newOverwriteHost()
	mustCreateEnv(t, h, "prod")
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))

	m := forgeModule("puppetlabs/apache", "6.1.0")
	for _, id := range []string{"zz-last", "mm-middle", "aa-first"} {
		proposeOverwrite(t, h, id, "prod", m)
		approveOverwrite(t, h, id)
	}
	for i := 0; i < 20; i++ {
		err := putModuleErr(h, "prod", m)
		if !local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want apply-pending", err)
		}
		fix := errorDetail(err).Fix
		if !strings.Contains(fix, "aa-first") || strings.Contains(fix, "mm-middle") || strings.Contains(fix, "zz-last") {
			t.Fatalf("lookup did not return the lowest doc id, Fix = %q", fix)
		}
	}
}

func TestCodeOverwriteApplyIdempotent_PuppetfileModule(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()
	mustCreateEnv(t, h, "prod")
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/ntp", ""))
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))

	proposeOverwrite(t, h, "apache-6", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
	approveOverwrite(t, h, "apache-6")

	first, err := applyModule(h, "apache-6")
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	textAfterFirst := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod")

	second, err := applyModule(h, "apache-6")
	if err != nil {
		t.Fatalf("second Apply must succeed, got: %v", err)
	}
	if status.Code(err) == codes.AlreadyExists {
		t.Fatalf("second Apply returned AlreadyExists")
	}
	if first.GetName() != second.GetName() || first.GetForge().GetVersion() != second.GetForge().GetVersion() {
		t.Fatalf("Apply not idempotent: first %+v, second %+v", first, second)
	}
	if got := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod"); got != textAfterFirst {
		t.Fatalf("second Apply changed the text:\nfirst:  %q\nsecond: %q", textAfterFirst, got)
	}
	if names := listModuleNames(t, h, "prod"); len(names) != 2 {
		t.Fatalf("second Apply double-appended: %v", names)
	}
}

// TestCodeOverwriteApplyConcurrent proves two Apply calls on one proposal
// converge on one identical stored Puppetfile. Run under -race.
func TestCodeOverwriteApplyConcurrent(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()
	mustCreateEnv(t, h, "prod")
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/ntp", ""))
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))
	proposeOverwrite(t, h, "apache-6", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
	approveOverwrite(t, h, "apache-6")

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = applyModule(h, "apache-6")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Apply %d: %v", i, err)
		}
	}
	if names := listModuleNames(t, h, "prod"); len(names) != 2 || names[1] != "puppetlabs/apache" {
		t.Fatalf("concurrent Apply produced %v, want [ntp apache]", names)
	}
	want := "6.1.0"
	resp, _ := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if got := resp.Modules[1].GetForge().GetVersion(); got != want {
		t.Fatalf("apache version = %q, want %q", got, want)
	}
}

func TestCodeOverwriteApplyRefusals_PuppetfileModule(t *testing.T) {
	setup := func(t *testing.T) (*host.Host, string) {
		h := newOverwriteHost()
		mustCreateEnv(t, h, "prod")
		mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))
		proposeOverwrite(t, h, "p1", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
		return h, puppetfileTextFromDocumentsRaw(t, h, context.Background(), "prod")
	}

	t.Run("pending", func(t *testing.T) {
		h, before := setup(t)
		_, err := applyModule(h, "p1")
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not approved") {
			t.Fatalf("got %v, want FailedPrecondition naming not approved", err)
		}
		if got := puppetfileTextFromDocumentsRaw(t, h, context.Background(), "prod"); got != before {
			t.Fatalf("Apply of a pending proposal wrote: %q", got)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		h, before := setup(t)
		rejectOverwrite(t, h, "p1")
		_, err := applyModule(h, "p1")
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not approved") {
			t.Fatalf("got %v, want FailedPrecondition naming not approved", err)
		}
		if got := puppetfileTextFromDocumentsRaw(t, h, context.Background(), "prod"); got != before {
			t.Fatalf("Apply of a rejected proposal wrote: %q", got)
		}
	})
	t.Run("unknown proposal", func(t *testing.T) {
		h, _ := setup(t)
		_, err := applyModule(h, "does-not-exist")
		if status.Code(err) != codes.NotFound {
			t.Fatalf("got %v, want the Documents store's NotFound", err)
		}
	})
	t.Run("proposal for another resource type", func(t *testing.T) {
		h, before := setup(t)
		body, _ := code.OverwriteBodyForPuppetfileModule("prod", forgeModule("puppetlabs/apache", "6.1.0"))
		body["target"].(map[string]any)["resource"] = code.OverwriteResourceHieraLevel
		if _, err := approval.ProposeBody(context.Background(), h, overwriteKind, "hiera-1", body); err != nil {
			t.Fatalf("ProposeBody: %v", err)
		}
		approveOverwrite(t, h, "hiera-1")
		_, err := applyModule(h, "hiera-1")
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("got %v, want FailedPrecondition", err)
		}
		if got := puppetfileTextFromDocumentsRaw(t, h, context.Background(), "prod"); got != before {
			t.Fatalf("wrong-resource Apply wrote: %q", got)
		}
	})
	t.Run("target and payload disagree", func(t *testing.T) {
		h, before := setup(t)
		body, _ := code.OverwriteBodyForPuppetfileModule("prod", forgeModule("puppetlabs/apache", "6.1.0"))
		body["target"].(map[string]any)["name"] = "puppetlabs/ntp"
		if _, err := approval.ProposeBody(context.Background(), h, overwriteKind, "mixed", body); err != nil {
			t.Fatalf("ProposeBody: %v", err)
		}
		approveOverwrite(t, h, "mixed")
		_, err := applyModule(h, "mixed")
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("got %v, want FailedPrecondition", err)
		}
		if got := puppetfileTextFromDocumentsRaw(t, h, context.Background(), "prod"); got != before {
			t.Fatalf("mismatched Apply wrote: %q", got)
		}
	})
	t.Run("environment gone", func(t *testing.T) {
		h, _ := setup(t)
		approveOverwrite(t, h, "p1")
		if _, err := h.Code.DeleteEnvironment(context.Background(), &hostv1.DeleteEnvironmentRequest{Name: "prod"}); err != nil {
			t.Fatalf("DeleteEnvironment: %v", err)
		}
		_, err := applyModule(h, "p1")
		if status.Code(err) != codes.NotFound {
			t.Fatalf("got %v, want NotFound", err)
		}
	})
}

// TestCodeOverwriteApplyDoesNotDecide proves Apply wrote nothing back into
// the proposal: same status, same version.
func TestCodeOverwriteApplyDoesNotDecide(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()
	mustCreateEnv(t, h, "prod")
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))
	proposeOverwrite(t, h, "apache-6", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
	approveOverwrite(t, h, "apache-6")

	before, ok := getDoc(t, h, ctx, code.OverwriteCollection, "apache-6")
	if !ok {
		t.Fatal("proposal document missing")
	}
	if _, err := applyModule(h, "apache-6"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := applyModule(h, "apache-6"); err != nil {
		t.Fatalf("Apply (repeat): %v", err)
	}
	after, _ := getDoc(t, h, ctx, code.OverwriteCollection, "apache-6")

	if got := after.Body.Value.AsMap()["status"]; got != "approved" {
		t.Fatalf("status after Apply = %v, want approved", got)
	}
	if after.Version != before.Version {
		t.Fatalf("proposal version changed by Apply: %d -> %d", before.Version, after.Version)
	}
}

func TestCodeOverwriteApplyPermission(t *testing.T) {
	// A host that can propose and approve but declares no code:rw.
	h := local.New([]string{"tokens:issue"}, "controlrepo")

	// An unknown proposal id: if Apply read the proposal before checking the
	// permission, this would be NotFound, not PermissionDenied.
	_, err := applyModule(h, "never-existed")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("got %v, want PermissionDenied", err)
	}
	if ed := errorDetail(err); ed == nil || !strings.Contains(ed.Fix, "code:rw") {
		t.Fatalf("denial should name code:rw, got %+v", ed)
	}

	// A real approved proposal is equally unreachable without the permission.
	proposeOverwrite(t, h, "p1", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
	approveOverwrite(t, h, "p1")
	if _, err := applyModule(h, "p1"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("approved proposal without code:rw: got %v, want PermissionDenied", err)
	}
}

// --- environment slice: settings and duplicate-over-existing ---

var overwriteSeq atomic.Int64

func nextProposalID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, overwriteSeq.Add(1))
}

func proposeBodyAs(t *testing.T, h *host.Host, proposalID string, body map[string]any, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("building proposal body %s: %v", proposalID, err)
	}
	if _, err := approval.ProposeBody(context.Background(), h, overwriteKind, proposalID, body); err != nil {
		t.Fatalf("ProposeBody(%s): %v", proposalID, err)
	}
}

func proposeSettings(t *testing.T, h *host.Host, proposalID string, settings *hostv1.EnvironmentSettings) {
	t.Helper()
	body, err := code.OverwriteBodyForSettings(settings)
	proposeBodyAs(t, h, proposalID, body, err)
}

func proposeDuplicate(t *testing.T, h *host.Host, proposalID, source, target string) {
	t.Helper()
	body, err := code.OverwriteBodyForEnvironmentDuplicate(source, target)
	proposeBodyAs(t, h, proposalID, body, err)
}

func applySettings(h *host.Host, proposalID string) (*hostv1.EnvironmentSettings, error) {
	return h.Code.ApplyEnvironmentSettings(context.Background(), &hostv1.ApplyEnvironmentSettingsRequest{ProposalId: proposalID})
}

func applyDuplicate(h *host.Host, proposalID string) (*hostv1.Environment, error) {
	return h.Code.ApplyEnvironmentDuplicate(context.Background(), &hostv1.ApplyEnvironmentDuplicateRequest{ProposalId: proposalID})
}

// putSettingsViaApproval runs the whole sanctioned path for one settings
// write: propose, approve, ApplyEnvironmentSettings. Settings writes are gated
// on every call (D-03), so tests that need settings on an environment use it.
func putSettingsViaApproval(t *testing.T, h *host.Host, settings *hostv1.EnvironmentSettings) *hostv1.EnvironmentSettings {
	t.Helper()
	id := nextProposalID("settings-" + settings.GetEnvironment())
	proposeSettings(t, h, id, settings)
	approveOverwrite(t, h, id)
	got, err := applySettings(h, id)
	if err != nil {
		t.Fatalf("ApplyEnvironmentSettings(%s): %v", id, err)
	}
	return got
}

func putSettingsErr(h *host.Host, settings *hostv1.EnvironmentSettings) error {
	_, err := h.Code.PutEnvironmentSettings(context.Background(), &hostv1.PutEnvironmentSettingsRequest{Settings: settings})
	return err
}

func duplicateErr(h *host.Host, source, target string) error {
	_, err := h.Code.DuplicateEnvironment(context.Background(), &hostv1.DuplicateEnvironmentRequest{SourceName: source, TargetName: target})
	return err
}

func mustGetSettings(t *testing.T, h *host.Host, env string) *hostv1.EnvironmentSettings {
	t.Helper()
	got, err := h.Code.GetEnvironmentSettings(context.Background(), &hostv1.GetEnvironmentSettingsRequest{Environment: env})
	if err != nil {
		t.Fatalf("GetEnvironmentSettings(%s): %v", env, err)
	}
	return got
}

func assertAllSettingsAbsent(t *testing.T, got *hostv1.EnvironmentSettings) {
	t.Helper()
	if got.Modulepath != nil || got.Manifest != nil || got.ConfigVersion != nil || got.EnvironmentTimeout != nil ||
		got.DisablePerEnvironmentManifest != nil || got.StaticCatalogs != nil || got.RichData != nil {
		t.Fatalf("settings not all absent: %+v", got)
	}
}

func TestCodeOverwriteSettings(t *testing.T) {
	newHost := func(t *testing.T) *host.Host {
		h := newOverwriteHost()
		mustCreateEnv(t, h, "production")
		return h
	}
	approved := &hostv1.EnvironmentSettings{Environment: "production", Modulepath: strPtr("site:modules"), RichData: boolPtr(true)}

	t.Run("first write to a fresh environment is refused", func(t *testing.T) {
		h := newHost(t)
		err := putSettingsErr(h, approved)
		if !local.IsCodeOverwriteRequiresApproval(err) || status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("got %v, want FailedPrecondition requires-approval", err)
		}
		if ed := errorDetail(err); ed == nil || !strings.Contains(ed.Fix, "ApplyEnvironmentSettings") {
			t.Fatalf("Fix should name ApplyEnvironmentSettings, got %+v", ed)
		}
		assertAllSettingsAbsent(t, mustGetSettings(t, h, "production"))
	})

	t.Run("creating the environment stays ungated", func(t *testing.T) {
		h := newOverwriteHost()
		if _, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: "fresh"}); err != nil {
			t.Fatalf("CreateEnvironment: %v", err)
		}
		assertAllSettingsAbsent(t, mustGetSettings(t, h, "fresh"))
	})

	t.Run("a pending proposal does not authorize the write", func(t *testing.T) {
		h := newHost(t)
		proposeSettings(t, h, "s1", approved)
		if err := putSettingsErr(h, approved); !local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("got %v, want requires-approval", err)
		}
	})

	t.Run("an approved proposal still refuses the Put with apply-pending", func(t *testing.T) {
		h := newHost(t)
		proposeSettings(t, h, "s1", approved)
		approveOverwrite(t, h, "s1")
		err := putSettingsErr(h, approved)
		if !local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want apply-pending", err)
		}
		if ed := errorDetail(err); ed == nil || !strings.Contains(ed.Fix, "s1") || !strings.Contains(ed.Fix, "ApplyEnvironmentSettings") {
			t.Fatalf("Fix should name the proposal and the Apply RPC, got %+v", ed)
		}
		assertAllSettingsAbsent(t, mustGetSettings(t, h, "production"))
	})

	t.Run("a malformed record is refused as malformed, before the gate", func(t *testing.T) {
		h := newHost(t)
		err := putSettingsErr(h, &hostv1.EnvironmentSettings{Environment: "production", Modulepath: strPtr("a\nb")})
		if status.Code(err) != codes.InvalidArgument || local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("got %v, want InvalidArgument from the render check, not the gate", err)
		}
	})

	t.Run("an unknown environment is NotFound before the gate", func(t *testing.T) {
		h := newHost(t)
		err := putSettingsErr(h, &hostv1.EnvironmentSettings{Environment: "nope", Modulepath: strPtr("x")})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("got %v, want NotFound", err)
		}
	})

	t.Run("Apply stores exactly the approved fields", func(t *testing.T) {
		h := newHost(t)
		got := putSettingsViaApproval(t, h, approved)
		if got.Environment != "production" || got.GetModulepath() != "site:modules" || !got.GetRichData() {
			t.Fatalf("Apply returned %+v", got)
		}
		read := mustGetSettings(t, h, "production")
		if read.Modulepath == nil || *read.Modulepath != "site:modules" || read.RichData == nil || !*read.RichData {
			t.Fatalf("stored settings %+v", read)
		}
		if read.Manifest != nil || read.ConfigVersion != nil || read.EnvironmentTimeout != nil ||
			read.DisablePerEnvironmentManifest != nil || read.StaticCatalogs != nil {
			t.Fatalf("fields the approver left unset must stay absent, got %+v", read)
		}
	})

	t.Run("Apply is idempotent", func(t *testing.T) {
		h := newHost(t)
		proposeSettings(t, h, "s1", approved)
		approveOverwrite(t, h, "s1")
		first, err := applySettings(h, "s1")
		if err != nil {
			t.Fatalf("first Apply: %v", err)
		}
		v1, _ := getDoc(t, h, context.Background(), "code-environments", "production")
		second, err := applySettings(h, "s1")
		if err != nil {
			t.Fatalf("second Apply must succeed: %v", err)
		}
		v2, _ := getDoc(t, h, context.Background(), "code-environments", "production")
		if first.GetModulepath() != second.GetModulepath() || first.GetRichData() != second.GetRichData() {
			t.Fatalf("Apply not idempotent: %+v vs %+v", first, second)
		}
		if v1.Version != v2.Version {
			t.Fatalf("second Apply rewrote the document: version %d -> %d", v1.Version, v2.Version)
		}
	})

	t.Run("Apply refuses pending and wrong-resource proposals", func(t *testing.T) {
		h := newHost(t)
		proposeSettings(t, h, "pending", approved)
		if _, err := applySettings(h, "pending"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("pending: got %v, want FailedPrecondition", err)
		}
		proposeDuplicate(t, h, "dup", "production", "staging")
		approveOverwrite(t, h, "dup")
		if _, err := applySettings(h, "dup"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("wrong resource: got %v, want FailedPrecondition", err)
		}
		assertAllSettingsAbsent(t, mustGetSettings(t, h, "production"))
	})

	t.Run("Apply replaces the whole record and preserves identity", func(t *testing.T) {
		h := newHost(t)
		putSettingsViaApproval(t, h, approved)
		got := putSettingsViaApproval(t, h, &hostv1.EnvironmentSettings{Environment: "production", Manifest: strPtr("site.pp")})
		if got.Modulepath != nil || got.RichData != nil || got.GetManifest() != "site.pp" {
			t.Fatalf("second Apply must replace, not merge: %+v", got)
		}
		env, err := h.Code.GetEnvironment(context.Background(), &hostv1.GetEnvironmentRequest{Name: "production"})
		if err != nil || env.Name != "production" {
			t.Fatalf("identity lost after Apply: %v %+v", err, env)
		}
	})
}

func TestCodeOverwriteDuplicate(t *testing.T) {
	ctx := context.Background()
	// newHosts builds production (full), and staging holding one stale hiera
	// data file and a stale Puppetfile that a replacement must remove.
	newHosts := func(t *testing.T) *host.Host {
		h := newOverwriteHost()
		seedFullEnvironment(t, h, ctx, "production")
		mustCreateEnv(t, h, "staging")
		seedDoc(t, h, ctx, "code-hiera-data", "staging/old.yaml", map[string]any{"path": "old.yaml", "yaml": "stale: true\n"})
		seedDoc(t, h, ctx, "code-puppetfiles", "staging", map[string]any{"text": "mod 'stale/module', '1.0.0'\n"})
		return h
	}
	snapshot := func(t *testing.T, h *host.Host, env string) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, c := range []string{"code-environments", "code-puppetfiles", "code-hiera-hierarchy"} {
			if d, ok := getDoc(t, h, ctx, c, env); ok {
				out[c+"/"+env] = fmt.Sprintf("v%d %v", d.Version, d.Body.Value.AsMap())
			}
		}
		resp, err := h.Documents.List(ctx, &hostv1.ListDocumentsRequest{Collection: "code-hiera-data"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, d := range resp.Documents {
			if strings.HasPrefix(d.DocId, env+"/") {
				out["code-hiera-data/"+d.DocId] = fmt.Sprintf("v%d %v", d.Version, d.Body.Value.AsMap())
			}
		}
		return out
	}
	equalSnap := func(a, b map[string]string) bool {
		if len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if b[k] != v {
				return false
			}
		}
		return true
	}

	t.Run("an unused target name stays ungated", func(t *testing.T) {
		h := newOverwriteHost()
		seedFullEnvironment(t, h, ctx, "production")
		if err := duplicateErr(h, "production", "qa"); err != nil {
			t.Fatalf("DuplicateEnvironment onto an unused name: %v", err)
		}
		assertEnvDocsPresent(t, h, ctx, "qa")
	})

	t.Run("a target in use is refused with the structured error and left untouched", func(t *testing.T) {
		h := newHosts(t)
		before := snapshot(t, h, "staging")
		err := duplicateErr(h, "production", "staging")
		if !local.IsCodeOverwriteRequiresApproval(err) || status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("got %v, want FailedPrecondition requires-approval", err)
		}
		if status.Code(err) == codes.AlreadyExists {
			t.Fatalf("must no longer be a bare AlreadyExists")
		}
		if ed := errorDetail(err); ed == nil || !strings.Contains(ed.Fix, "ApplyEnvironmentDuplicate") {
			t.Fatalf("Fix should name ApplyEnvironmentDuplicate, got %+v", ed)
		}
		if after := snapshot(t, h, "staging"); !equalSnap(before, after) {
			t.Fatalf("target changed by a refused duplicate:\nbefore %v\nafter  %v", before, after)
		}
	})

	t.Run("an approval is source-specific", func(t *testing.T) {
		h := newHosts(t)
		mustCreateEnv(t, h, "dev")
		proposeDuplicate(t, h, "p1", "production", "staging")
		approveOverwrite(t, h, "p1")
		// Approved for production over staging, not for dev over staging.
		err := duplicateErr(h, "dev", "staging")
		if !local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want requires-approval for the other source", err)
		}
	})

	t.Run("an approved proposal refuses the DuplicateEnvironment with apply-pending", func(t *testing.T) {
		h := newHosts(t)
		before := snapshot(t, h, "staging")
		proposeDuplicate(t, h, "p1", "production", "staging")
		approveOverwrite(t, h, "p1")
		err := duplicateErr(h, "production", "staging")
		if !local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want apply-pending", err)
		}
		if ed := errorDetail(err); ed == nil || !strings.Contains(ed.Fix, "p1") || !strings.Contains(ed.Fix, "ApplyEnvironmentDuplicate") {
			t.Fatalf("Fix should name the proposal and the Apply RPC, got %+v", ed)
		}
		if after := snapshot(t, h, "staging"); !equalSnap(before, after) {
			t.Fatalf("target changed by a refused duplicate")
		}
	})

	t.Run("Apply replaces the target's owned documents with the source's copies", func(t *testing.T) {
		h := newHosts(t)
		srcBefore := snapshot(t, h, "production")
		proposeDuplicate(t, h, "p1", "production", "staging")
		approveOverwrite(t, h, "p1")

		env, err := applyDuplicate(h, "p1")
		if err != nil {
			t.Fatalf("ApplyEnvironmentDuplicate: %v", err)
		}
		if env.Name != "staging" {
			t.Fatalf("Apply returned %+v", env)
		}
		assertEnvDocsPresent(t, h, ctx, "staging")
		if _, ok := getDoc(t, h, ctx, "code-hiera-data", "staging/old.yaml"); ok {
			t.Fatal("the target's prior hiera data file survived the replacement")
		}
		pf, _ := getDoc(t, h, ctx, "code-puppetfiles", "staging")
		if got := pf.Body.Value.AsMap()["text"]; got != "mod 'puppetlabs/apache', '5.0.0'\n" {
			t.Fatalf("target Puppetfile not replaced by source's: %v", got)
		}
		id, _ := getDoc(t, h, ctx, "code-environments", "staging")
		if id.Body.Value.AsMap()["name"] != "staging" {
			t.Fatalf("identity document name not rekeyed: %v", id.Body.Value.AsMap())
		}
		if srcAfter := snapshot(t, h, "production"); !equalSnap(srcBefore, srcAfter) {
			t.Fatalf("the source changed by Apply")
		}

		// The copies share no value with the source: editing the target's
		// settings afterwards leaves the source's unchanged.
		putSettingsViaApproval(t, h, &hostv1.EnvironmentSettings{Environment: "staging", Modulepath: strPtr("changed")})
		if got := mustGetSettings(t, h, "production").GetModulepath(); got != testSettingsModulepath {
			t.Fatalf("source settings changed through the copy: %q", got)
		}
	})

	t.Run("Apply is idempotent", func(t *testing.T) {
		h := newHosts(t)
		proposeDuplicate(t, h, "p1", "production", "staging")
		approveOverwrite(t, h, "p1")
		if _, err := applyDuplicate(h, "p1"); err != nil {
			t.Fatalf("first Apply: %v", err)
		}
		first := snapshot(t, h, "staging")
		if _, err := applyDuplicate(h, "p1"); err != nil {
			t.Fatalf("second Apply must succeed: %v", err)
		}
		if second := snapshot(t, h, "staging"); !equalSnap(first, second) {
			t.Fatalf("second Apply changed the document set:\nfirst  %v\nsecond %v", first, second)
		}
	})

	t.Run("Apply reads the source at apply time", func(t *testing.T) {
		h := newHosts(t)
		proposeDuplicate(t, h, "p1", "production", "staging")
		approveOverwrite(t, h, "p1")
		cur, _ := getDoc(t, h, ctx, "code-puppetfiles", "production")
		if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
			Collection: "code-puppetfiles", DocId: "production", IfVersion: cur.Version,
			Body: &hostv1.Json{Value: mustStruct(t, map[string]any{"text": "mod 'puppetlabs/ntp', '9.0.0'\n"})},
		}); err != nil {
			t.Fatalf("updating the source Puppetfile: %v", err)
		}
		if _, err := applyDuplicate(h, "p1"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		pf, _ := getDoc(t, h, ctx, "code-puppetfiles", "staging")
		if got := pf.Body.Value.AsMap()["text"]; got != "mod 'puppetlabs/ntp', '9.0.0'\n" {
			t.Fatalf("Apply used a stale source: %v", got)
		}
	})

	t.Run("an applied approval cannot be replayed against a changed source (CR-01)", func(t *testing.T) {
		h := newHosts(t)
		proposeDuplicate(t, h, "p1", "production", "staging")
		approveOverwrite(t, h, "p1")
		if _, err := applyDuplicate(h, "p1"); err != nil {
			t.Fatalf("first Apply: %v", err)
		}
		afterFirst := snapshot(t, h, "staging")

		// The source is rewritten after the approval was used.
		cur, _ := getDoc(t, h, ctx, "code-puppetfiles", "production")
		if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
			Collection: "code-puppetfiles", DocId: "production", IfVersion: cur.Version,
			Body: &hostv1.Json{Value: mustStruct(t, map[string]any{"text": "mod 'evil/module', '6.6.6'\n"})},
		}); err != nil {
			t.Fatalf("rewriting the source Puppetfile: %v", err)
		}

		_, err := applyDuplicate(h, "p1")
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "already applied") {
			t.Fatalf("replay against a changed source: got %v, want FailedPrecondition naming already applied", err)
		}
		if after := snapshot(t, h, "staging"); !equalSnap(afterFirst, after) {
			t.Fatalf("a refused replay changed the target:\nbefore %v\nafter  %v", afterFirst, after)
		}
	})

	t.Run("Apply refusals", func(t *testing.T) {
		h := newHosts(t)
		proposeDuplicate(t, h, "pending", "production", "staging")
		if _, err := applyDuplicate(h, "pending"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("pending: got %v", err)
		}
		proposeDuplicate(t, h, "self", "staging", "staging")
		approveOverwrite(t, h, "self")
		if _, err := applyDuplicate(h, "self"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("source == target: got %v", err)
		}
		proposeDuplicate(t, h, "gone", "ghost", "staging")
		approveOverwrite(t, h, "gone")
		if _, err := applyDuplicate(h, "gone"); status.Code(err) != codes.NotFound {
			t.Fatalf("missing source: got %v, want NotFound", err)
		}
		proposeSettings(t, h, "wrong", &hostv1.EnvironmentSettings{Environment: "staging", Modulepath: strPtr("x")})
		approveOverwrite(t, h, "wrong")
		if _, err := applyDuplicate(h, "wrong"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("wrong resource: got %v", err)
		}
	})
}

// TestCodeOverwriteDuplicateApplyIsAtomic runs concurrent readers against
// ApplyEnvironmentDuplicate. The target starts with one hiera data file and
// ends with two, so a reader listing that collection through one RPC must see
// exactly one or exactly two, never zero (deleted, not yet rewritten) or three.
func TestCodeOverwriteDuplicateApplyIsAtomic(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()
	seedFullEnvironment(t, h, ctx, "production")
	mustCreateEnv(t, h, "staging")
	seedDoc(t, h, ctx, "code-hiera-data", "staging/old.yaml", map[string]any{"path": "old.yaml", "yaml": "stale: true\n"})
	proposeDuplicate(t, h, "p1", "production", "staging")
	approveOverwrite(t, h, "p1")

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := h.Documents.List(ctx, &hostv1.ListDocumentsRequest{Collection: "code-hiera-data"})
				if err != nil {
					t.Errorf("List: %v", err)
					return
				}
				n := 0
				for _, d := range resp.Documents {
					if strings.HasPrefix(d.DocId, "staging/") {
						n++
					}
				}
				if n != 1 && n != 2 {
					t.Errorf("torn state observed: %d staging hiera data documents", n)
					return
				}
			}
		}()
	}

	var appliers sync.WaitGroup
	for i := 0; i < 8; i++ {
		appliers.Add(1)
		go func() {
			defer appliers.Done()
			if _, err := applyDuplicate(h, "p1"); err != nil {
				t.Errorf("Apply: %v", err)
			}
		}()
	}
	appliers.Wait()
	close(stop)
	readers.Wait()
	assertEnvDocsPresent(t, h, ctx, "staging")
}

// TestCodeOverwriteDeletesStayUngated pins D-01: no delete RPC consults a
// proposal, so each still succeeds with no proposal anywhere.
func TestCodeOverwriteDeletesStayUngated(t *testing.T) {
	ctx := context.Background()
	h := newOverwriteHost()
	seedFullEnvironment(t, h, ctx, "production")
	if _, err := h.Code.DeleteEnvironment(ctx, &hostv1.DeleteEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("DeleteEnvironment must stay ungated: %v", err)
	}
	assertEnvDocsAbsent(t, h, ctx, "production")

	// The Hiera and Puppetfile deletes need no proposal either.
	h = newHieraHost(t)
	mustPutModule(t, h, "prod", forgeModule("puppetlabs/ntp", ""))
	if _, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "prod", Name: "puppetlabs/ntp"}); err != nil {
		t.Fatalf("RemovePuppetfileModule must stay ungated: %v", err)
	}
	if _, err := h.Code.RemoveHieraDataKey(ctx, &hostv1.RemoveHieraDataKeyRequest{Environment: "prod", Path: "common.yaml", Key: "port"}); err != nil {
		t.Fatalf("RemoveHieraDataKey must stay ungated: %v", err)
	}
	if _, err := h.Code.RemoveHieraLevel(ctx, &hostv1.RemoveHieraLevelRequest{Environment: "prod", Name: "role"}); err != nil {
		t.Fatalf("RemoveHieraLevel must stay ungated: %v", err)
	}
	if _, err := h.Code.DeleteHieraDataFile(ctx, &hostv1.DeleteHieraDataFileRequest{Environment: "prod", Path: "nodes/web01.yaml"}); err != nil {
		t.Fatalf("DeleteHieraDataFile must stay ungated: %v", err)
	}
	if _, err := h.Code.DeleteEnvironment(ctx, &hostv1.DeleteEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("DeleteEnvironment (with content) must stay ungated: %v", err)
	}
}

// --- Hiera slice: level overwrite and data-key overwrite ---

func proposeHieraLevel(t *testing.T, h *host.Host, proposalID, env string, lvl *hostv1.HieraLevel, index int32, insert bool) {
	t.Helper()
	body, err := code.OverwriteBodyForHieraLevel(env, lvl, index, insert)
	proposeBodyAs(t, h, proposalID, body, err)
}

func proposeHieraDataKey(t *testing.T, h *host.Host, proposalID, env, path, key string, value *hostv1.Json) {
	t.Helper()
	body, err := code.OverwriteBodyForHieraDataKey(env, path, key, value)
	proposeBodyAs(t, h, proposalID, body, err)
}

func applyHieraLevel(h *host.Host, proposalID string) (*hostv1.PutHieraLevelResponse, error) {
	return h.Code.ApplyHieraLevelOverwrite(context.Background(), &hostv1.ApplyHieraLevelOverwriteRequest{ProposalId: proposalID})
}

func applyHieraDataKey(h *host.Host, proposalID string) (*hostv1.HieraDataFile, error) {
	return h.Code.ApplyHieraDataKeyOverwrite(context.Background(), &hostv1.ApplyHieraDataKeyOverwriteRequest{ProposalId: proposalID})
}

// putHieraLevelViaApproval runs propose, approve and ApplyHieraLevelOverwrite
// for one level, for tests that need to replace a level that already exists.
func putHieraLevelViaApproval(t *testing.T, h *host.Host, env string, lvl *hostv1.HieraLevel, index int32, insert bool) *hostv1.PutHieraLevelResponse {
	t.Helper()
	id := nextProposalID("level-" + lvl.GetName())
	proposeHieraLevel(t, h, id, env, lvl, index, insert)
	approveOverwrite(t, h, id)
	resp, err := applyHieraLevel(h, id)
	if err != nil {
		t.Fatalf("ApplyHieraLevelOverwrite(%s): %v", id, err)
	}
	return resp
}

// dataKeyViaApproval runs propose, approve and ApplyHieraDataKeyOverwrite.
func dataKeyViaApproval(t *testing.T, h *host.Host, env, path, key string, value *hostv1.Json) *hostv1.HieraDataFile {
	t.Helper()
	id := nextProposalID("key-" + key)
	proposeHieraDataKey(t, h, id, env, path, key, value)
	approveOverwrite(t, h, id)
	df, err := applyHieraDataKey(h, id)
	if err != nil {
		t.Fatalf("ApplyHieraDataKeyOverwrite(%s): %v", id, err)
	}
	return df
}

func putLevelErr(h *host.Host, env string, lvl *hostv1.HieraLevel, index int32, insert bool) error {
	_, err := h.Code.PutHieraLevel(context.Background(), &hostv1.PutHieraLevelRequest{Environment: env, Level: lvl, Index: index, Insert: insert})
	return err
}

func putKeyErr(h *host.Host, env, path, key string, value *hostv1.Json) error {
	_, err := h.Code.PutHieraDataKey(context.Background(), &hostv1.PutHieraDataKeyRequest{Environment: env, Path: path, Key: key, Value: value})
	return err
}

func scalarJSON(t *testing.T, v any) *hostv1.Json {
	t.Helper()
	return &hostv1.Json{Value: mustStruct(t, map[string]any{"v": v})}
}

const seededHierarchy = "# head comment\nversion: 5\nhierarchy:\n  - name: role\n    path: roles/x.yaml # role note\n  - name: common\n    path: common.yaml\n"

const seededCommon = "# port comment\nport: 8080\nname: web\n"

func newHieraHost(t *testing.T) *host.Host {
	t.Helper()
	h := newOverwriteHost()
	ctx := context.Background()
	mustCreateEnv(t, h, "prod")
	seedDoc(t, h, ctx, "code-hiera-hierarchy", "prod", map[string]any{"yaml": seededHierarchy})
	seedDoc(t, h, ctx, "code-hiera-data", "prod/common.yaml", map[string]any{"path": "common.yaml", "yaml": seededCommon})
	seedDoc(t, h, ctx, "code-hiera-data", "prod/nodes/web01.yaml", map[string]any{"path": "nodes/web01.yaml", "yaml": "port: 1\n"})
	return h
}

func levelNames(hier *hostv1.HieraHierarchy) []string {
	var out []string
	for _, l := range hier.GetLevels() {
		out = append(out, l.GetName())
	}
	return out
}

func TestCodeOverwriteHieraLevel(t *testing.T) {
	ctx := context.Background()
	replacement := &hostv1.HieraLevel{Name: "common", Path: "common.yaml", DataHash: "yaml_data"}

	t.Run("adding a new level stays ungated and warnings travel with the write", func(t *testing.T) {
		h := newHieraHost(t)
		resp, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
			Environment: "prod",
			Level:       &hostv1.HieraLevel{Name: "env", Path: "env/%{environment}/x.yaml"},
			Index:       1, Insert: true,
		})
		if err != nil {
			t.Fatalf("PutHieraLevel(new level): %v", err)
		}
		if got := levelNames(resp.Hierarchy); len(got) != 3 || got[1] != "env" {
			t.Fatalf("levels = %v", got)
		}
		if len(resp.Warnings) == 0 {
			t.Fatal("the lint warning must still travel on the applied write")
		}
	})

	t.Run("a new level in an environment with no hierarchy stays ungated", func(t *testing.T) {
		h := newOverwriteHost()
		mustCreateEnv(t, h, "fresh")
		if err := putLevelErr(h, "fresh", &hostv1.HieraLevel{Name: "common", Path: "common.yaml"}, 0, true); err != nil {
			t.Fatalf("PutHieraLevel on an unauthored hierarchy: %v", err)
		}
	})

	t.Run("an existing level is refused and the yaml is untouched", func(t *testing.T) {
		h := newHieraHost(t)
		err := putLevelErr(h, "prod", replacement, 0, false)
		if !local.IsCodeOverwriteRequiresApproval(err) || status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("got %v, want FailedPrecondition requires-approval", err)
		}
		if got := hieraHierarchyTextRaw(t, h, ctx, "prod"); got != seededHierarchy {
			t.Fatalf("stored yaml changed by a refused write:\n%q", got)
		}
		// insert=true of an existing name is equally an overwrite.
		if err := putLevelErr(h, "prod", replacement, 0, true); !local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("insert of an existing name: got %v, want requires-approval", err)
		}
		if got := hieraHierarchyTextRaw(t, h, ctx, "prod"); got != seededHierarchy {
			t.Fatalf("stored yaml changed by a refused insert:\n%q", got)
		}
	})

	t.Run("a prefix or case variant of an approved name is not covered", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraLevel(t, h, "p1", "prod", &hostv1.HieraLevel{Name: "common_extra", Path: "x.yaml"}, 0, false)
		approveOverwrite(t, h, "p1")
		if err := putLevelErr(h, "prod", replacement, 0, false); !local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want requires-approval", err)
		}
	})

	t.Run("an approved proposal still refuses the Put with apply-pending", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraLevel(t, h, "p1", "prod", replacement, 0, false)
		approveOverwrite(t, h, "p1")
		err := putLevelErr(h, "prod", replacement, 0, false)
		if !local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want apply-pending", err)
		}
		if ed := errorDetail(err); ed == nil || !strings.Contains(ed.Fix, "p1") || !strings.Contains(ed.Fix, "ApplyHieraLevelOverwrite") {
			t.Fatalf("Fix should name the proposal and the Apply RPC, got %+v", ed)
		}
		if got := hieraHierarchyTextRaw(t, h, ctx, "prod"); got != seededHierarchy {
			t.Fatalf("stored yaml changed:\n%q", got)
		}
	})

	t.Run("read-only lookup_options is refused before the gate", func(t *testing.T) {
		h := newHieraHost(t)
		err := putLevelErr(h, "prod", &hostv1.HieraLevel{Name: "common", LookupOptions: map[string]string{"k": "deep"}}, 0, false)
		if status.Code(err) != codes.InvalidArgument || local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("got %v, want InvalidArgument from the read-only check", err)
		}
	})

	t.Run("an absent environment is NotFound before the gate", func(t *testing.T) {
		h := newHieraHost(t)
		if err := putLevelErr(h, "ghost", replacement, 0, false); status.Code(err) != codes.NotFound {
			t.Fatalf("got %v, want NotFound", err)
		}
	})

	t.Run("Apply replaces in place, preserves order and comments, returns lint warnings", func(t *testing.T) {
		h := newHieraHost(t)
		lvl := &hostv1.HieraLevel{Name: "common", Path: "env/%{environment}/common.yaml", DataHash: "yaml_data"}
		resp := putHieraLevelViaApproval(t, h, "prod", lvl, 0, false)
		if got := levelNames(resp.Hierarchy); len(got) != 2 || got[0] != "role" || got[1] != "common" {
			t.Fatalf("hierarchy order = %v, want [role common]", got)
		}
		if resp.Hierarchy.Levels[1].DataHash != "yaml_data" {
			t.Fatalf("level not replaced: %+v", resp.Hierarchy.Levels[1])
		}
		if len(resp.Warnings) == 0 {
			t.Fatal("Apply must compute lint warnings for the approved level")
		}
		text := hieraHierarchyTextRaw(t, h, ctx, "prod")
		if !strings.Contains(text, "# head comment") || !strings.Contains(text, "# role note") {
			t.Fatalf("comments lost:\n%s", text)
		}
	})

	t.Run("Apply mirrors lookup_options for display", func(t *testing.T) {
		h := newHieraHost(t)
		cur, _ := getDoc(t, h, ctx, "code-hiera-data", "prod/common.yaml")
		if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
			Collection: "code-hiera-data", DocId: "prod/common.yaml", IfVersion: cur.Version,
			Body: &hostv1.Json{Value: mustStruct(t, map[string]any{"path": "common.yaml", "yaml": "lookup_options:\n  port:\n    merge: deep\nport: 1\n"})},
		}); err != nil {
			t.Fatal(err)
		}
		resp := putHieraLevelViaApproval(t, h, "prod", replacement, 0, false)
		if got := resp.Hierarchy.Levels[1].LookupOptions["port"]; got != "deep" {
			t.Fatalf("lookup_options not mirrored: %v", resp.Hierarchy.Levels[1].LookupOptions)
		}
	})

	t.Run("Apply is idempotent", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraLevel(t, h, "p1", "prod", replacement, 0, false)
		approveOverwrite(t, h, "p1")
		first, err := applyHieraLevel(h, "p1")
		if err != nil {
			t.Fatalf("first Apply: %v", err)
		}
		textAfterFirst := hieraHierarchyTextRaw(t, h, ctx, "prod")
		second, err := applyHieraLevel(h, "p1")
		if err != nil {
			t.Fatalf("second Apply must succeed: %v", err)
		}
		if got := hieraHierarchyTextRaw(t, h, ctx, "prod"); got != textAfterFirst {
			t.Fatalf("second Apply changed the yaml:\n%q\n%q", textAfterFirst, got)
		}
		if !proto.Equal(first.Hierarchy, second.Hierarchy) {
			t.Fatalf("results differ: %v vs %v", first.Hierarchy, second.Hierarchy)
		}
	})

	t.Run("an approved insert never duplicates a name that exists", func(t *testing.T) {
		h := newHieraHost(t)
		resp := putHieraLevelViaApproval(t, h, "prod", replacement, 0, true)
		if got := levelNames(resp.Hierarchy); len(got) != 2 || got[0] != "role" || got[1] != "common" {
			t.Fatalf("hierarchy = %v, want the level replaced in place, not duplicated", got)
		}
	})

	t.Run("Apply refusals", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraLevel(t, h, "pending", "prod", replacement, 0, false)
		if _, err := applyHieraLevel(h, "pending"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("pending: got %v", err)
		}
		proposeSettings(t, h, "wrong", &hostv1.EnvironmentSettings{Environment: "prod", Modulepath: strPtr("x")})
		approveOverwrite(t, h, "wrong")
		if _, err := applyHieraLevel(h, "wrong"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("wrong resource: got %v", err)
		}
		body, _ := code.OverwriteBodyForHieraLevel("prod", replacement, 0, false)
		body["target"].(map[string]any)["name"] = "role"
		if _, err := approval.ProposeBody(ctx, h, overwriteKind, "mixed", body); err != nil {
			t.Fatal(err)
		}
		approveOverwrite(t, h, "mixed")
		if _, err := applyHieraLevel(h, "mixed"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("target/payload mismatch: got %v", err)
		}
		if got := hieraHierarchyTextRaw(t, h, ctx, "prod"); got != seededHierarchy {
			t.Fatalf("a refused Apply wrote:\n%q", got)
		}
		proposeHieraLevel(t, h, "gone", "ghost", replacement, 0, false)
		approveOverwrite(t, h, "gone")
		if _, err := applyHieraLevel(h, "gone"); status.Code(err) != codes.NotFound {
			t.Fatalf("absent environment: got %v, want NotFound", err)
		}
	})
}

func TestCodeOverwriteHieraDataKey(t *testing.T) {
	ctx := context.Background()
	newValue := func() *hostv1.Json { return scalarJSON(t, float64(9090)) }

	t.Run("a new key in an existing file stays ungated", func(t *testing.T) {
		h := newHieraHost(t)
		if err := putKeyErr(h, "prod", "common.yaml", "fresh_key", scalarJSON(t, "x")); err != nil {
			t.Fatalf("PutHieraDataKey(new key): %v", err)
		}
	})

	t.Run("a key in an absent file stays ungated", func(t *testing.T) {
		h := newHieraHost(t)
		if err := putKeyErr(h, "prod", "brand-new.yaml", "port", scalarJSON(t, "x")); err != nil {
			t.Fatalf("PutHieraDataKey(absent file): %v", err)
		}
	})

	t.Run("an existing key is refused and the yaml is untouched", func(t *testing.T) {
		h := newHieraHost(t)
		err := putKeyErr(h, "prod", "common.yaml", "port", newValue())
		if !local.IsCodeOverwriteRequiresApproval(err) || status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("got %v, want FailedPrecondition requires-approval", err)
		}
		if got := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml"); got != seededCommon {
			t.Fatalf("stored yaml changed by a refused write:\n%q", got)
		}
	})

	t.Run("an approved proposal still refuses the Put with apply-pending", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraDataKey(t, h, "p1", "prod", "common.yaml", "port", newValue())
		approveOverwrite(t, h, "p1")
		err := putKeyErr(h, "prod", "common.yaml", "port", newValue())
		if !local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want apply-pending", err)
		}
		if ed := errorDetail(err); ed == nil || !strings.Contains(ed.Fix, "p1") || !strings.Contains(ed.Fix, "ApplyHieraDataKeyOverwrite") {
			t.Fatalf("Fix should name the proposal and the Apply RPC, got %+v", ed)
		}
		if got := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml"); got != seededCommon {
			t.Fatalf("stored yaml changed:\n%q", got)
		}
	})

	t.Run("an approval is specific to the data file", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraDataKey(t, h, "p1", "prod", "common.yaml", "port", newValue())
		approveOverwrite(t, h, "p1")
		err := putKeyErr(h, "prod", "nodes/web01.yaml", "port", newValue())
		if !local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want requires-approval for the other file", err)
		}
		if got := hieraDataTextRaw(t, h, ctx, "prod", "nodes/web01.yaml"); got != "port: 1\n" {
			t.Fatalf("other file changed: %q", got)
		}
	})

	t.Run("an approval is specific to the key", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraDataKey(t, h, "p1", "prod", "common.yaml", "name", newValue())
		approveOverwrite(t, h, "p1")
		if err := putKeyErr(h, "prod", "common.yaml", "port", newValue()); !local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("got %v, want requires-approval for the other key", err)
		}
	})

	t.Run("the reserved lookup_options key is refused by its own path, not the gate", func(t *testing.T) {
		h := newHieraHost(t)
		err := putKeyErr(h, "prod", "common.yaml", "lookup_options", scalarJSON(t, "x"))
		if status.Code(err) != codes.InvalidArgument || local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("got %v, want InvalidArgument from the reserved-key refusal", err)
		}
	})

	t.Run("an absent environment is NotFound before the gate", func(t *testing.T) {
		h := newHieraHost(t)
		if err := putKeyErr(h, "ghost", "common.yaml", "port", newValue()); status.Code(err) != codes.NotFound {
			t.Fatalf("got %v, want NotFound", err)
		}
	})

	t.Run("Apply replaces the value and keeps comments and key order", func(t *testing.T) {
		h := newHieraHost(t)
		df := dataKeyViaApproval(t, h, "prod", "common.yaml", "port", newValue())
		if df.Environment != "prod" || df.Path != "common.yaml" {
			t.Fatalf("response identity: %+v", df)
		}
		if got := df.Values["port"].Value.AsMap()["v"]; got != float64(9090) {
			t.Fatalf("port = %v, want 9090", got)
		}
		text := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml")
		if !strings.Contains(text, "# port comment") {
			t.Fatalf("comment lost:\n%s", text)
		}
		if strings.Index(text, "port:") > strings.Index(text, "name:") {
			t.Fatalf("key order changed:\n%s", text)
		}
		if !strings.Contains(text, "9090") || strings.Contains(text, "8080") {
			t.Fatalf("value not replaced:\n%s", text)
		}
	})

	t.Run("Apply is idempotent", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraDataKey(t, h, "p1", "prod", "common.yaml", "port", newValue())
		approveOverwrite(t, h, "p1")
		first, err := applyHieraDataKey(h, "p1")
		if err != nil {
			t.Fatalf("first Apply: %v", err)
		}
		textAfterFirst := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml")
		doc1, _ := getDoc(t, h, ctx, "code-hiera-data", "prod/common.yaml")
		second, err := applyHieraDataKey(h, "p1")
		if err != nil {
			t.Fatalf("second Apply must succeed: %v", err)
		}
		doc2, _ := getDoc(t, h, ctx, "code-hiera-data", "prod/common.yaml")
		if got := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml"); got != textAfterFirst {
			t.Fatalf("second Apply changed the yaml:\n%q\n%q", textAfterFirst, got)
		}
		if doc1.Version != doc2.Version {
			t.Fatalf("second Apply rewrote the document")
		}
		if !proto.Equal(first, second) {
			t.Fatalf("results differ: %v vs %v", first, second)
		}
	})

	t.Run("Apply supports an object-valued key", func(t *testing.T) {
		h := newHieraHost(t)
		obj := &hostv1.Json{Value: mustStruct(t, map[string]any{"a": "b", "n": float64(2)})}
		df := dataKeyViaApproval(t, h, "prod", "common.yaml", "name", obj)
		got := df.Values["name"].Value.AsMap()
		if got["a"] != "b" || got["n"] != float64(2) {
			t.Fatalf("object value = %v", got)
		}
	})

	t.Run("Apply refusals", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraDataKey(t, h, "pending", "prod", "common.yaml", "port", newValue())
		if _, err := applyHieraDataKey(h, "pending"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("pending: got %v", err)
		}
		proposeSettings(t, h, "wrong", &hostv1.EnvironmentSettings{Environment: "prod", Modulepath: strPtr("x")})
		approveOverwrite(t, h, "wrong")
		if _, err := applyHieraDataKey(h, "wrong"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("wrong resource: got %v", err)
		}
		body, _ := code.OverwriteBodyForHieraDataKey("prod", "common.yaml", "lookup_options", scalarJSON(t, "x"))
		if _, err := approval.ProposeBody(ctx, h, overwriteKind, "reserved", body); err != nil {
			t.Fatal(err)
		}
		approveOverwrite(t, h, "reserved")
		if _, err := applyHieraDataKey(h, "reserved"); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("reserved key: got %v, want InvalidArgument", err)
		}
		if got := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml"); got != seededCommon {
			t.Fatalf("a refused Apply wrote:\n%q", got)
		}
		proposeHieraDataKey(t, h, "gone", "ghost", "common.yaml", "port", newValue())
		approveOverwrite(t, h, "gone")
		if _, err := applyHieraDataKey(h, "gone"); status.Code(err) != codes.NotFound {
			t.Fatalf("absent environment: got %v, want NotFound", err)
		}
	})
}

// TestCodeOverwriteHieraDataKeyKeepsCommentOnlyHeader proves the approved write
// path does not erase a comment-only data file's text (WR-01, IMP-05). The
// approval gate exists so a human sees exactly what will be written; a write
// that silently deletes the header they were looking at defeats the gate rather
// than merely losing a comment, so this is a distinct case from the direct-path
// comment test and must not be folded into it.
func TestCodeOverwriteHieraDataKeyKeepsCommentOnlyHeader(t *testing.T) {
	ctx := context.Background()
	const header = "# IMPORTANT: do not edit by hand\n# owner: team-a\n"
	seed := func(t *testing.T, text string) *host.Host {
		h := newHieraHost(t)
		seedDoc(t, h, ctx, "code-hiera-data", "prod/placeholder.yaml", map[string]any{"path": "placeholder.yaml", "yaml": text})
		return h
	}

	t.Run("an approved overwrite keeps the comment and writes the key once", func(t *testing.T) {
		h := seed(t, header)
		proposeHieraDataKey(t, h, "p1", "prod", "placeholder.yaml", "port", scalarJSON(t, float64(9090)))
		approveOverwrite(t, h, "p1")
		df, err := applyHieraDataKey(h, "p1")
		if err != nil {
			t.Fatalf("ApplyHieraDataKeyOverwrite: %v", err)
		}
		if got := df.Values["port"].Value.AsMap()["v"]; got != float64(9090) {
			t.Fatalf("port = %v, want 9090", got)
		}
		text := hieraDataTextRaw(t, h, ctx, "prod", "placeholder.yaml")
		if !strings.HasPrefix(text, header) || !strings.Contains(text, "9090") {
			t.Fatalf("the header was not kept above the written key:\n%s", text)
		}
		// The proposal is spent exactly once: a replay is idempotent and does not rewrite.
		again, err := applyHieraDataKey(h, "p1")
		if err != nil || !proto.Equal(df, again) {
			t.Fatalf("replay: %v, %v", again, err)
		}
		if got := hieraDataTextRaw(t, h, ctx, "prod", "placeholder.yaml"); got != text {
			t.Fatalf("replay changed the yaml:\n%q\n%q", text, got)
		}
	})

	t.Run("a refused write leaves the file untouched and the proposal unspent", func(t *testing.T) {
		const unclassifiable = "# header\n--- !!null\n"
		h := seed(t, unclassifiable)
		proposeHieraDataKey(t, h, "p1", "prod", "placeholder.yaml", "port", scalarJSON(t, float64(9090)))
		approveOverwrite(t, h, "p1")
		if _, err := applyHieraDataKey(h, "p1"); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("unclassifiable content-free file: got %v, want InvalidArgument", err)
		}
		if got := hieraDataTextRaw(t, h, ctx, "prod", "placeholder.yaml"); got != unclassifiable {
			t.Fatalf("a refused Apply wrote:\n%q", got)
		}
		// Fix the file; the same proposal still applies, so the refusal did not spend it.
		cur, _ := getDoc(t, h, ctx, "code-hiera-data", "prod/placeholder.yaml")
		if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
			Collection: "code-hiera-data", DocId: "prod/placeholder.yaml", IfVersion: cur.Version,
			Body: &hostv1.Json{Value: mustStruct(t, map[string]any{"path": "placeholder.yaml", "yaml": header})},
		}); err != nil {
			t.Fatalf("fixing the file: %v", err)
		}
		if _, err := applyHieraDataKey(h, "p1"); err != nil {
			t.Fatalf("Apply after fixing the file: %v", err)
		}
	})
}

// TestCodeOverwriteAllApplyRPCsRequireCodeRW proves each of the five Apply
// RPCs is reachable only through the gate and needs code:rw and nothing
// narrower: a host with no code:rw is denied before any proposal is read, and
// a host with it reaches a real body (an unknown proposal is NotFound, not
// Unimplemented).
func TestCodeOverwriteAllApplyRPCsRequireCodeRW(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(h *host.Host) error{
		"ApplyEnvironmentSettings": func(h *host.Host) error {
			_, err := h.Code.ApplyEnvironmentSettings(ctx, &hostv1.ApplyEnvironmentSettingsRequest{ProposalId: "x"})
			return err
		},
		"ApplyEnvironmentDuplicate": func(h *host.Host) error {
			_, err := h.Code.ApplyEnvironmentDuplicate(ctx, &hostv1.ApplyEnvironmentDuplicateRequest{ProposalId: "x"})
			return err
		},
		"ApplyPuppetfileModuleOverwrite": func(h *host.Host) error {
			_, err := h.Code.ApplyPuppetfileModuleOverwrite(ctx, &hostv1.ApplyPuppetfileModuleOverwriteRequest{ProposalId: "x"})
			return err
		},
		"ApplyHieraLevelOverwrite": func(h *host.Host) error {
			_, err := h.Code.ApplyHieraLevelOverwrite(ctx, &hostv1.ApplyHieraLevelOverwriteRequest{ProposalId: "x"})
			return err
		},
		"ApplyHieraDataKeyOverwrite": func(h *host.Host) error {
			_, err := h.Code.ApplyHieraDataKeyOverwrite(ctx, &hostv1.ApplyHieraDataKeyOverwriteRequest{ProposalId: "x"})
			return err
		},
	}
	denied := local.New([]string{"tokens:issue"}, "controlrepo")
	allowed := newOverwriteHost()
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(denied); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("without code:rw: got %v, want PermissionDenied", err)
			}
			if err := call(allowed); status.Code(err) != codes.NotFound {
				t.Fatalf("with code:rw and an unknown proposal: got %v, want the store's NotFound", err)
			}
		})
	}
}

// TestCodeOverwriteAppliedProposalIsSpent pins WR-02: an approval covers one
// application. After a proposal is applied and a later approved proposal changes
// the same item, replaying the first proposal is refused rather than silently
// reverting the item, and the Put refusal never names a spent proposal.
func TestCodeOverwriteAppliedProposalIsSpent(t *testing.T) {
	ctx := context.Background()

	requireSpent := func(t *testing.T, err error) {
		t.Helper()
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "already applied") {
			t.Fatalf("replay of a spent proposal: got %v, want FailedPrecondition naming already applied", err)
		}
	}
	requireNotPending := func(t *testing.T, err error) {
		t.Helper()
		if !local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("a Put over an item whose only approval is spent: got %v, want requires-approval (not apply-pending)", err)
		}
	}

	t.Run("puppetfile module", func(t *testing.T) {
		h := newOverwriteHost()
		mustCreateEnv(t, h, "prod")
		mustPutModule(t, h, "prod", forgeModule("puppetlabs/ntp", "0.1.0"))
		overwriteViaApproval(t, h, "a", "prod", forgeModule("puppetlabs/ntp", "1.0.0"))
		requireNotPending(t, putModuleErr(h, "prod", forgeModule("puppetlabs/ntp", "1.0.0")))
		overwriteViaApproval(t, h, "b", "prod", forgeModule("puppetlabs/ntp", "2.0.0"))

		_, err := applyModule(h, "a")
		requireSpent(t, err)
		resp, _ := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
		if got := resp.Modules[0].GetForge().GetVersion(); got != "2.0.0" {
			t.Fatalf("stale replay reverted the module to %q, want 2.0.0", got)
		}
		// A retry that changes nothing is still fine.
		if _, err := applyModule(h, "b"); err != nil {
			t.Fatalf("no-op retry of the latest proposal: %v", err)
		}
	})

	t.Run("environment settings", func(t *testing.T) {
		h := newOverwriteHost()
		mustCreateEnv(t, h, "production")
		proposeSettings(t, h, "a", &hostv1.EnvironmentSettings{Environment: "production", Modulepath: strPtr("one")})
		approveOverwrite(t, h, "a")
		if _, err := applySettings(h, "a"); err != nil {
			t.Fatalf("Apply a: %v", err)
		}
		putSettingsViaApproval(t, h, &hostv1.EnvironmentSettings{Environment: "production", Modulepath: strPtr("two")})

		_, err := applySettings(h, "a")
		requireSpent(t, err)
		if got := mustGetSettings(t, h, "production").GetModulepath(); got != "two" {
			t.Fatalf("stale replay reverted settings to %q, want two", got)
		}
	})

	t.Run("hiera level", func(t *testing.T) {
		h := newHieraHost(t)
		first := &hostv1.HieraLevel{Name: "common", Path: "one.yaml"}
		putHieraLevelViaApproval(t, h, "prod", first, 0, false)
		requireNotPending(t, putLevelErr(h, "prod", first, 0, false))
		proposeHieraLevel(t, h, "a", "prod", first, 0, false)
		approveOverwrite(t, h, "a")
		if _, err := applyHieraLevel(h, "a"); err != nil {
			t.Fatalf("Apply a: %v", err)
		}
		putHieraLevelViaApproval(t, h, "prod", &hostv1.HieraLevel{Name: "common", Path: "two.yaml"}, 0, false)

		_, err := applyHieraLevel(h, "a")
		requireSpent(t, err)
		if got := hieraHierarchyTextRaw(t, h, ctx, "prod"); !strings.Contains(got, "two.yaml") || strings.Contains(got, "one.yaml") {
			t.Fatalf("stale replay reverted the level:\n%s", got)
		}
	})

	t.Run("hiera data key", func(t *testing.T) {
		h := newHieraHost(t)
		proposeHieraDataKey(t, h, "a", "prod", "common.yaml", "port", scalarJSON(t, float64(1111)))
		approveOverwrite(t, h, "a")
		if _, err := applyHieraDataKey(h, "a"); err != nil {
			t.Fatalf("Apply a: %v", err)
		}
		requireNotPending(t, putKeyErr(h, "prod", "common.yaml", "port", scalarJSON(t, float64(1111))))
		dataKeyViaApproval(t, h, "prod", "common.yaml", "port", scalarJSON(t, float64(2222)))

		_, err := applyHieraDataKey(h, "a")
		requireSpent(t, err)
		if got, _ := getDoc(t, h, ctx, "code-hiera-data", "prod/common.yaml"); !strings.Contains(got.Body.Value.AsMap()["yaml"].(string), "2222") {
			t.Fatalf("stale replay reverted the key: %v", got.Body.Value.AsMap())
		}
	})
}

// TestCodeOverwriteApprovalProvenance pins WR-03: the gate trusts an approval
// only when the proposal records a decision made under code:approve, not on the
// bare status string.
func TestCodeOverwriteApprovalProvenance(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*host.Host, string) {
		h := newOverwriteHost()
		mustCreateEnv(t, h, "prod")
		mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))
		proposeOverwrite(t, h, "p1", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
		return h, puppetfileTextFromDocumentsRaw(t, h, ctx, "prod")
	}
	setBody := func(t *testing.T, h *host.Host, mutate func(map[string]any)) {
		t.Helper()
		doc, ok := getDoc(t, h, ctx, code.OverwriteCollection, "p1")
		if !ok {
			t.Fatal("proposal missing")
		}
		body := doc.Body.Value.AsMap()
		mutate(body)
		if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
			Collection: code.OverwriteCollection, DocId: "p1", IfVersion: doc.Version,
			Body: &hostv1.Json{Value: mustStruct(t, body)},
		}); err != nil {
			t.Fatalf("Documents.Put: %v", err)
		}
	}

	t.Run("a genuine approval records the verified scope", func(t *testing.T) {
		h, _ := setup(t)
		approveOverwrite(t, h, "p1")
		doc, _ := getDoc(t, h, ctx, code.OverwriteCollection, "p1")
		m := doc.Body.Value.AsMap()
		if m["approved_scope"] != code.OverwriteApproveScope || m["decided_by"] != "operator" {
			t.Fatalf("approval provenance missing from %v", m)
		}
	})

	t.Run("an approval decided under another scope does not authorize an overwrite", func(t *testing.T) {
		h, before := setup(t)
		otherKind := approval.Kind{Collection: code.OverwriteCollection, ApproveScope: "anything:else"}
		tok, err := h.Auth.IssueToken(ctx, &hostv1.IssueTokenRequest{Scope: "anything:else", Label: "someone", TtlSeconds: 300})
		if err != nil {
			t.Fatalf("IssueToken: %v", err)
		}
		if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: otherKind, ProposalID: "p1", TokenSecret: tok.Secret}); err != nil {
			t.Fatalf("Approve with the other kind: %v", err)
		}
		_, err = applyModule(h, "p1")
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), code.OverwriteApproveScope) {
			t.Fatalf("Apply: got %v, want FailedPrecondition naming the required scope", err)
		}
		if err := putModuleErr(h, "prod", forgeModule("puppetlabs/apache", "6.1.0")); !local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("Put: got %v, want requires-approval (the proposal covers nothing)", err)
		}
		if got := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod"); got != before {
			t.Fatalf("a wrong-scope approval wrote: %q", got)
		}
	})

	t.Run("a bare status flip without provenance is refused", func(t *testing.T) {
		h, before := setup(t)
		setBody(t, h, func(m map[string]any) { m["status"] = "approved" })
		_, err := applyModule(h, "p1")
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("Apply: got %v, want FailedPrecondition", err)
		}
		// The Code gate shares approval.RequireApproved's typed refusal rather
		// than building its own, so both gates disagree about nothing.
		if !approval.IsNotApproved(err) {
			t.Fatalf("Apply: got %v, want an approval.IsNotApproved refusal", err)
		}
		if got := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod"); got != before {
			t.Fatalf("a bare approved status wrote: %q", got)
		}
	})

	// This pins an accepted property, like TestApproval_DocumentsHasNoFieldLevelACL
	// for inventory: the Documents facet has no ACL, so any holder of *host.Host
	// can write a complete, well-formed approval record straight into the
	// code-overwrites collection. The gate defends against accidental and
	// unapproved overwrites through the Code RPCs; it is not a boundary against a
	// pack that can also write Documents.
	t.Run("Documents has no ACL, so a full forged approval record is honoured", func(t *testing.T) {
		h, _ := setup(t)
		setBody(t, h, func(m map[string]any) {
			m["status"] = "approved"
			m["approved_scope"] = code.OverwriteApproveScope
			m["decided_by"] = "forged"
		})
		got, err := applyModule(h, "p1")
		if err != nil {
			t.Fatalf("Apply of a forged-but-well-formed approval: %v", err)
		}
		if got.GetForge().GetVersion() != "6.1.0" {
			t.Fatalf("Apply returned %+v", got)
		}
	})
}

// TestCodeOverwriteRemoveThenPutBypassesTheGate documents WR-04: deletes are
// deliberately ungated (D-01), so replacing an item without approval takes two
// calls, a remove and then a put, and the put is an ungated create. This test
// pins that reachable path so the gate is described honestly: it guards against
// accidental in-place replacement through a Put, and is not a control against a
// code:rw holder acting deliberately.
func TestCodeOverwriteRemoveThenPutBypassesTheGate(t *testing.T) {
	ctx := context.Background()

	t.Run("puppetfile module", func(t *testing.T) {
		h := newOverwriteHost()
		mustCreateEnv(t, h, "prod")
		mustPutModule(t, h, "prod", forgeModule("puppetlabs/apache", "0.10.0"))
		replacement := forgeModule("puppetlabs/apache", "6.1.0")
		if err := putModuleErr(h, "prod", replacement); !local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("direct replace: got %v, want requires-approval", err)
		}
		if _, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "prod", Name: "puppetlabs/apache"}); err != nil {
			t.Fatalf("RemovePuppetfileModule: %v", err)
		}
		mustPutModule(t, h, "prod", replacement)
		resp, _ := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
		if len(resp.Modules) != 1 || resp.Modules[0].GetForge().GetVersion() != "6.1.0" {
			t.Fatalf("module not replaced through remove-then-put: %v", resp.Modules)
		}
	})

	t.Run("hiera level", func(t *testing.T) {
		h := newHieraHost(t)
		replacement := &hostv1.HieraLevel{Name: "role", Path: "roles/y.yaml"}
		if err := putLevelErr(h, "prod", replacement, 0, false); !local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("direct replace: got %v, want requires-approval", err)
		}
		if _, err := h.Code.RemoveHieraLevel(ctx, &hostv1.RemoveHieraLevelRequest{Environment: "prod", Name: "role"}); err != nil {
			t.Fatalf("RemoveHieraLevel: %v", err)
		}
		if err := putLevelErr(h, "prod", replacement, 0, true); err != nil {
			t.Fatalf("PutHieraLevel after remove: %v", err)
		}
		if got := hieraHierarchyTextRaw(t, h, ctx, "prod"); !strings.Contains(got, "roles/y.yaml") {
			t.Fatalf("level not replaced through remove-then-put:\n%s", got)
		}
	})

	t.Run("hiera data key", func(t *testing.T) {
		h := newHieraHost(t)
		if err := putKeyErr(h, "prod", "common.yaml", "port", scalarJSON(t, float64(1))); !local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("direct replace: got %v, want requires-approval", err)
		}
		if _, err := h.Code.RemoveHieraDataKey(ctx, &hostv1.RemoveHieraDataKeyRequest{Environment: "prod", Path: "common.yaml", Key: "port"}); err != nil {
			t.Fatalf("RemoveHieraDataKey: %v", err)
		}
		if err := putKeyErr(h, "prod", "common.yaml", "port", scalarJSON(t, float64(1))); err != nil {
			t.Fatalf("PutHieraDataKey after remove: %v", err)
		}
	})
}

// TestCodeOverwriteGateDoesNotParseWholeFiles pins WR-06: deciding whether a
// Put is an overwrite must not depend on every other value or level in the file
// converting cleanly, or a file the gate cannot fully parse becomes impossible to
// add to even though nothing is being replaced.
func TestCodeOverwriteGateDoesNotParseWholeFiles(t *testing.T) {
	ctx := context.Background()

	t.Run("a new data key next to an unrepresentable value stays ungated", func(t *testing.T) {
		h := newHieraHost(t)
		seedDoc(t, h, ctx, "code-hiera-data", "prod/odd.yaml", map[string]any{
			"path": "odd.yaml",
			"yaml": "when: 2020-01-02T03:04:05Z\nports:\n  80: http\nname: web\n",
		})
		// The call's response re-reads the whole file (a separate, pre-existing
		// step) and so may still report that file as unrepresentable. What this
		// pins is that the overwrite gate itself no longer refuses, so the write
		// reaches storage.
		err := putKeyErr(h, "prod", "odd.yaml", "fresh", scalarJSON(t, "x"))
		if local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("adding a new key was refused by the overwrite gate: %v", err)
		}
		if got, _ := getDoc(t, h, ctx, "code-hiera-data", "prod/odd.yaml"); !strings.Contains(got.Body.Value.AsMap()["yaml"].(string), "fresh") {
			t.Fatalf("the new key never reached storage (err = %v)", err)
		}
		if err := putKeyErr(h, "prod", "odd.yaml", "name", scalarJSON(t, "x")); !local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("replacing an existing key in that file: got %v, want requires-approval", err)
		}
	})

	t.Run("a new level beside an unparseable level stays ungated", func(t *testing.T) {
		h := newHieraHost(t)
		cur, _ := getDoc(t, h, ctx, "code-hiera-hierarchy", "prod")
		if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
			Collection: "code-hiera-hierarchy", DocId: "prod", IfVersion: cur.Version,
			Body: &hostv1.Json{Value: mustStruct(t, map[string]any{
				"yaml": "version: 5\nhierarchy:\n  - name: role\n    path: roles/x.yaml\n  - just-a-string\n",
			})},
		}); err != nil {
			t.Fatalf("seeding hierarchy: %v", err)
		}
		// As above: the response re-reads the whole hierarchy and may still
		// report the unparseable level; the gate itself must not refuse.
		err := putLevelErr(h, "prod", &hostv1.HieraLevel{Name: "fresh", Path: "fresh.yaml"}, 0, true)
		if local.IsCodeOverwriteRequiresApproval(err) || local.IsCodeOverwriteApplyPending(err) {
			t.Fatalf("adding a new level was refused by the overwrite gate: %v", err)
		}
		if got := hieraHierarchyTextRaw(t, h, ctx, "prod"); !strings.Contains(got, "fresh.yaml") {
			t.Fatalf("the new level never reached storage (err = %v)", err)
		}
		if err := putLevelErr(h, "prod", &hostv1.HieraLevel{Name: "role", Path: "other.yaml"}, 0, false); !local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("replacing an existing level: got %v, want requires-approval", err)
		}
	})

	t.Run("Apply of a data key idempotence check ignores unrelated unrepresentable values", func(t *testing.T) {
		h := newHieraHost(t)
		seedDoc(t, h, ctx, "code-hiera-data", "prod/odd.yaml", map[string]any{
			"path": "odd.yaml",
			"yaml": "when: 2020-01-02T03:04:05Z\nname: web\n",
		})
		proposeHieraDataKey(t, h, "k1", "prod", "odd.yaml", "name", scalarJSON(t, "api"))
		approveOverwrite(t, h, "k1")
		// The response re-reads the whole file, which cannot be represented, so
		// the call may report that; what matters is that the gate and the write
		// were not blocked before it, so the stored file holds the new value.
		_, _ = applyHieraDataKey(h, "k1")
		got, _ := getDoc(t, h, ctx, "code-hiera-data", "prod/odd.yaml")
		if y := got.Body.Value.AsMap()["yaml"].(string); !strings.Contains(y, "api") || strings.Contains(y, "web") {
			t.Fatalf("Apply did not write the approved value: %q", y)
		}
	})
}
