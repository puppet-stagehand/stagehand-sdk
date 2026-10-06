package manifest

import (
	"os"
	"strings"
	"testing"
)

// This file guards a published reference document, not a manifest. It lives in
// package manifest only because schema/proto/ holds no Go package. It follows the repo's precedent of tests that read a file
// outside the package (validate_test.go reads schema.json;
// examples/opentofu-lite reads ../../manifest/testdata/).
//
// schema/proto/ is a verbatim copy of proto/ maintained by `go generate
// ./schema` (SD-4 resolved by D-01); schema.TestSchemaProtoMatchesProto owns
// byte equality. This test keeps the Forge permission wording pinned in both.

// TestSchemaProtoReferenceForgePermissionIsCurrent pins WR-04: the agent-facing
// tree names the Forge permission that exists, and the permission Phase 12
// deleted never reappears in either proto tree.
func TestSchemaProtoReferenceForgePermissionIsCurrent(t *testing.T) {
	const deleted = "forge:read"
	const wantLine = "// Permission: Search and Resolve require forge:rw; Recommend requires"

	for _, path := range []string{
		"../schema/proto/stagehand/host/v1/host.proto",
		"../proto/stagehand/host/v1/host.proto",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if strings.Contains(body, deleted) {
			t.Errorf("%s mentions %q, a permission Phase 12 removed: schema/proto is what CLAUDE.md, AGENTS.md, README.md and llms.txt point coding agents at, so a stale permission there teaches an author to declare one that pack-check refuses (permission_unknown)", path, deleted)
		}
		if !strings.Contains(body, wantLine) {
			t.Errorf("%s lost the Forge permission line %q: it names the permission the Forge facet requires, so a reword must not drop it (if proto/ changed, run go generate ./schema)", path, wantLine)
		}
	}
}
