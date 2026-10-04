package manifest

import (
	"os"
	"strings"
	"testing"
)

// This file guards a published reference document, not a manifest. It lives in
// package manifest only because schema/proto/ holds no Go package. It follows
// the repo's precedent of tests that read a file outside the package
// (validate_test.go reads schema.json; examples/opentofu-lite reads
// ../../manifest/testdata/).

// TestSchemaProtoReferenceForgePermissionIsCurrent pins WR-04: the reference
// tree names the Forge permission that exists, points at the live contract,
// and the permission Phase 12 deleted never reappears in either proto tree.
func TestSchemaProtoReferenceForgePermissionIsCurrent(t *testing.T) {
	const deleted = "forge:read"
	const wantLine = "// Permission: forge:rw. Resolves the pack's Content module. The live Forge service is proto/stagehand/host/v1/host.proto."

	ref, err := os.ReadFile("../schema/proto/stagehand/host/v1/host.proto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ref), deleted) {
		t.Errorf("schema/proto/stagehand/host/v1/host.proto mentions %q, a permission Phase 12 removed: that tree is what CLAUDE.md, AGENTS.md, README.md and llms.txt point coding agents at, so a stale permission there teaches an author to declare one that pack-check refuses (permission_unknown)", deleted)
	}
	if !strings.Contains(string(ref), wantLine) {
		t.Errorf("schema/proto/stagehand/host/v1/host.proto lost the Forge permission line %q: it names the permission the Forge facet requires and points at the live contract, so a reword must not drop either", wantLine)
	}

	live, err := os.ReadFile("../proto/stagehand/host/v1/host.proto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(live), deleted) {
		t.Errorf("proto/stagehand/host/v1/host.proto mentions %q: Phase 12 (INT-2) removed that permission, and the wire comments must agree with the manifest vocabularies", deleted)
	}
}
