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
		{"content needs no forge permission", func(m *Manifest) {
			m.Content = &Content{ForgeSlug: "souldo-stagehand_hello", Version: ">=1 <2"}
			m.Permissions = []string{"documents:rw"}
		}, ""},
		{"removed forge:read is an unknown permission", func(m *Manifest) {
			m.Permissions = append(m.Permissions, "forge:read")
		}, "permission_unknown"},
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

func TestCodeRWPermissionIsAccepted(t *testing.T) {
	m := load(t, "../examples/hello/manifest.json")
	m.Permissions = append(m.Permissions, "code:rw")
	if fs := Validate(m); len(fs) != 0 {
		t.Fatalf("code:rw must validate clean; got %v", fs)
	}

	m2 := load(t, "../examples/hello/manifest.json")
	m2.Permissions = append(m2.Permissions, "code:write")
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
	if !strings.Contains(found.Fix, "code:rw") {
		t.Fatalf("permission_unknown fix line must offer code:rw; got %q", found.Fix)
	}
}

// TestCodeRWIsAnExactLiteralNotAPrefix fails if the two code: branches are
// ever "simplified" into one prefix pattern — the widening that would hand
// a pack permissions it never declared (T-06-10).
func TestCodeRWIsAnExactLiteralNotAPrefix(t *testing.T) {
	for _, p := range []string{"code:rw", "code:read"} {
		if !rePerm.MatchString(p) {
			t.Errorf("rePerm must match %q", p)
		}
	}
	for _, p := range []string{"code:write", "code:readwrite", "code:rw:all", "code:"} {
		if rePerm.MatchString(p) {
			t.Errorf("rePerm must NOT match %q — code:rw must be an exact literal, not a widened prefix", p)
		}
	}
}

