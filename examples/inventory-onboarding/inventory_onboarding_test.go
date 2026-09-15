package inventoryonboarding_test

import (
	"context"
	"os"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	inventoryonboarding "github.com/puppet-stagehand/stagehand-sdk/examples/inventory-onboarding"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

// scenarioNodeID and scenarioGroupID are deliberately distinct from
// host.Local's built-in Discover/class defaults (db-01/web-01.example.test,
// group "webservers") so that no assertion in this file can pass by
// accidentally matching a default that may change later.
const (
	scenarioNodeID  = "web-07.example.test"
	scenarioGroupID = "production-web"
)

// newHost loads manifest.json from this example's own directory and builds
// one *host.Host seeded with this package's own Discover candidate and
// group-class fixtures — never host.Local's built-in defaults.
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
	return local.New(m.Permissions, m.ID,
		local.WithDiscoverCandidates(
			&hostv1.Node{Id: scenarioNodeID, DisplayName: "web-07", Status: hostv1.Node_DISCOVERED, Environment: "production"},
		),
		local.WithGroupClasses(map[string][]*hostv1.Class{
			scenarioGroupID: {
				{Name: "profile::base"},
				{Name: "role::web_server"},
			},
		}),
	)
}

// approverToken mints an approval.ScopeApprove-scoped token and returns
// only its secret — standing in for an operator obtaining a token out of
// band. This helper belongs in the test file and nowhere else: it is the
// entire structural claim of this example that no function in
// inventory_onboarding.go can reach it.
func approverToken(t *testing.T, h *host.Host, label string) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{
		Scope:      approval.ScopeApprove,
		Label:      label,
		TtlSeconds: 300,
	})
	if err != nil {
		t.Fatalf("approverToken: IssueToken: %v", err)
	}
	return tok.Secret
}

func TestInventoryOnboarding_EndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newHost(t)

	// --- Proposer persona: discovers, groups, fact-tags, and proposes. ---
	proposer := inventoryonboarding.NewProposer(h)
	approver := inventoryonboarding.NewApprover(h)

	candidates, err := proposer.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected exactly 1 discover candidate, got %d", len(candidates))
	}
	if candidates[0].Id != scenarioNodeID {
		t.Fatalf("expected candidate id %q, got %q", scenarioNodeID, candidates[0].Id)
	}
	if candidates[0].Status != hostv1.Node_DISCOVERED {
		t.Fatalf("expected candidate status DISCOVERED, got %v", candidates[0].Status)
	}

	if err := proposer.AddToGroup(ctx, scenarioNodeID, scenarioGroupID); err != nil {
		t.Fatalf("AddToGroup: %v", err)
	}

	classes, err := proposer.GroupClasses(ctx, scenarioGroupID)
	if err != nil {
		t.Fatalf("GroupClasses: %v", err)
	}
	if len(classes) != 2 || classes[0].Name != "profile::base" || classes[1].Name != "role::web_server" {
		t.Fatalf("expected classes [profile::base role::web_server] in that order, got %+v", classes)
	}

	if _, err := proposer.AttachFacts(ctx, scenarioNodeID, map[string]any{
		"os":        "linux",
		"cpu_count": float64(8),
	}); err != nil {
		t.Fatalf("AttachFacts: %v", err)
	}

	proposal, err := proposer.ProposeOnboarding(ctx, scenarioNodeID)
	if err != nil {
		t.Fatalf("ProposeOnboarding: %v", err)
	}
	if proposal.Status != approval.StatusPending {
		t.Fatalf("expected proposal status %q, got %q", approval.StatusPending, proposal.Status)
	}

	// --- Operator persona: mints a token out of band and decides. ---
	secret := approverToken(t, h, "operator-1")
	approved, err := approver.Approve(ctx, proposal.ID, secret)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != approval.StatusApproved {
		t.Fatalf("expected approved status %q, got %q", approval.StatusApproved, approved.Status)
	}
	if approved.DecidedBy != "operator-1" {
		t.Fatalf("expected DecidedBy %q, got %q", "operator-1", approved.DecidedBy)
	}

	// Onboard is called immediately after Approve's error is checked, with
	// no intervening read of the proposal.
	onboarded, err := proposer.Onboard(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if onboarded.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected onboarded node status ONBOARDED, got %v", onboarded.Status)
	}

	found, err := proposer.FindByFact(ctx, "os", "linux")
	if err != nil {
		t.Fatalf("FindByFact: %v", err)
	}
	if len(found) != 1 || found[0].Id != scenarioNodeID {
		t.Fatalf("expected exactly node %q from FindByFact, got %+v", scenarioNodeID, found)
	}
	if found[0].Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected found node status ONBOARDED, got %v", found[0].Status)
	}

	groups, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: scenarioNodeID})
	if err != nil {
		t.Fatalf("ListNodeGroups: %v", err)
	}
	if len(groups.Groups) != 1 || groups.Groups[0].Id != scenarioGroupID {
		t.Fatalf("expected node still in group %q after onboarding, got %+v", scenarioGroupID, groups.Groups)
	}
}
