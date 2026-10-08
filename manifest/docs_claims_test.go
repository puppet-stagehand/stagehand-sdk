package manifest

import (
	"os"
	"strings"
	"testing"
)

// This file pins what the published docs, the agent-facing proto copies and a
// few load-bearing code comments claim about enforcement (FND-04, D-13, D-14).
// A doc that overstates or understates a control is a security defect in its
// own right: operators and coding agents decide what to defend from it.
//
// Each row names a file (relative to this package), fragments that must appear
// (the corrected anchors) and fragments that must not (retired claims). It
// follows the precedent of schema_proto_reference_test.go: a package manifest
// test that reads files outside the package.
//
// Every anchor is a single unwrapped line in the source, so a reflow of a
// comment or paragraph cannot silently break a row.

type docsClaimRow struct {
	path           string
	mustContain    []string
	mustNotContain []string
	// why names the decision that retired or required the fragments.
	why string
}

func TestDocsClaimsAreCurrent(t *testing.T) {
	protoRow := func(path string) docsClaimRow {
		return docsClaimRow{
			path: path,
			mustContain: []string{
				"Documents refuses pack writes to every code- collection",
				"moduledir must be one safe relative path segment",
				"approval_transition_requires_token",
				"collection_reserved",
			},
			mustNotContain: []string{
				"since Documents has no access control",
			},
			why: "D-14: the Code service comment used to say a Documents-writing caller can bypass the gate; since Phase 13 the Documents facet refuses reserved-collection writes and forged approval transitions",
		}
	}

	rows := []docsClaimRow{
		protoRow("../proto/stagehand/host/v1/host.proto"),
		protoRow("../schema/proto/stagehand/host/v1/host.proto"),
		// gen/go carries the same comments; the generated file is what Go
		// authors read in their editor.
		{
			path: "../gen/go/stagehand/host/v1/host_grpc.pb.go",
			mustContain: []string{
				"Documents refuses pack writes to every code- collection",
				"moduledir must be one safe relative path segment",
				"approval_transition_requires_token",
				"collection_reserved",
			},
			mustNotContain: []string{
				"since Documents has no access control",
			},
			why: "D-14: generated code must carry the corrected contract text (run buf generate, then go generate ./schema)",
		},
	}

	for _, row := range rows {
		raw, err := os.ReadFile(row.path)
		if err != nil {
			t.Errorf("%s: %v", row.path, err)
			continue
		}
		body := string(raw)
		for _, frag := range row.mustContain {
			if !strings.Contains(body, frag) {
				t.Errorf("%s lost the required anchor %q (%s)", row.path, frag, row.why)
			}
		}
		for _, frag := range row.mustNotContain {
			if strings.Contains(body, frag) {
				t.Errorf("%s still contains the retired claim %q (%s)", row.path, frag, row.why)
			}
		}
	}
}