// TestCodeReadSurvivesTheCodeRWAddition pins T-06-13: code:read names a
// different, dormant service (a read-only file browser) than this
// milestone's write-capable Code facet, so it must be added alongside,
// never in place of, the existing permission.
func TestCodeReadSurvivesTheCodeRWAddition(t *testing.T) {
	var found bool
	for _, p := range Permissions {
		if p == "code:read" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Permissions slice must still contain code:read")
	}
	if !rePerm.MatchString("code:read") {
		t.Error("rePerm must still match code:read")
	}
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "code:read") {
		t.Error("schema.json must still contain the literal code:read")
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

// TestCodeApproveScopeIsNotAManifestPermission pins the same orthogonality
// for the Code facet's overwrite gate (GOV-02) that the test above pins for
// Inventory onboarding. A facet permission ("code:rw") is a standing
// install-time grant held for the pack's whole lifetime; the "code:approve"
// scope is a short-lived per-decision token the pack cannot mint for itself.
// A fixture that carries both strings validates the moment someone widens
// rePerm by one alternative, and that is the fastest wrong way to make it
// pass: it would let a pack that can propose an overwrite hold the authority
// to approve it permanently. code:rw is the wrong precedent to copy here;
// inventory:approve is the right one. This test fails loudly if any of the
// three independently maintained vocabularies ever admits the scope, and it
// carries a positive control so it cannot pass just because an alternation
// was deleted.
//
// The scope is declared locally on purpose. This test pins the literal string
// the manifest surfaces must reject, whatever any Go package renames.
func TestCodeApproveScopeIsNotAManifestPermission(t *testing.T) {
	const codeApproveScope = "code:approve"

	schemaBytes, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("rejected as a manifest permission", func(t *testing.T) {
		m := load(t, "../examples/hello/manifest.json")
		m.Permissions = append(m.Permissions, codeApproveScope)
		fs := Validate(m)
		var found bool
		for _, f := range fs {
			if f.Code == "permission_unknown" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected a permission_unknown finding for %q in permissions, got %v", codeApproveScope, codes(fs))
		}
	})

	t.Run("absent from all three vocabularies", func(t *testing.T) {
		if rePerm.MatchString(codeApproveScope) {
			t.Errorf("rePerm must NOT match %q: it is an approval scope, not a facet permission", codeApproveScope)
		}
		for _, p := range Permissions {
			if p == codeApproveScope {
				t.Errorf("Permissions slice must NOT contain %q: it is an approval scope, not a facet permission", codeApproveScope)
			}
		}
		if strings.Contains(string(schemaBytes), codeApproveScope) {
			t.Errorf("schema.json must NOT contain %q anywhere in its permission pattern", codeApproveScope)
		}
	})

	t.Run("accepted as a route access.scope with no schema change", func(t *testing.T) {
		m := load(t, "../examples/hello/manifest.json")
		m.Permissions = append(m.Permissions, "tokens:issue")
		m.Routes = []Route{{Method: "POST", Path: "overwrites/{id}/decide", Access: Access{Scope: codeApproveScope}, OperationID: "decideOverwrite"}}
		m.OpenAPIPath = "/stagehand/openapi.json"
		fs := Validate(m)
		for _, f := range fs {
			switch f.Code {
			case "permission_unknown", "route_scope_requires_tokens_issue":
				t.Errorf("route access.scope %q must not trigger %s; got %v", codeApproveScope, f.Code, codes(fs))
			}
			if strings.Contains(f.Code, "scope") {
				t.Errorf("route access.scope %q must produce no scope finding; got %v", codeApproveScope, codes(fs))
			}
		}
	})

	t.Run("scope route without tokens:issue trips the cross-check", func(t *testing.T) {
		// Proof the clean case above is live: the same route without
		// tokens:issue must be flagged, so the validator demonstrably looked at it.
		m := load(t, "../examples/hello/manifest.json")
		m.Routes = []Route{{Method: "POST", Path: "overwrites/{id}/decide", Access: Access{Scope: codeApproveScope}, OperationID: "decideOverwrite"}}
		m.OpenAPIPath = "/stagehand/openapi.json"
		fs := Validate(m)
		var saw bool
		for _, f := range fs {
			if f.Code == "route_scope_requires_tokens_issue" {
				saw = true
				break
			}
		}
		if !saw {
			t.Fatalf("expected route_scope_requires_tokens_issue without tokens:issue in permissions, got %v", codes(fs))
		}
	})

	t.Run("positive control: code:read and code:rw still real permissions", func(t *testing.T) {
		for _, real := range []string{"code:read", "code:rw"} {
			if !rePerm.MatchString(real) {
				t.Errorf("rePerm must still match %q", real)
			}
			var inSlice bool
			for _, p := range Permissions {
				if p == real {
					inSlice = true
					break
				}
			}
			if !inSlice {
				t.Errorf("Permissions slice must still contain %q", real)
			}
			if !strings.Contains(string(schemaBytes), real) {
				t.Errorf("schema.json must still contain the literal %q", real)
			}

			m := load(t, "../examples/hello/manifest.json")
			m.Permissions = append(m.Permissions, real)
			for _, f := range Validate(m) {
				if f.Code == "permission_unknown" {
					t.Errorf("permission %q must validate; got permission_unknown", real)
				}
			}
		}
	})
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

// TestForgePermissionsAreAcceptedExactly pins that both Forge permissions a
// real Recommend consumer declares validate clean, and that a widened spelling
// of either is refused with a fix line that offers a real Forge literal.
func TestForgePermissionsAreAcceptedExactly(t *testing.T) {
	realForge := []string{"forge:rw", "forge:recommend"}
	for _, real := range []string{"forge:rw", "forge:recommend"} {
		m := load(t, "../examples/hello/manifest.json")
		m.Permissions = append(m.Permissions, real)
		if fs := Validate(m); len(fs) != 0 {
			t.Fatalf("%s must validate clean; got %v", real, codes(fs))
		}
	}

	for _, widened := range []string{"forge:read", "forge:write", "forge:readwrite", "forge:rw:all", "forge:recommend:all", "forge:rec", "forge:recommends", "forge:"} {
		m := load(t, "../examples/hello/manifest.json")
		m.Permissions = append(m.Permissions, widened)
		fs := Validate(m)
		var found *Finding
		for i := range fs {
			if fs[i].Code == "permission_unknown" {
				found = &fs[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("expected a permission_unknown finding for %q, got %v", widened, codes(fs))
		}
		var offers bool
		for _, real := range realForge {
			if strings.Contains(found.Fix, real) {
				offers = true
				break
			}
		}
		if !offers {
			t.Fatalf("permission_unknown fix line for %q must offer a real Forge literal; got %q", widened, found.Fix)
		}
	}
}

// TestForgePermissionVocabulariesCarryBothLiterals pins forge:rw and
// forge:recommend as exact literals on all three independently maintained
// vocabularies, and pins the ABSENCE of forge:read. forge:read was removed in
// Phase 12 as a dead grant: no host path honoured it, and the only manifest
// rule that asked for it (content_requires_forge_read) advertised a permission
// that did nothing. A manifest that still declares it now gets the generic
// permission_unknown finding. It is the positive-control counterpart to
// TestCodeApproveScopeIsNotAManifestPermission: forge:recommend IS a facet
// permission, where code:approve is not.
//
// The literals are declared locally so the pin holds whatever any Go package
// later renames.
func TestForgePermissionVocabulariesCarryBothLiterals(t *testing.T) {
	const (
		removedForgeRead = "forge:read"
		forgeRW          = "forge:rw"
		forgeRecommend   = "forge:recommend"
	)
	real := []string{forgeRW, forgeRecommend}
	widened := []string{removedForgeRead, "forge:write", "forge:readwrite", "forge:rw:all", "forge:recommend:all", "forge:rec", "forge:recommends", "forge:"}

	t.Run("rePerm is an exact-literal match", func(t *testing.T) {
		for _, p := range real {
			if !rePerm.MatchString(p) {
				t.Errorf("rePerm must match %q", p)
			}
		}
		for _, p := range widened {
			if rePerm.MatchString(p) {
				t.Errorf("rePerm must NOT match %q: the forge branches must be exact literals, not a widened prefix", p)
			}
		}
	})

	t.Run("Permissions slice carries both live literals and not the removed one", func(t *testing.T) {
		for _, want := range real {
			var found bool
			for _, p := range Permissions {
				if p == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Permissions slice must contain %q", want)
			}
		}
		for _, p := range Permissions {
			if p == removedForgeRead {
				t.Errorf("Permissions slice must not contain the removed literal %q", removedForgeRead)
			}
		}
	})

	t.Run("schema.json carries both live literals and not the removed one", func(t *testing.T) {
		raw, err := os.ReadFile("schema.json")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range real {
			if !strings.Contains(string(raw), want) {
				t.Errorf("schema.json must contain the literal %q", want)
			}
		}
		if strings.Contains(string(raw), removedForgeRead+"|") || strings.Contains(string(raw), removedForgeRead+`"`) {
			t.Errorf("schema.json must not contain the removed literal %q", removedForgeRead)
		}
	})

	t.Run("fixture declaring both permissions validates", func(t *testing.T) {
		m := load(t, "testdata/forge-recommend.json")
		if fs := Validate(m); len(fs) != 0 {
			t.Fatalf("forge-recommend fixture must validate; got %v", codes(fs))
		}
	})
}

func TestUnknownFieldIsAFinding(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"id": "x", "sneaky": true})
	if _, fs := Parse(raw); len(fs) == 0 || fs[0].Code != "manifest_unparseable" {
		t.Fatalf("unknown fields must be rejected; got %v", fs)
	}
}

// TestCodeImportPermissionIsAcceptedExactly pins that code:import, declared
// alongside code:rw as the two-permission import gate requires (D-03, DQ-1),
// validates clean through the same Validate entry point pack-check uses.
//
// The literals are declared locally: a test that reads the vocabulary it is
// pinning proves nothing about that vocabulary.
func TestCodeImportPermissionIsAcceptedExactly(t *testing.T) {
	const (
		codeRW     = "code:rw"
		codeImport = "code:import"
	)
	m := load(t, "../examples/hello/manifest.json")
	m.Permissions = append(m.Permissions, codeRW, codeImport)
	if fs := Validate(m); len(fs) != 0 {
		t.Fatalf("code:rw + code:import must validate clean; got %v", codes(fs))
	}
}

// TestCodeImportVocabulariesCarryTheLiteral pins code:import as an exact
// literal on all three independently maintained vocabularies, and pins the
// survival of code:read and code:rw as the positive control that the edit
// added a literal rather than replacing one (the Phase 6 code:read
// precedent). code:import IS a facet permission, where code:approve is not
// (see TestCodeApproveScopeIsNotAManifestPermission).
func TestCodeImportVocabulariesCarryTheLiteral(t *testing.T) {
	const (
		codeRead   = "code:read"
		codeRW     = "code:rw"
		codeImport = "code:import"
	)
	real := []string{codeRead, codeRW, codeImport}

	t.Run("rePerm matches code:import as an exact literal", func(t *testing.T) {
		for _, p := range real {
			if !rePerm.MatchString(p) {
				t.Errorf("rePerm must match %q", p)
			}
		}
	})

	t.Run("Permissions slice carries code:import and the older code literals", func(t *testing.T) {
		for _, want := range real {
			var found bool
			for _, p := range Permissions {
				if p == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Permissions slice must contain %q", want)
			}
		}
	})

	t.Run("schema.json carries code:import and the older code literals", func(t *testing.T) {
		raw, err := os.ReadFile("schema.json")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range real {
			if !strings.Contains(string(raw), want) {
				t.Errorf("schema.json must contain the literal %q", want)
			}
		}
	})

	t.Run("fixture declaring code:rw and code:import validates", func(t *testing.T) {
		m := load(t, "testdata/code-import.json")
		if fs := Validate(m); len(fs) != 0 {
			t.Fatalf("code-import fixture must validate; got %v", codes(fs))
		}
	})
}

// TestCodeImportIsAnExactLiteralNotAPrefix fails if the code:import branch is
// ever widened into a prefix, a character class or a quantifier — the widening
// that would hand a pack a clone-and-bulk-create grant it never declared
// (T-10-05). Every spelling is written out literally; none is built from a
// package value.
func TestCodeImportIsAnExactLiteralNotAPrefix(t *testing.T) {
	widened := []string{
		"code:import:all",
		"code:imports",
		"code:imp",
		"code:",
		"code:IMPORT",
		"code:import ",
	}
	for _, p := range widened {
		t.Run(p, func(t *testing.T) {
			if rePerm.MatchString(p) {
				t.Errorf("rePerm must NOT match %q — code:import must be an exact literal", p)
			}
			m := load(t, "../examples/hello/manifest.json")
			m.Permissions = append(m.Permissions, p)
			fs := Validate(m)
			var found *Finding
			n := 0
			for i := range fs {
				if fs[i].Code == "permission_unknown" {
					n++
					found = &fs[i]
				}
			}
			if n != 1 {
				t.Fatalf("expected exactly one permission_unknown finding for %q, got %v", p, codes(fs))
			}
			// The Fix line is the only place a pack author learns the correct
			// spelling (T-10-36).
			if !strings.Contains(found.Fix, "code:import") {
				t.Errorf("permission_unknown fix for %q must name the real literal code:import; got %q", p, found.Fix)
			}
		})
	}
}

// TestCodeRWAndReadSurviveTheCodeImportAddition is the collateral-damage
// control: adding code:import must not displace code:read or code:rw
// (T-10-35), and must not drag code:approve in with it (T-10-06). It restates,
// from the import side, the property TestCodeApproveScopeIsNotAManifestPermission
// pins; it does not replace that test.
func TestCodeRWAndReadSurviveTheCodeImportAddition(t *testing.T) {
	for _, p := range []string{"code:read", "code:rw"} {
		t.Run(p+" validates alone", func(t *testing.T) {
			m := load(t, "../examples/hello/manifest.json")
			m.Permissions = append(m.Permissions, p)
			if fs := Validate(m); len(fs) != 0 {
				t.Fatalf("%s must still validate clean; got %v", p, codes(fs))
			}
		})
	}

	t.Run("code:approve is still not a permission", func(t *testing.T) {
		const approve = "code:approve"
		if rePerm.MatchString(approve) {
			t.Fatalf("rePerm must NOT match %q — it is a per-decision Auth scope, never an install-time permission", approve)
		}
		m := load(t, "../examples/hello/manifest.json")
		m.Permissions = append(m.Permissions, approve)
		var found bool
		for _, f := range Validate(m) {
			if f.Code == "permission_unknown" {
				found = true
			}
		}
		if !found {
			t.Fatalf("a manifest declaring %q must yield a permission_unknown finding", approve)
		}
	})
}
