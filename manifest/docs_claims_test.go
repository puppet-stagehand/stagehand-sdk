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
		{
			path: "../docs/code-overwrite-gating.md",
			mustContain: []string{
				"A Puppetfile poisoned before 12.1 is not refused when it is read",
				"collection_reserved",
			},
			mustNotContain: []string{
				"facet has no access control, so a pack that can call",
			},
			why: "D-14 retired the Documents-has-no-ACL claim; D-13 requires the pre-12.1 read caveat to be stated, not implied away",
		},
		{
			path: "../docs/approval-pattern.md",
			mustContain: []string{
				"approval_transition_requires_token",
				"stagehand-approver-token",
			},
			mustNotContain: []string{
				"Documents store has no ACL, so anything that can reach the host",
				"A store with no ACL can still be",
				"No field-level enforcement on the proposals collection",
			},
			why: "D-14: FND-03 closed INT-4; the transition guard and its token key are the corrected description",
		},
		{
			path: "../docs/code-import.md",
			mustContain: []string{
				"collection_reserved",
			},
			mustNotContain: []string{
				"the Documents facet has no access",
			},
			why: "D-14: Documents refuses a pack's writes to code- collections since Phase 13",
		},
		{
			path: "../docs/control-repo-authoring-testing.md",
			mustContain: []string{
				"A Puppetfile poisoned before 12.1 is not refused when it is read",
				"### Step 20",
				"### Step 23",
			},
			mustNotContain: []string{
				"Two kinds of value are deliberately still allowed",
				"the reference copy was not regenerated from the live one",
			},
			why: "D-13 caveat in Step 17; D-14 and D-01 resolved SD-4 and SD-6; Steps 20-23 are the manual proofs of the Phase 13 controls",
		},
		{
			path: "../docs/control-repo-authoring.md",
			mustContain: []string{
				"Why the module folder must be one plain name",
				"Why some notes are locked",
			},
			mustNotContain: []string{
				"is still fine: the facet checks only whether the value can be",
			},
			why: "FND-02 and FND-03: the ELI10 guide explains both new refusals and must not keep the superseded SD-6 claim that a path outside the environment is fine",
		},
		{
			path: "../approval/require.go",
			mustContain: []string{
				"FND-03",
			},
			mustNotContain: []string{
				"because the Documents store has no ACL",
			},
			why: "D-14: the comment on the single read-side definition of approved must not say Documents has no ACL",
		},
		{
			path: "../host/local/inventory.go",
			mustContain: []string{
				"FND-03",
			},
			mustNotContain: []string{
				"the Documents store has no ACL and anything holding the host can write one",
			},
			why: "D-14: the OnboardNode comment must not say Documents has no ACL",
		},
		{
			path:           "../README.md",
			mustNotContain: []string{"has no access control", "has no ACL"},
			why:            "D-14: README.md and llms.txt carry no Documents-ACL claim",
		},
		{
			path:           "../llms.txt",
			mustNotContain: []string{"has no access control", "has no ACL"},
			why:            "D-14: README.md and llms.txt carry no Documents-ACL claim",
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
