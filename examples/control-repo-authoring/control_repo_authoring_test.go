package controlrepoauthoring_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	controlrepoauthoring "github.com/puppet-stagehand/stagehand-sdk/examples/control-repo-authoring"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

// newHost loads manifest.json from this example's own directory, refuses any
// parse or validation finding, and builds one *host.Host scoped to exactly the
// permissions that manifest declares. The host's grant therefore comes from
// the manifest and nowhere else.
func newHost(t *testing.T) *host.Host {
	t.Helper()
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, findings := manifest.Parse(raw)
	if len(findings) > 0 {
		t.Fatalf("manifest.json failed to parse: %v", findings)
	}
	if findings := manifest.Validate(m); len(findings) > 0 {
		t.Fatalf("manifest.json failed validation: %v", findings)
	}
	return local.New(m.Permissions, m.ID)
}

// approverToken mints a CodeKind.ApproveScope-scoped token and returns only
// its secret — standing in for an operator obtaining a token out of band.
// Token minting lives in this test file and nowhere else: it is the entire
// structural claim of this example that no function in
// control_repo_authoring.go can reach it.
func approverToken(t *testing.T, h *host.Host, label string) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{
		Scope:      controlrepoauthoring.CodeKind.ApproveScope,
		Label:      label,
		TtlSeconds: 300,
	})
	if err != nil {
		t.Fatalf("approverToken: IssueToken: %v", err)
	}
	return tok.Secret
}

// ptr returns a pointer to v, for the optional proto3 fields of
// EnvironmentSettings.
func ptr[T any](v T) *T { return &v }

// TestControlRepoAuthoring_EndToEnd walks one blank host through the whole
// slice: create environments, author a module, then author settings through
// the approval gate, and read every byte back. Run with -v to read it as a
// transcript.
func TestControlRepoAuthoring_EndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newHost(t)
	proposer := controlrepoauthoring.NewProposer(h)
	approver := controlrepoauthoring.NewApprover(h)

	// Step 1: two blank environments.
	for _, name := range []string{"authored", "canary"} {
		env, err := proposer.CreateEnvironment(ctx, name)
		if err != nil {
			t.Fatalf("CreateEnvironment(%q): %v", name, err)
		}
		if env.Name != name {
			t.Fatalf("CreateEnvironment: expected name %q, got %q", name, env.Name)
		}
		t.Logf("step 1: created blank environment %q", env.Name)
	}

	// Step 2: author one Puppetfile module (an ungated, additive write).
	mod, err := proposer.AddModule(ctx, "authored", &hostv1.PuppetfileModule{
		Name:   "puppetlabs/ntp",
		Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "13.2.1"}},
	})
	if err != nil {
		t.Fatalf("AddModule: %v", err)
	}
	if mod.Name != "puppetlabs/ntp" || mod.GetForge().GetVersion() != "13.2.1" {
		t.Fatalf("AddModule: expected puppetlabs/ntp at 13.2.1, got %+v", mod)
	}
	t.Logf("step 2: added module %s at %s to authored", mod.Name, mod.GetForge().GetVersion())

	// Step 3: propose the environment settings through the gate.
	const proposalID = "settings-authored-1"
	proposal, err := proposer.ProposeSettings(ctx, proposalID, &hostv1.EnvironmentSettings{
		Environment:        "authored",
		ConfigVersion:      ptr("scripts/config_version.sh"),
		EnvironmentTimeout: ptr("5m"),
	})
	if err != nil {
		t.Fatalf("ProposeSettings: %v", err)
	}
	if proposal.Status != approval.StatusPending {
		t.Fatalf("ProposeSettings: expected status %q, got %q", approval.StatusPending, proposal.Status)
	}
	t.Logf("step 3: proposed settings for authored as %q (status %s)", proposalID, proposal.Status)

	// Step 4: a second persona, holding a token the proposer never saw, approves.
	secret := approverToken(t, h, "operator-ada")
	approved, err := approver.Approve(ctx, proposalID, secret)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != approval.StatusApproved {
		t.Fatalf("Approve: expected status %q, got %q", approval.StatusApproved, approved.Status)
	}
	if approved.DecidedBy != "operator-ada" {
		t.Fatalf("Approve: expected DecidedBy %q, got %q", "operator-ada", approved.DecidedBy)
	}
	t.Logf("step 4: %s approved %q", approved.DecidedBy, proposalID)

	// Step 5: apply immediately after Approve, with no intervening read.
	applied, err := proposer.ApplySettings(ctx, proposalID)
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	assertAuthoredSettings(t, "ApplySettings", applied)
	t.Logf("step 5: applied settings: config_version=%s environment_timeout=%s", applied.GetConfigVersion(), applied.GetEnvironmentTimeout())

	// Step 6: read everything back.
	got, err := proposer.Settings(ctx, "authored")
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	assertAuthoredSettings(t, "Settings", got)

	text, err := proposer.RenderPuppetfile(ctx, "authored")
	if err != nil {
		t.Fatalf("RenderPuppetfile: %v", err)
	}
	if want := "mod 'puppetlabs/ntp', '13.2.1'\n"; text != want {
		t.Fatalf("RenderPuppetfile: expected %q, got %q", want, text)
	}

	envs, err := proposer.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 2 || envs[0].Name != "authored" || envs[1].Name != "canary" {
		t.Fatalf("ListEnvironments: expected [authored canary], got %+v", envs)
	}
	t.Logf("step 6: read back settings, Puppetfile %q and environments [%s %s]", text, envs[0].Name, envs[1].Name)
}

