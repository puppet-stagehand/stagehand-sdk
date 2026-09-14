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
	m := load(t, "../../examples/hello/manifest.json")
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
	base := func() *Manifest { return load(t, "../../examples/hello/manifest.json") }
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

func TestUnknownFieldIsAFinding(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"id": "x", "sneaky": true})
	if _, fs := Parse(raw); len(fs) == 0 || fs[0].Code != "manifest_unparseable" {
		t.Fatalf("unknown fields must be rejected; got %v", fs)
	}
}
