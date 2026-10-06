package code

import (
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// reQuotedSpan matches one single-quoted span of rendered Puppetfile text.
// Every value RenderPuppetfile interpolates sits inside one, and a safe value
// never contains a single quote, so a span never swallows a second one.
var reQuotedSpan = regexp.MustCompile(`'[^']*'`)

// assertRenderedShape is the fuzz target's independent oracle (WR-03). It
// reads the rendered text without calling ParsePuppetfile, RenderPuppetfile or
// checkRenderRoundTrip, so it can still fail if the implementation's own
// self-check were weakened or both copies shared a bug. It asserts that:
//   - the number of lines beginning "mod " equals the number of modules, so no
//     value forged an extra statement;
//   - at most one "moduledir" line exists, and exactly the expected number;
//   - once every single-quoted span is removed, each line is nothing but
//     the fixed skeleton tokens, so no input byte appears outside a quoted span
//     and no line carries a stray statement.
func assertRenderedShape(t *testing.T, out string, wantModules int, wantModuledir bool) {
	t.Helper()
	// Skeleton tokens that may remain on a line once quoted spans, commas and
	// spaces are removed. A mod opener, a version-pinned or :latest Forge line,
	// moduledir, and the six Git attribute keys with the optional bare
	// control-branch symbol.
	allowed := map[string]bool{
		"": true, "mod": true, "mod:latest": true, "moduledir": true,
		":git=>": true, ":ref=>": true, ":tag=>": true, ":branch=>": true,
		":commit=>": true, ":default_branch=>": true, ":branch=>:control_branch": true,
	}
	mods, moduledirs := 0, 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "mod ") {
			mods++
		}
		if strings.HasPrefix(line, "moduledir ") {
			moduledirs++
		}
		skeleton := reQuotedSpan.ReplaceAllString(line, "")
		skeleton = strings.NewReplacer(" ", "", ",", "").Replace(skeleton)
		if !allowed[skeleton] {
			t.Fatalf("rendered line %q has text outside a quoted span (skeleton %q); rendered text %q", line, skeleton, out)
		}
		if strings.Count(line, "'")%2 != 0 {
			t.Fatalf("rendered line %q has an unbalanced single quote; rendered text %q", line, out)
		}
	}
	if mods != wantModules {
		t.Fatalf("rendered text has %d mod statements, the model has %d modules; rendered text %q", mods, wantModules, out)
	}
	wantMD := 0
	if wantModuledir {
		wantMD = 1
	}
	if moduledirs != wantMD {
		t.Fatalf("rendered text has %d moduledir lines, want %d; rendered text %q", moduledirs, wantMD, out)
	}
}

// FuzzRenderParseRoundTrip states two properties about anything
// RenderPuppetfile accepts. First, an oracle independent of the implementation
// (assertRenderedShape) finds the expected statement count and no text outside
// a quoted span. Second, the text strict-parses back to the identical moduledir
// and the identical modules, the fuzzed form of the checkRenderRoundTrip
// invariant (SD-3). The second property alone cannot fail, because RenderPuppetfile
// already ends with that very comparison, which is why the first exists.
//
// The high two bits of the last seed argument pick which modules are present
// (both, Forge only, Git only), so a value that is invalid for one arm cannot
// short-circuit the whole input before the other arm is exercised.
//
// The property is one-directional on purpose. A render error is the safe
// outcome (nothing is stored), so the body only requires that a rejection
// returns no text, then stops. Only what RenderPuppetfile accepts is checked.
//
// Every payload reproduced by the NEW-2 audit (12.1-RESEARCH.md Finding 1) is a
// seed, so plain `go test` runs each as a sub-test with no -fuzz flag.
func FuzzRenderParseRoundTrip(f *testing.F) {
	// Seed tuple: forgeName, forgeVersion, gitName, gitURL, refValue,
	// moduledir, refSel. Every f.Add must carry all seven arguments. The low
	// bits of refSel (mod 6) pick the ref arm; bits 6 and 7 pick which modules
	// are present: 0 or 3 both, 1 Forge only, 2 Git only.

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
	// Path-shaped moduledir values (FND-02): all must render to an error or
	// to text that strict-parses back unchanged.
	for _, md := range []string{"/srv/modules", "../up", ".", "..", "a/b", "-x", strings.Repeat("a", 65)} {
		f.Add("puppetlabs/stdlib", "9.4.1", "profiles", "https://example.com/a.git", "main", md, uint8(0))
	}
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
	// Single-arm seeds: a hostile value on the Git arm with the Forge module
	// absent, and a hostile Forge version with the Git module absent.
	f.Add("", "", "zz'\nmod 'qq", "https://example.com/a.git", "main", "", uint8(128))
	f.Add("puppetlabs/stdlib", "1.0'\nsystem('id')\n#", "", "", "", "", uint8(64))
	f.Add("puppetlabs/stdlib", "9.4.1", "", "", "", "thirdparty", uint8(64))
	f.Add("", "", "profiles", "https://example.com/a.git", "main", "thirdparty", uint8(130))

	f.Fuzz(func(t *testing.T, forgeName, forgeVersion, gitName, gitURL, refValue, moduledir string, refSel uint8) {
		git := &hostv1.GitSource{Url: gitURL, DefaultBranch: refValue}
		forge := &hostv1.PuppetfileModule{Name: forgeName, Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: forgeVersion}}}
		gitMod := &hostv1.PuppetfileModule{Name: gitName, Source: &hostv1.PuppetfileModule_Git{Git: git}}
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

		pf := &hostv1.Puppetfile{Moduledir: moduledir}
		switch refSel >> 6 {
		case 1:
			pf.Modules = []*hostv1.PuppetfileModule{forge}
		case 2:
			pf.Modules = []*hostv1.PuppetfileModule{gitMod}
		default:
			pf.Modules = []*hostv1.PuppetfileModule{forge, gitMod}
		}

		out, err := RenderPuppetfile(pf)
		if err != nil {
			// A rejection is the safe outcome, but it must never hand back text.
			if len(out) != 0 {
				t.Fatalf("RenderPuppetfile returned an error and a partial render %q (err %v)", out, err)
			}
			return
		}

		assertRenderedShape(t, out, len(pf.GetModules()), pf.GetModuledir() != "")

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
