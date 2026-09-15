package manifest

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func load(t *testing.T, path string) *Manifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, fs := Parse(raw)
	if len(fs) > 0 {
		t.Fatalf("parse findings: %v", fs)
	}
	return m
}

func codes(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return out
}

func TestHelloExampleIsValid(t *testing.T) {
	m := load(t, "../examples/hello/manifest.json")
	if fs := Validate(m); len(fs) != 0 {
		t.Fatalf("hello must validate; got %v", fs)
	}
}

func TestOpenTofuFixtureIsValid(t *testing.T) {
	m := load(t, "testdata/opentofu.json")
	if fs := Validate(m); len(fs) != 0 {
		t.Fatalf("opentofu fixture must validate; got %v", fs)
	}
}

// TestInventoryOnboardingManifest asserts that Phase 5's shipped example
// fixture (examples/inventory-onboarding/manifest.json) actually uses the
// facet-permission-vs-route-scope split correctly: inventory:rw gates every
// Inventory call for the whole life of the pack, a standing install-time
// grant, while inventory:approve gates one decision and arrives as a
// short-lived token the pack cannot mint for itself. Collapsing the second
// into the permission list would turn a per-decision capability into a
// standing grant — the same self-approval outcome Task 2's static call-graph
// test guards structurally, but arriving through configuration instead of
// through code. TestApprovalScopeIsNotAManifestPermission (above) pins the
// vocabulary itself — that inventory:approve can never be a permission and
// must be a route access.scope; this test's job is narrower: proving this
// example's fixture actually uses that split as shipped.
func TestInventoryOnboardingManifest(t *testing.T) {
	const approvalScope = "inventory:approve"

	m := load(t, "../examples/inventory-onboarding/manifest.json")
	if fs := Validate(m); len(fs) != 0 {
		t.Fatalf("inventory-onboarding fixture must validate; got %v", codes(fs))
	}

	if len(m.Permissions) != 2 {
		t.Fatalf("expected exactly 2 permissions, got %d: %v", len(m.Permissions), m.Permissions)
	}
	if !contains(m.Permissions, "inventory:rw") || !contains(m.Permissions, "tokens:issue") {
		t.Fatalf("expected permissions [inventory:rw tokens:issue], got %v", m.Permissions)
	}
	if contains(m.Permissions, approvalScope) {
		t.Fatalf("permissions must NOT contain %q — a per-decision capability must not become a standing install-time grant", approvalScope)
	}

	var scopedRoutes []Route
	for _, r := range m.Routes {
		if r.Access.Scope == approvalScope {
			scopedRoutes = append(scopedRoutes, r)
		}
	}
	if len(scopedRoutes) != 2 {
		t.Fatalf("expected exactly 2 routes with access.scope %q, got %d: %+v", approvalScope, len(scopedRoutes), m.Routes)
	}
	if scopedRoutes[0].OperationID == scopedRoutes[1].OperationID {
		t.Fatalf("expected the two %q-scoped routes to carry distinct operation_ids, both were %q", approvalScope, scopedRoutes[0].OperationID)
	}

	var proposeRoute *Route
	for i := range m.Routes {
		if m.Routes[i].OperationID == "proposeOnboarding" {
			proposeRoute = &m.Routes[i]
		}
	}
	if proposeRoute == nil {
		t.Fatalf("expected a route with operation_id proposeOnboarding, got %+v", m.Routes)
	}
	if proposeRoute.Access.Scope != "" || proposeRoute.Access.Role != "" {
		t.Fatalf("expected the proposeOnboarding route to carry neither scope nor role — proposing is ungoverned, deciding is governed — got %+v", proposeRoute.Access)
	}

	if m.OpenAPIPath == "" {
		t.Fatalf("expected a non-empty openapi_path since the fixture declares routes")
	}
}

func TestEveryFindingHasAFix(t *testing.T) {
	m := load(t, "testdata/everything-wrong.json")
	fs := Validate(m)
	if len(fs) < 10 {
		t.Fatalf("expected many findings, got %d: %v", len(fs), fs)
	}
	for _, f := range fs {
		if strings.TrimSpace(f.Fix) == "" || strings.TrimSpace(f.Code) == "" || !strings.HasPrefix(f.Path, "/") {
			t.Errorf("finding without code/path/fix: %+v", f)
		}
	}
}

