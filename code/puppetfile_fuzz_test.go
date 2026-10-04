package code

import (
	"testing"

	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// FuzzRenderParseRoundTrip states one property: anything RenderPuppetfile
// accepts must strict-parse back to the identical moduledir and the identical
// modules. It is the fuzzed form of the checkRenderRoundTrip invariant (SD-3):
// the self-check enforces the property per call, this target searches for
// inputs where it would otherwise be violated.
//
// The property is one-directional on purpose. A render error is the safe
// outcome (nothing is stored), so the body only requires that a rejection
// returns no text, then stops. Only what RenderPuppetfile accepts is checked.
//
// Every payload reproduced by the NEW-2 audit (12.1-RESEARCH.md Finding 1) is a
// seed, so plain `go test` runs each as a sub-test with no -fuzz flag.
func FuzzRenderParseRoundTrip(f *testing.F) {
	// Seed tuple: forgeName, forgeVersion, gitName, gitURL, refValue,
	// moduledir, refSel. Every f.Add must carry all seven arguments.

	// All-valid and all-empty.
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "git@git.example.com:puppet/profiles.git", "main", "thirdparty", uint8(5))
	f.Add("", "", "", "", "", "", uint8(0))

	// Hostile Git module name: newline then a second mod statement.
	f.Add("puppetlabs/stdlib", "9.4.1", "zz'\nmod 'puppetlabs-stdlib', '99.0.0'\nmod 'qq", "https://example.com/a.git", "main", "", uint8(0))
	// Hostile Forge version: newline then Ruby code.
	f.Add("puppetlabs/stdlib", "1.0'\nsystem('id')\n#", "profiles", "https://example.com/a.git", "main", "", uint8(0))
	// Backslash-terminated Forge version.
	f.Add("puppetlabs/stdlib", "1.0\\", "profiles", "https://example.com/a.git", "main", "", uint8(0))
	// Hostile ref value for each ref selector arm that carries a value.
	hostile := "x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#"
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git", hostile, "", uint8(1))
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git", hostile, "", uint8(2))
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git", hostile, "", uint8(3))
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git", hostile, "", uint8(4))
	// Hostile default_branch (refSel 0 leaves RefKind nil, so only DefaultBranch carries it).
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git", hostile, "", uint8(0))
	// Hostile default_branch alongside control_branch.
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git", hostile, "", uint8(5))
	// Hostile moduledir.
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git", "main", hostile, uint8(0))
	// Expression-breakout Git URL.
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://a/'+`id`+'", "main", "", uint8(0))
	// Control-character Git URL.
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://a/\v\fb\x00", "main", "", uint8(0))
	// U+2028 LINE SEPARATOR in the Git URL.
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://a/ b", "main", "", uint8(0))
	// Trailing-newline Forge name.
	f.Add("puppetlabs/ntp\n", "9.4.1", "profiles", "https://example.com/a.git", "main", "", uint8(0))
	// Hash-rocket text inside values: scalarSafe, but confuses the strict reader.
	f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git?k=:x=>y", ":x=>y", "", uint8(1))

	f.Fuzz(func(t *testing.T, forgeName, forgeVersion, gitName, gitURL, refValue, moduledir string, refSel uint8) {
		git := &hostv1.GitSource{Url: gitURL, DefaultBranch: refValue}
		switch refSel % 6 {
		case 0:
			// RefKind stays nil.
		case 1:
			git.RefKind = &hostv1.GitSource_Ref{Ref: refValue}
		case 2:
			git.RefKind = &hostv1.GitSource_Tag{Tag: refValue}
		case 3:
			git.RefKind = &hostv1.GitSource_Branch{Branch: refValue}
		case 4:
			git.RefKind = &hostv1.GitSource_Commit{Commit: refValue}
		case 5:
			git.RefKind = &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}
		}

		pf := &hostv1.Puppetfile{
			Moduledir: moduledir,
			Modules: []*hostv1.PuppetfileModule{
				{Name: forgeName, Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: forgeVersion}}},
				{Name: gitName, Source: &hostv1.PuppetfileModule_Git{Git: git}},
			},
		}

		out, err := RenderPuppetfile(pf)
		if err != nil {
			// A rejection is the safe outcome, but it must never hand back text.
			if len(out) != 0 {
				t.Fatalf("RenderPuppetfile returned an error and a partial render %q (err %v)", out, err)
			}
			return
		}

		back, err := ParsePuppetfile(out)
		if err != nil {
			t.Fatalf("rendered text %q does not strict-parse: %v\ninputs: forgeName=%q forgeVersion=%q gitName=%q gitURL=%q refValue=%q moduledir=%q refSel=%d",
				out, err, forgeName, forgeVersion, gitName, gitURL, refValue, moduledir, refSel)
		}
		if back.GetModuledir() != pf.GetModuledir() {
			t.Fatalf("moduledir %q parsed back as %q; rendered text %q", pf.GetModuledir(), back.GetModuledir(), out)
		}
		if len(back.GetModules()) != len(pf.GetModules()) {
			t.Fatalf("model has %d modules, rendered text parses to %d; rendered text %q", len(pf.GetModules()), len(back.GetModules()), out)
		}
		// Per module, never the whole message, so a future Environment field on
		// the model cannot produce a false failure.
		for i := range pf.GetModules() {
			if !proto.Equal(back.GetModules()[i], pf.GetModules()[i]) {
				t.Fatalf("module %d differs after round trip:\nmodel: %v\nparsed: %v\nrendered text %q", i, pf.GetModules()[i], back.GetModules()[i], out)
			}
		}
	})
}
