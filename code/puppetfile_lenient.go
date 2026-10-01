package code

// The lenient Puppetfile entry point. Like import.go it is part of the pure
// format layer: hostv1, protobuf and the standard library only, never
// approval, host or host/local.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// ParsePuppetfileLenient turns Puppetfile text into a model plus findings
// instead of failing on the first statement the write-side model cannot
// represent (IMP-02). It returns no error: a Puppetfile is never unparseable
// as a whole, because every statement can be skipped, so every finding is a
// warning (D-10).
//
// It is one grammar with ParsePuppetfile, not a second one: it iterates the
// same logicalLines, matches the same regexes and calls the same per-statement
// parse functions, differing only in that each failure becomes a finding and
// the Ruby-1.9 key form is accepted (DQ-5). Every module it returns passes
// ValidateModule and survives RenderPuppetfile followed by strict
// ParsePuppetfile unchanged, which is what lets ApplyImport's second pass be
// infallible (RESEARCH Pitfall 2, T-10-15).
//
// Findings carry the parser's line numbers and an excerpt but no branch or
// file; stampFindings sets those.
func ParsePuppetfileLenient(text string, lim ImportLimits) (*hostv1.Puppetfile, []*hostv1.ImportFinding) {
	lim = lim.withDefaults()
	text = strings.TrimPrefix(text, "\xEF\xBB\xBF")
	pf := &hostv1.Puppetfile{}
	fl := newFindingList(lim)
	warn := func(kind string, line int, excerpt, msg string) {
		fl.add(newFinding(kind, hostv1.ImportFinding_WARNING, line, excerpt, msg, lim))
	}

	for _, ll := range logicalLines(text) {
		if m := reModuledir.FindStringSubmatch(ll.text); m != nil {
			pf.Moduledir = m[1]
			continue
		}
		if m := reMod.FindStringSubmatch(ll.text); m != nil {
			name := m[1]
			remainder := strings.TrimSpace(m[2])
			mod, err := parseModuleRemainder(name, remainder, ll.line, true)
			if err != nil {
				warn(FindingPuppetfileUnsupportedRuby, ll.line, ll.text, "this mod statement uses a form the module model cannot represent; it was skipped")
				continue
			}
			if err := ValidateModule(mod); err != nil {
				warn(FindingPuppetfileInvalidModule, ll.line, ll.text, fmt.Sprintf("the module would not render back to a Puppetfile (%v); it was skipped", err))
				continue
			}
			if !utf8.ValidString(ll.text) || !moduleSurvivesRender(mod) {
				warn(FindingPuppetfileInvalidModule, ll.line, ll.text, "the module does not survive a render and strict re-parse unchanged (an embedded quote or invalid UTF-8); it was skipped")
				continue
			}
			pf.Modules = append(pf.Modules, mod)
			continue
		}
		if isForgeDirective(ll.text) {
			warn(FindingPuppetfileForgeDirective, ll.line, ll.text, "the model has no forge-URL field; the directive was skipped and the rest of the file imported")
			continue
		}
		warn(FindingPuppetfileUnsupportedRuby, ll.line, ll.text, "this line is Ruby the Puppetfile model does not represent; it was skipped")
	}
	return pf, fl.items
}

// isForgeDirective reports whether a logical line is the r10k `forge '...'`
// directive: the word forge followed by whitespace, a quote or a paren.
func isForgeDirective(line string) bool {
	rest, ok := strings.CutPrefix(line, "forge")
	if !ok || rest == "" {
		return false
	}
	switch rest[0] {
	case ' ', '\t', '\'', '"', '(':
		return true
	}
	return false
}

// moduleSurvivesRender reports whether mod, rendered alone and strict-parsed
// back, comes out equal. It is the general guard behind ValidateModule: a
// value the renderer cannot re-quote faithfully (an embedded quote character)
// would otherwise import as one thing and re-read as another.
func moduleSurvivesRender(mod *hostv1.PuppetfileModule) bool {
	txt, err := RenderPuppetfile(&hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{mod}})
	if err != nil {
		return false
	}
	back, err := ParsePuppetfile(txt)
	if err != nil || len(back.GetModules()) != 1 {
		return false
	}
	return proto.Equal(back.GetModules()[0], mod)
}