func TestRules(t *testing.T) {
	base := func() *Manifest { return load(t, "../examples/hello/manifest.json") }
	cases := []struct {
		name   string
		mutate func(m *Manifest)
		want   string
	}{
		{"bad id", func(m *Manifest) { m.ID = "Hello-World" }, "id_invalid"},
		{"contract mismatch", func(m *Manifest) { m.ContractVersion = 2 }, "contract_version_unsupported"},
		{"unknown permission", func(m *Manifest) { m.Permissions = append(m.Permissions, "database:rw") }, "permission_unknown"},
		{"kind permission ok", func(m *Manifest) { m.Permissions = append(m.Permissions, "inventory:kind:cluster") }, ""},
		{"page without nav", func(m *Manifest) { m.Slots = append(m.Slots, "page"); m.Nav = nil }, "page_requires_nav"},
		{"nav group invented", func(m *Manifest) {
			m.Slots = append(m.Slots, "page")
			m.Nav = &Nav{Group: "Packs", Label: "Hello", Route: "/x/hello"}
		}, "nav_group_unknown"},
		{"nav route mismatch", func(m *Manifest) {
			m.Slots = append(m.Slots, "page")
			m.Nav = &Nav{Group: "Automation", Label: "Hello", Route: "/x/other"}
		}, "nav_route_invalid"},
		{"slots need digest", func(m *Manifest) { m.UIDigest = "" }, "ui_digest_missing"},
		{"absolute route path", func(m *Manifest) {
			m.Permissions = append(m.Permissions, "tokens:issue")
			m.Routes = []Route{{Method: "GET", Path: "/state", Access: Access{Scope: "state:rw"}, OperationID: "getState"}}
			m.OpenAPIPath = "/stagehand/openapi.json"
		}, "route_path_invalid"},
		{"path traversal route path", func(m *Manifest) {
			m.Permissions = append(m.Permissions, "tokens:issue")
			m.Routes = []Route{{Method: "GET", Path: "../other-pack/secret", Access: Access{Scope: "state:rw"}, OperationID: "getState"}}
			m.OpenAPIPath = "/stagehand/openapi.json"
		}, "route_path_invalid"},
		{"scope without tokens:issue", func(m *Manifest) {
			m.Routes = []Route{{Method: "LOCK", Path: "state/{ws}", Access: Access{Scope: "state:rw"}, OperationID: "lockState"}}
			m.OpenAPIPath = "/stagehand/openapi.json"
		}, "route_scope_requires_tokens_issue"},
		{"routes need openapi", func(m *Manifest) {
			m.Routes = []Route{{Method: "GET", Path: "ping", Access: Access{}, OperationID: "ping"}}
			m.OpenAPIPath = ""
		}, "openapi_path_missing"},
		{"job too frequent", func(m *Manifest) { m.Jobs = []Job{{Name: "tick", Every: "5s"}} }, "job_interval_invalid"},
		{"content needs forge:read", func(m *Manifest) {
			m.Content = &Content{ForgeSlug: "souldo-stagehand_hello", Version: ">=1 <2"}
			m.Permissions = []string{"documents:rw"}
		}, "content_requires_forge_read"},
		{"egress with scheme", func(m *Manifest) { m.Network = &Network{Egress: []string{"https://example.com"}} }, "network_egress_invalid"},
		{"settings oneOf", func(m *Manifest) {
			m.SettingsSchema = map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"oneOf": []any{}}}}
		}, "settings_schema_unsupported_keyword"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base()
			c.mutate(m)
			got := codes(Validate(m))
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected clean, got %v", got)
				}
				return
			}
			for _, g := range got {
				if g == c.want {
					return
				}
			}
			t.Fatalf("want %s in %v", c.want, got)
		})
	}
}