// assertAuthoredSettings checks the two written fields and that the five
// unwritten ones are still unset — presence, not zero value: a setting nobody
// wrote stays absent, it does not become a Puppet default.
func assertAuthoredSettings(t *testing.T, label string, s *hostv1.EnvironmentSettings) {
	t.Helper()
	if s.GetEnvironment() != "authored" {
		t.Fatalf("%s: expected environment %q, got %q", label, "authored", s.GetEnvironment())
	}
	if s.ConfigVersion == nil || *s.ConfigVersion != "scripts/config_version.sh" {
		t.Fatalf("%s: expected ConfigVersion %q, got %v", label, "scripts/config_version.sh", s.ConfigVersion)
	}
	if s.EnvironmentTimeout == nil || *s.EnvironmentTimeout != "5m" {
		t.Fatalf("%s: expected EnvironmentTimeout %q, got %v", label, "5m", s.EnvironmentTimeout)
	}
	if s.Modulepath != nil || s.Manifest != nil || s.DisablePerEnvironmentManifest != nil || s.StaticCatalogs != nil || s.RichData != nil {
		t.Fatalf("%s: expected Modulepath, Manifest, DisablePerEnvironmentManifest, StaticCatalogs and RichData to stay unset, got %+v", label, s)
	}
}

// TestControlRepoAuthoring_ManifestDeclaresExactly pins the manifest's grant
// by equality, so an added headroom permission, a renamed route or an
// approval scope promoted into permissions fails here.
func TestControlRepoAuthoring_ManifestDeclaresExactly(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, findings := manifest.Parse(raw)
	if len(findings) > 0 {
		t.Fatalf("manifest.json failed to parse: %v", findings)
	}
	if findings := manifest.Validate(m); len(findings) > 0 {
		t.Fatalf("manifest.json failed validation: %v", findings)
	}

	// The registry read permission for the manifest content rule is absent on
	// purpose: no host/local facet checks it, and the only rule that requires
	// it fires inside the Content != nil branch, which this manifest's null
	// content never enters. Do not add it back.
	wantPerms := []string{"code:rw", "code:import", "forge:rw", "forge:recommend", "secrets:rw", "tokens:issue"}
	if !reflect.DeepEqual(m.Permissions, wantPerms) {
		t.Fatalf("permissions: expected exactly %v, got %v", wantPerms, m.Permissions)
	}
	for _, p := range m.Permissions {
		if p == controlrepoauthoring.CodeKind.ApproveScope {
			t.Fatalf("permissions must never contain %q: the approval scope is a per-decision token, not a standing install-time grant", p)
		}
	}

	type wantRoute struct{ opID, scope string }
	want := []wantRoute{
		{"proposeImport", ""},
		{"proposeOverwrite", ""},
		{"approveProposal", "code:approve"},
		{"rejectProposal", "code:approve"},
	}
	if len(m.Routes) != len(want) {
		t.Fatalf("routes: expected %d, got %d: %+v", len(want), len(m.Routes), m.Routes)
	}
	for i, w := range want {
		if m.Routes[i].OperationID != w.opID {
			t.Errorf("route %d: expected operation_id %q, got %q", i, w.opID, m.Routes[i].OperationID)
		}
		if m.Routes[i].Access.Scope != w.scope {
			t.Errorf("route %q: expected access scope %q, got %q", w.opID, w.scope, m.Routes[i].Access.Scope)
		}
	}

	if m.Content != nil {
		t.Errorf("content: expected nil, got %+v", m.Content)
	}
	if m.OpenAPIPath == "" {
		t.Errorf("openapi_path: required because the manifest declares routes")
	}
}

// TestControlRepoAuthoring_CodeKindIsPinned keeps the two literals the README
// and both guides quote from drifting silently: CodeKind is built from the
// code package constants, and this test is what ties it to the strings.
func TestControlRepoAuthoring_CodeKindIsPinned(t *testing.T) {
	want := approval.Kind{Collection: "code-overwrites", ApproveScope: "code:approve"}
	if controlrepoauthoring.CodeKind != want {
		t.Fatalf("CodeKind: expected %+v, got %+v", want, controlrepoauthoring.CodeKind)
	}
}