func TestInventoryRWPermissionIsAccepted(t *testing.T) {
	m := load(t, "../examples/hello/manifest.json")
	m.Permissions = append(m.Permissions, "inventory:rw")
	if fs := Validate(m); len(fs) != 0 {
		t.Fatalf("inventory:rw must validate clean; got %v", fs)
	}

	m2 := load(t, "../examples/hello/manifest.json")
	m2.Permissions = append(m2.Permissions, "database:rw")
	fs := Validate(m2)
	var found *Finding
	for i := range fs {
		if fs[i].Code == "permission_unknown" {
			found = &fs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a permission_unknown finding, got %v", codes(fs))
	}
	if !strings.Contains(found.Fix, "inventory:rw") {
		t.Fatalf("permission_unknown fix line must offer inventory:rw; got %q", found.Fix)
	}
}

// TestApprovalScopeIsNotAManifestPermission pins the orthogonality ONB-05
// shipped in Phase 3: a facet permission is granted once at install and
// held for the pack's whole lifetime, while the "inventory:approve" scope
// has to be presented per decision by whoever holds the token. Phase 5
// ships a manifest fixture carrying both strings, and the fastest way to
// make that fixture validate is to widen rePerm by one alternative — which
// would collapse the two grants into one and let a pack that can propose
// an onboarding hold approval authority permanently. This test fails
// loudly if that widening ever happens, and proves the string's actual
// home is a route's access.scope, which already accepts it unchanged.
func TestApprovalScopeIsNotAManifestPermission(t *testing.T) {
	const approvalScope = "inventory:approve"

	// Rejection: the scope must not be usable as a manifest permission.
	m := load(t, "../examples/hello/manifest.json")
	m.Permissions = append(m.Permissions, approvalScope)
	fs := Validate(m)
	var found *Finding
	for i := range fs {
		if fs[i].Code == "permission_unknown" {
			found = &fs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a permission_unknown finding for %q in permissions, got %v", approvalScope, codes(fs))
	}

	// Three independent vocabulary checks — the three lists share no
	// common source of truth, so each must be asserted on its own.
	if rePerm.MatchString(approvalScope) {
		t.Errorf("rePerm must NOT match %q — it is an Auth scope, not a facet permission", approvalScope)
	}
	for _, p := range Permissions {
		if p == approvalScope {
			t.Errorf("Permissions slice must NOT contain %q — it is an Auth scope, not a facet permission", approvalScope)
		}
	}
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), approvalScope) {
		t.Errorf("schema.json must NOT contain %q anywhere in its permission pattern", approvalScope)
	}

	// Acceptance: the scope belongs on a route's access.scope, alongside
	// tokens:issue — this is the manifest shape Phase 5's example needs,
	// and it needs no schema change.
	m2 := load(t, "../examples/hello/manifest.json")
	m2.Permissions = append(m2.Permissions, "tokens:issue")
	m2.Routes = []Route{{Method: "POST", Path: "proposals/{id}/decide", Access: Access{Scope: approvalScope}, OperationID: "decideProposal"}}
	m2.OpenAPIPath = "/stagehand/openapi.json"
	fs2 := Validate(m2)
	for _, f := range fs2 {
		if f.Code == "permission_unknown" {
			t.Errorf("route access.scope %q must not trigger permission_unknown; got %v", approvalScope, codes(fs2))
		}
		if f.Code == "route_scope_requires_tokens_issue" {
			t.Errorf("route access.scope %q with tokens:issue present must not trigger route_scope_requires_tokens_issue; got %v", approvalScope, codes(fs2))
		}
	}

	// Proof the cross-check above is live: without tokens:issue, the same
	// route must trip route_scope_requires_tokens_issue. Without this, the
	// clean case above could be passing only because the validator never
	// looked at the route at all.
	m3 := load(t, "../examples/hello/manifest.json")
	m3.Routes = []Route{{Method: "POST", Path: "proposals/{id}/decide", Access: Access{Scope: approvalScope}, OperationID: "decideProposal"}}
	m3.OpenAPIPath = "/stagehand/openapi.json"
	fs3 := Validate(m3)
	var sawCrossCheck bool
	for _, f := range fs3 {
		if f.Code == "route_scope_requires_tokens_issue" {
			sawCrossCheck = true
			break
		}
	}
	if !sawCrossCheck {
		t.Fatalf("expected route_scope_requires_tokens_issue without tokens:issue in permissions, got %v", codes(fs3))
	}
}

func TestPermissionVocabulariesAgree(t *testing.T) {
	for _, p := range Permissions {
		if !rePerm.MatchString(p) {
			t.Errorf("Permissions entry %q does not match rePerm — a suggestion the fix line offers would itself be rejected", p)
		}
	}

	if !rePerm.MatchString("inventory:rw") {
		t.Error("rePerm must match inventory:rw")
	}
	if rePerm.MatchString("inventory:write") {
		t.Error("rePerm must NOT match inventory:write — the new branch must be an exact literal, not a widened prefix match")
	}

	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	props, _ := schema["properties"].(map[string]any)
	permissions, _ := props["permissions"].(map[string]any)
	items, _ := permissions["items"].(map[string]any)
	pattern, _ := items["pattern"].(string)
	if pattern != rePerm.String() {
		t.Fatalf("schema.json permissions pattern must equal rePerm.String() exactly\nschema.json: %s\nrePerm:      %s", pattern, rePerm.String())
	}
}

func TestUnknownFieldIsAFinding(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"id": "x", "sneaky": true})
	if _, fs := Parse(raw); len(fs) == 0 || fs[0].Code != "manifest_unparseable" {
		t.Fatalf("unknown fields must be rejected; got %v", fs)
	}
}
